package functions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// OrphanGraceSeconds 是孤儿恢复的行级宽限（P0.5）：staleAfter =
// timeout_seconds + 本值；与迁移 000014 的判定 SQL、app 层周期恢复共用同一
// 口径。
const OrphanGraceSeconds = 120

// ——客户端调用面词表（P2，迁移 000016 CHECK 约束同源；设计 §4/§5）——
const (
	// TriggerSourceClient 是客户端调用面的调用记录 trigger_source 值
	//（无后缀；http/cron 来源为 "<type>:{trigger_id}"）。
	TriggerSourceClient = "client"

	// 限频窗口词表（client_limit_window CHECK 同源）。
	ClientLimitWindowMinute = "minute"
	ClientLimitWindowHour   = "hour"
	ClientLimitWindowDay    = "day"
)

// IsValidClientLimitWindow 报告窗口值是否在词表内（app 层校验 + DB CHECK 双保险）。
func IsValidClientLimitWindow(w string) bool {
	switch w {
	case ClientLimitWindowMinute, ClientLimitWindowHour, ClientLimitWindowDay:
		return true
	}
	return false
}

// IsTriggerSource 报告 trigger_source 值是否为非 server 面（client / http:{id} /
// cron:{id}）来源——保留分级（Q7）的行分类依据。
func IsTriggerSource(triggerSource string) bool {
	return triggerSource == TriggerSourceClient ||
		strings.HasPrefix(triggerSource, TriggerTypeHTTP+":") ||
		strings.HasPrefix(triggerSource, TriggerTypeCron+":")
}

// ErrInvocationIdempotencyConflict 表示客户端幂等键冲突（P2）：并发同键的
// 第二次预占 INSERT 撞 partial 唯一索引——调用方应回读既有行原样返回。
var ErrInvocationIdempotencyConflict = errors.New("invocation idempotency conflict")

// InvocationListFilter 是调用记录列表的结构化过滤（exact 匹配 + created_at
// 闭区间；零值字段 = 不过滤）。Status 取值 = 下方 InvocationStatus* 常量。
type InvocationListFilter struct {
	Status        string
	CreatedAfter  time.Time
	CreatedBefore time.Time
}

