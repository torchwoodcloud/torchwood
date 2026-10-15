package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// Function 是 functions 表行模型（v4 target 模型，docs/design/functions-v4.md
// §3：执行位置 = endpoint 常驻服务；池策略/运行时/资源规格列随迁移 000028
// 退役，target 列族承载调用终点与签名密钥）。
type Function struct {
	bun.BaseModel `bun:"table:functions,alias:f"`

	ID             string   `bun:"id,pk"`
	ProjectID      string   `bun:"project_id,notnull"`
	Name           string   `bun:"name,notnull"`
	TimeoutSeconds int      `bun:"timeout_seconds,notnull,default:15"`
	Enabled        bool     `bun:"enabled,notnull"`
	DeclaredScopes []string `bun:"declared_scopes,array,notnull,default:'{}'"`
	// ——endpoint target（v4 §3，迁移 000028）——TargetSecretEnc 是签名密钥
	// 的 secretbox 密文（enc:v1: 前缀；明文仅在创建/轮换响应出现一次——
	// 签名方每次调用需明文计算 HMAC）；TargetPreviousSecretEnc 是轮换宽限
	// 窗内的旧钥密文；指纹是管理面唯一可见形态。
	TargetURL               string    `bun:"target_url,notnull,default:''"`
	TargetSecretEnc         string    `bun:"target_secret_enc,notnull,default:''"`
	TargetPreviousSecretEnc string    `bun:"target_previous_secret_enc,nullzero"`
	TargetSecretRotatedAt   time.Time `bun:"target_secret_rotated_at,nullzero"`
	TargetSecretFingerprint string    `bun:"target_secret_fingerprint,notnull,default:''"`
	// ——客户端调用面策略（P2，迁移 000016；CHECK 下限与迁移同源）——
	ClientCallable         bool      `bun:"client_callable,notnull,default:false"`
	ClientAnonymousAllowed bool      `bun:"client_anonymous_allowed,notnull,default:false"`
	ClientPerUserLimit     int       `bun:"client_per_user_limit,notnull,default:0"`
	ClientLimitWindow      string    `bun:"client_limit_window,notnull,default:'day'"`
	CreatedAt              time.Time `bun:"created_at,notnull"`
	UpdatedAt              time.Time `bun:"updated_at,notnull"`
}

// FunctionInvocation 是 function_invocations 表行模型（v4 §2：原
// FunctionExecution/function_executions 随迁移 000028 改名——torchwood 只
// 发起调用、不执行）。stdout/stderr 列随 v4 退役（日志归 substrate 侧）。
type FunctionInvocation struct {
	bun.BaseModel `bun:"table:function_invocations,alias:fi"`

	ID                string `bun:"id,pk"`
	FunctionID        string `bun:"function_id,notnull"`
	ProjectID         string `bun:"project_id,notnull"`
	Status            string `bun:"status,notnull,default:'queued'"`
	Response          string `bun:"response,notnull,default:''"`
	ResponseTruncated bool   `bun:"response_truncated,notnull,default:false"`
	// StatusCode 是 http.request 透传的函数 HTTP status（fetch 语义，非零是
	// 一等结果）；其余 method 恒 0。
	StatusCode int    `bun:"status_code,notnull,default:0"`
	DurationMS int64  `bun:"duration_ms,notnull,default:0"`
	Error      string `bun:"error,notnull,default:''"`
	// TimeoutSeconds 是函数超时的行内快照（P0.5 两写预占；NULL = 旧行，
	// 孤儿恢复回退 1h）。
	TimeoutSeconds *int `bun:"timeout_seconds"`
	// ——触发器来源（P1，迁移 000015；INSERT 期写入、之后不可变）——
	// TriggerSource 形如 http:{trigger_id} / cron:{trigger_id} / event:{id}；
	// 空 = server 面。公开触发路由不经 gRPC 拦截器链，调用记录即审计载体。
	TriggerSource string `bun:"trigger_source,notnull,default:''"`
	SourceIP      string `bun:"source_ip,notnull,default:''"`
	// ——客户端调用面（P2，迁移 000016；INSERT 期写入、之后不可变）——
	InvokingUserID       string    `bun:"invoking_user_id,notnull,default:''"`
	ClientIdempotencyKey string    `bun:"client_idempotency_key,notnull,default:''"`
	CreatedAt            time.Time `bun:"created_at,notnull"`
	UpdatedAt            time.Time `bun:"updated_at,notnull"`
}

// FunctionTrigger 是函数触发器（P1，迁移 000015）：HTTP webhook / cron 定时。
// config JSONB 存分类型配置；token 提为独立列（UNIQUE 索引支撑 /f/ 查找）。
type FunctionTrigger struct {
	bun.BaseModel `bun:"table:function_triggers,alias:ft"`

	ID         string          `bun:"id,pk"`
	ProjectID  string          `bun:"project_id,notnull"`
	FunctionID string          `bun:"function_id,notnull"`
	Type       string          `bun:"type,notnull"`
	Config     json.RawMessage `bun:"config,type:jsonb,notnull"`
	// Token 是 http 触发器的公开调用凭证；cron 为 NULL（UNIQUE 不去重 NULL）。
	Token   string `bun:"token,nullzero"`
	Enabled bool   `bun:"enabled,notnull,default:true"`
	// NextRunAt 是 cron 专用：下一次计划执行时刻；http 恒 NULL。
	NextRunAt time.Time `bun:"next_run_at,nullzero"`
	CreatedAt time.Time `bun:"created_at,notnull"`
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}
