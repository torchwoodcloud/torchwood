package functions

import (
	"context"
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
	// TriggerSourceClient 是客户端调用面的执行记录 trigger_source 值
	// （无后缀；http/cron 来源为 "<type>:{trigger_id}"）。
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

// RunnerTemplateVersion 是平台 runner 镜像模板版本（P0.5 执行器 v2 → v3
// 实例内多路复用 §1.2 → v4 Web 标准 fetch 接口 §2.1，docs/design/
// functions-v3.md，端口级契约单一事实源）：模板任何语义变更递增；
// infra/functions/runner 的模板资产与本常量同步（编译期断言），构建时写入
// function_deployments.template_version，存量 deployment 据此按新模板重建。
// 0 = v1 模板或未知。
const RunnerTemplateVersion int32 = 4

// MinConcurrencyTemplateVersion 是支持实例内并发的最低模板版本（v3 §1.5
// 降级保护的判定基准）：v3 引入 per-request 基建（ctx/分桶/per-request
// 超时），并发语义自此成立；v4（§2.1）仅扩展接口面（fetch 双轨），并发
// 语义不变。降级判定按「template_version < 本值」固定——**不得**改用
// RunnerTemplateVersion 比较，否则 v4 版本 bump 会把存量 v3 deployment
// 误降级（重部署才能恢复并发）。
const MinConcurrencyTemplateVersion int32 = 3

// ErrExecutionIdempotencyConflict 表示客户端幂等键冲突（P2）：并发同键的
// 第二次预占 INSERT 撞 partial 唯一索引——调用方应回读既有行原样返回。
var ErrExecutionIdempotencyConflict = errors.New("execution idempotency conflict")

// FunctionRepo 持久化函数/部署/变量/执行记录（bun 静态表适配）。
type FunctionRepo interface {
	CreateFunction(ctx context.Context, fn *Function) error
	GetFunction(ctx context.Context, projectID, functionID string) (*Function, error)
	ListFunctions(ctx context.Context, projectID string) ([]Function, error)
	UpdateFunction(ctx context.Context, fn *Function) error
	DeleteFunction(ctx context.Context, projectID, functionID string) error

	CreateDeployment(ctx context.Context, d *Deployment) error
	GetDeployment(ctx context.Context, projectID, functionID, deploymentID string) (*Deployment, error)
	ListDeployments(ctx context.Context, projectID, functionID string) ([]Deployment, error)
	UpdateDeployment(ctx context.Context, d *Deployment) error
	// ActivateDeployment 在一个事务内把部署置 ready 并维护
	// functions.latest_ready_deployment_id（热路径清账，P0.5）。
	ActivateDeployment(ctx context.Context, d *Deployment) error
	DeleteDeployment(ctx context.Context, projectID, functionID, deploymentID string) error

	SetVariables(ctx context.Context, projectID, functionID string, vars map[string]string) error
	GetVariables(ctx context.Context, projectID, functionID string) (map[string]string, error)

	CreateExecution(ctx context.Context, e *ExecutionRecord) error
	GetExecution(ctx context.Context, projectID, functionID, executionID string) (*ExecutionRecord, error)
	ListExecutions(ctx context.Context, projectID, functionID string, limit int) ([]ExecutionRecord, error)
	UpdateExecution(ctx context.Context, e *ExecutionRecord) error
	// TransitionExecutionStatus 条件更新执行状态（CAS）：仅当当前状态等于 from
	// 时置为 to 并刷新 updated_at，返回是否生效。用于 worker 领取闸门
	// （queued→building 防重复消费）与可重试失败归还（building→queued）。
	TransitionExecutionStatus(ctx context.Context, projectID, functionID, executionID, from, to string) (bool, error)
	// FailExecutionIfActive 将仍处于未终态（queued/building/running）的执行
	// 标记为 failed；completed/failed 不被覆盖（防重复投递回写覆盖终态）。
	FailExecutionIfActive(ctx context.Context, projectID, functionID, executionID, reason string) error
	// RecoverOrphanExecutionsInProject 将指定项目中停留未终态
	// （queued/building/running，P0.5 起纳入 queued——同步快路径崩溃曾留
	// queued 永不入队）且超过各自 staleAfter 的记录标记为 failed。staleAfter
	// 按行计算：timeout_seconds 非空 = 该值 + 120s 宽限（两写预占的行内
	// 快照）；NULL（存量行）回退 1h。limit 是本项目本轮上限，供 app/worker
	// 扣减全局预算。
	RecoverOrphanExecutionsInProject(ctx context.Context, projectID string, olderThan time.Time, limit int) (int64, error)
	// PruneOldExecutionsInProject 清理该项目该函数 server 面来源
	// （trigger_source = ''）超过 keepRecent 条最新之外的终态记录。
	// 条数式保留只作用于 server 面（Q7 保留分级）：client/http/cron 来源行
	// 是限频 DB 降级的窗口内计数依据，条数裁剪会造成少计超发。
	PruneOldExecutionsInProject(ctx context.Context, projectID, functionID string, keepRecent int) error
	// PruneTriggerExecutionsInProject 清理该项目该函数 client/http/cron 来源
	// （Q7 保留分级）超过 olderThan 的终态记录（time-based，48h ≥ 2× 最长
	// 限频窗口 day——保证 DB 降级计数窗口内行不被 prune）。
	PruneTriggerExecutionsInProject(ctx context.Context, projectID, functionID string, olderThan time.Time) error
	// GetExecutionByIdempotencyKey 按 (project, function, user, key) 查既有
	// 客户端执行（幂等冲突回读原样返回；running 返回 running）。未命中 nil。
	GetExecutionByIdempotencyKey(ctx context.Context, projectID, functionID, userID, key string) (*ExecutionRecord, error)
	// CountClientInvocations 统计窗口起点以来该用户的 client 来源执行数
	// （限频 DB 降级路径的计数依据；P2 迁移 partial 索引支撑）。
	CountClientInvocations(ctx context.Context, projectID, functionID, userID string, since time.Time) (int, error)
}

// Function 状态常量。
const (
	DeploymentStatusPending  = "pending"
	DeploymentStatusBuilding = "building"
	DeploymentStatusReady    = "ready"
	DeploymentStatusFailed   = "failed"

	ExecutionStatusQueued    = "queued"
	ExecutionStatusBuilding  = "building"
	ExecutionStatusRunning   = "running"
	ExecutionStatusCompleted = "completed"
	ExecutionStatusFailed    = "failed"
)

type Function struct {
	ID             string
	ProjectID      string
	Name           string
	Runtime        string
	Entrypoint     string
	TimeoutSeconds int
	Spec           string
	Enabled        bool
	// DeclaredScopes 是函数执行 principal 的平台访问声明（P0 执行身份）：
	// 每项形如 "<resource>:<op>"（如 assets:write），词表校验见
	// management.go ValidateDeclaredScopes；空集 = 无平台访问权限（fail-closed）。
	DeclaredScopes []string
	// ——池策略（P0.5 执行器 v2，迁移 000014；平台默认 + per-function 覆盖，
	// 语义见设计 §6）——v1 docker executor 忽略这些字段。
	MinInstances           int // 保温下限（0 = 纯 scale-from-zero）
	MaxInstances           int // 突发并发上限（超限有界排队）
	IdleTTLSeconds         int // 空闲回收阈值（实例数 > min 时）
	MaxRequestsPerInstance int // 实例请求数到期排空替换（防内存泄漏，Lambda 同款）
	// Concurrency 是单实例并发上限（v3 实例内多路复用，迁移 000017，
	// docs/design/functions-v3.md §1.1/§1.5）：默认 1（v2 串行等价）、上限 16
	//（DB CHECK 兜底），显式 opt-in 承诺 main 可重入。v1 docker executor 忽略。
	Concurrency int
	// LatestReadyDeploymentID 是最新 ready 部署的冗余投影（热路径清账，
	// 设计 §6 约束③）：selectDeployment 优先读该列，NULL 回退全量列表逻辑；
	// 由 CreateDeployment ready / DeleteDeployment 同事务维护。
	LatestReadyDeploymentID string
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

type Deployment struct {
	ID         string
	FunctionID string
	ProjectID  string
	Size       int64
	Status     string // pending/building/ready/failed
	Error      string
	// TemplateVersion 是构建所用镜像模板版本（P0.5 模板版本化重建；构建时
	// 由 runner 模板版本写入，0 = v1 模板或未知）。
	TemplateVersion int32
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Variable 是函数环境变量实体。Kind 区分 text（普通文本）与 secret
// （第三方密钥；P0 起弃用 variables 存平台凭证——平台能力一律走执行身份）。
type Variable struct {
	ID         string
	FunctionID string
	Key        string
	Value      string
	Kind       string
}

// Variable kind 词表（迁移 000013 function_variables.kind CHECK 约束同源）。
const (
	VariableKindText   = "text"
	VariableKindSecret = "secret"
)

// ExecutionRecord 是执行记录实体；命名区别于 executor 入参 Execution。
type ExecutionRecord struct {
	ID                string
	FunctionID        string
	ProjectID         string
	DeploymentID      string
	Status            string
	Response          string
	ResponseTruncated bool
	Stdout            string
	StdoutTruncated   bool
	Stderr            string
	StderrTruncated   bool
	StatusCode        int
	DurationMS        int64
	Error             string
	// TimeoutSeconds 是函数超时的行内快照（P0.5 两写预占记账；周期孤儿恢复
	// 的 staleAfter = 本值 + 120s 宽限，NULL 回退 1h）。
	TimeoutSeconds *int
	// ——触发来源（P1，迁移 000015）——TriggerSource 形如 http:{trigger_id} /
	// cron:{trigger_id}（空 = server 面）；SourceIP 是 HTTP 触发的来源 IP
	// 摘要。公开触发路由不经 gRPC 拦截器链，执行记录即审计载体（设计 §3）。
	// INSERT 期写入、之后不可变。
	TriggerSource string
	SourceIP      string
	// ——客户端调用面（P2，迁移 000016）——InvokingUserID 是客户端触发时的
	// 调用用户（执行身份 token info 携带 invoking_user → 账本 operator 溯源
	// function+user）；ClientIdempotencyKey 是客户端幂等键（partial 唯一索引
	// (project, function, user, key) WHERE key <> '' 去重）。INSERT 期写入、
	// 之后不可变（UpdateExecution 列白名单排除）。
	InvokingUserID       string
	ClientIdempotencyKey string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	// ——sync 完整透传（v3 §2.2 表「HTTP 触发器」行 / D10；运行期字段，
	// **不落库**）——HTTPHeaders 是 fetch 风格函数设置的响应头（runner 已滤
	// hop-by-hop/date/server）；ResponseB64 是无损响应 body（base64，≤64KB，
	// 截断发生在 runner）。仅 executor 链路（dispatcher v4 fetch 风格）填充，
	// 供 HTTP 触发器 sync 模式完整回写 HTTP 响应；bun 映射与 gRPC 投影均为
	// 显式字段拷贝，二者天然排除（OQ7：headers 不持久化、流式后置）。
	HTTPHeaders map[string]string
	ResponseB64 string
}