// FunctionRepo 持久化函数与调用记录（bun 静态表适配）。v4：部署/变量面随
// 执行面外包退役（docs/design/functions-v4.md §10），Function 承载单一
// endpoint target（§3）。
type FunctionRepo interface {
	CreateFunction(ctx context.Context, fn *Function) error
	GetFunction(ctx context.Context, projectID, functionID string) (*Function, error)
	ListFunctions(ctx context.Context, projectID string) ([]Function, error)
	UpdateFunction(ctx context.Context, fn *Function) error
	DeleteFunction(ctx context.Context, projectID, functionID string) error

	CreateInvocation(ctx context.Context, e *InvocationRecord) error
	GetInvocation(ctx context.Context, projectID, functionID, invocationID string) (*InvocationRecord, error)
	// ListInvocations 管理面分页列出（created_at DESC + InvocationListFilter
	// 结构化过滤，offset 型分页，返回总数）。
	ListInvocations(ctx context.Context, projectID, functionID string, limit, offset int, f InvocationListFilter) ([]InvocationRecord, int, error)
	UpdateInvocation(ctx context.Context, e *InvocationRecord) error
	// TransitionInvocationStatus 条件更新调用状态（CAS）：仅当当前状态等于
	// from 时置为 to 并刷新 updated_at，返回是否生效。用于 worker 领取闸门
	//（queued→running 防重复投递并发执行）与可重试失败归还（running→queued）。
	TransitionInvocationStatus(ctx context.Context, projectID, functionID, invocationID, from, to string) (bool, error)
	// FailInvocationIfActive 将仍处于未终态（queued/running）的调用标记为
	// failed；completed/failed 不被覆盖（防重复投递回写覆盖终态）。
	FailInvocationIfActive(ctx context.Context, projectID, functionID, invocationID, reason string) error
	// RecoverOrphanInvocationsInProject 将指定项目中停留未终态（queued/running）
	// 且超过 staleAfter 的记录标记为 failed。staleAfter 按行计算：
	// timeout_seconds（NOT NULL，迁移 000029）+ 120s 宽限（两写预占的行内
	// 快照）。limit 是本项目本轮上限，供 app/worker 扣减全局预算。
	RecoverOrphanInvocationsInProject(ctx context.Context, projectID string, limit int) (int64, error)
	// PruneOldInvocationsInProject 清理该项目该函数 server 面来源
	//（trigger_source = ''）超过 keepRecent 条最新之外的终态记录。
	// 条数式保留只作用于 server 面（Q7 保留分级）：client/http/cron 来源行
	// 是限频 DB 降级的窗口内计数依据，条数裁剪会造成少计超发。
	PruneOldInvocationsInProject(ctx context.Context, projectID, functionID string, keepRecent int) error
	// PruneTriggerInvocationsInProject 清理该项目该函数 client/http/cron 来源
	//（Q7 保留分级）超过 olderThan 的终态记录（time-based，48h ≥ 2× 最长
	// 限频窗口 day——保证 DB 降级计数窗口内行不被 prune）。
	PruneTriggerInvocationsInProject(ctx context.Context, projectID, functionID string, olderThan time.Time) error
	// GetInvocationByIdempotencyKey 按 (project, function, user, key) 查既有
	// 客户端调用（幂等冲突回读原样返回；running 返回 running）。未命中 nil。
	GetInvocationByIdempotencyKey(ctx context.Context, projectID, functionID, userID, key string) (*InvocationRecord, error)
	// CountClientInvocations 统计窗口起点以来该用户的 client 来源调用数
	//（限频 DB 降级路径的计数依据；partial 索引支撑）。
	CountClientInvocations(ctx context.Context, projectID, functionID, userID string, since time.Time) (int, error)
}

// 调用状态常量（v4 §7 状态机收敛：building 态随补构建退役——构建外包后
// 无中间构建态；异步链 = queued → worker CAS 领取 → running → 终态）。
const (
	InvocationStatusQueued    = "queued"
	InvocationStatusRunning   = "running"
	InvocationStatusCompleted = "completed"
	InvocationStatusFailed    = "failed"
)

// SecretPrefix 是 endpoint target 签名密钥明文前缀（twfnsec_，与 webhooks
// whsec_ 同构惯例；接收方一眼可辨用途）。
const SecretPrefix = "twfnsec_" // #nosec G101 -- 前缀常量非凭证（密钥本体随机生成）

// FingerprintPrefix 是指纹前缀（区别于明文前缀，管理面唯一可见形态）。
const FingerprintPrefix = "twfnsec_"

// TargetSecretRotationWindow 是 target 签名密钥轮换宽限窗缺省（v2 签名窗口
// 24h；与 webhooks SecretRotationWindow 同构——独立常量避免子系统联动漂移）。
const TargetSecretRotationWindow = 24 * time.Hour

// Fingerprint 由 target 签名密钥明文派生指纹（twfnsec_ + SHA-256 前 8 hex）：
// 管理面比对用，不可逆推明文（与 webhooks Fingerprint 同构）。
func Fingerprint(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return FingerprintPrefix + hex.EncodeToString(sum[:])[:8]
}

// Function 是函数实体（v4 §3 target 模型：执行位置 = endpoint 常驻服务；
// 池策略/运行时/资源规格归 substrate，字段整体退役）。
type Function struct {
	ID             string
	ProjectID      string
	Name           string
	TimeoutSeconds int
	Enabled        bool
	// DeclaredScopes 是函数执行 principal 的平台访问声明（P0 执行身份）：
	// 每项形如 "<resource>:<op>"（如 assets:write），词表校验见
	// management.go ValidateDeclaredScopes；空集 = 无平台访问权限（fail-closed）。
	DeclaredScopes []string
	// ——endpoint target（v4 §3 单一 endpoint kind）——
	// TargetURL 是函数常驻服务的调用终点（https 强制；开发环境可放行 http，
	// app 层按环境裁决）。
	TargetURL string
	// TargetSecretEnc 是签名密钥的 secretbox 密文（enc:v1: 前缀；明文仅在
	// 创建/轮换响应出现一次，此后不落任何日志/响应面——签名方每次调用需
	// 明文计算 HMAC，哈希落库无法签名，实施勘误 2026-10-15）。
	TargetSecretEnc string
	// TargetPreviousSecretEnc 是轮换窗口内的旧钥密文（空 = 无轮换或窗口已过）。
	TargetPreviousSecretEnc string
	// TargetSecretRotatedAt 是最近一次轮换时刻（读取侧据此判定旧钥宽限窗）。
	TargetSecretRotatedAt time.Time
	// TargetSecretFingerprint 是密钥指纹（管理面唯一可见形态）。
	TargetSecretFingerprint string
	// ——客户端调用面策略（P2，迁移 000016；设计 §4/§5）——
	// ClientCallable=false（存量默认）时客户端 InvokeFunction 拒绝（fail-closed）。
	ClientCallable bool
	// ClientAnonymousAllowed 字段保留、一期禁用：管理面遇 true 显式报错，
	// 匿名主体在 use-case 层被 RequireEndUser 拒绝（Q4 拍板：匿名 IP 兜底
	// 限频做好后再开）。
	ClientAnonymousAllowed bool
	// ClientPerUserLimit 是可配窗口内的每用户调用配额（>=1 起；0 = client_callable
	// 不可开启，client_callable=true 要求 limit>=1）。
	ClientPerUserLimit int
	// ClientLimitWindow 是限频窗口粒度（minute|hour|day；day 按 UTC 日期）。
	ClientLimitWindow string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// InvocationRecord 是一次函数调用的记录实体；命名区别于 invoker 入参
// Invocation（v4 §2：ExecutionRecord 改名——torchwood 只发起调用、不执行）。
type InvocationRecord struct {
	ID                string
	FunctionID        string
	ProjectID         string
	Status            string
	Response          string
	ResponseTruncated bool
	StatusCode        int
	DurationMS        int64
	Error             string
	// TimeoutSeconds 是函数超时的行内快照（P0.5 两写预占记账；周期孤儿恢复
	// 的 staleAfter = 本值 + 120s 宽限。迁移 000029 起 NOT NULL——NULL 存量
	// 行已回填平台缺省 15，回退分支退役）。
	TimeoutSeconds int
	// ——触发来源（P1，迁移 000015）——TriggerSource 形如 http:{trigger_id} /
	// cron:{trigger_id} / event:{trigger_id}（空 = server 面）；SourceIP 是
	// HTTP 触发的来源 IP 摘要。公开触发路由不经 gRPC 拦截器链，调用记录即
	// 审计载体（设计 §3）。INSERT 期写入、之后不可变。
	TriggerSource string
	SourceIP      string
	// ——客户端调用面（P2，迁移 000016）——InvokingUserID 是客户端触发时的
	// 调用用户（执行身份 token info 携带 invoking_user → 账本 operator 溯源
	// function+user）；ClientIdempotencyKey 是客户端幂等键（partial 唯一索引
	// (project, function, user, key) WHERE key <> '' 去重）。INSERT 期写入、
	// 之后不可变（UpdateInvocation 列白名单排除）。
	InvokingUserID       string
	ClientIdempotencyKey string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	// ——sync 完整透传（v3 §2.2 表「HTTP 触发器」行 / D10；运行期字段，
	// **不落库**）——HTTPHeaders 是函数设置的响应头（服务方已滤 hop-by-hop）；
	// ResponseB64 是无损响应 body（base64）。仅 Invoker 链路（http.request
	// method 的 HTTPResult）填充，供 HTTP 触发器 sync 模式完整回写 HTTP 响应；
	// bun 映射与 gRPC 投影均为显式字段拷贝，二者天然排除（OQ7：headers 不
	// 持久化、流式后置）。
	HTTPHeaders map[string]string
	ResponseB64 string
}
