package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

type Function struct {
	bun.BaseModel `bun:"table:functions,alias:f"`

	ID             string   `bun:"id,pk"`
	ProjectID      string   `bun:"project_id,notnull"`
	Name           string   `bun:"name,notnull"`
	Runtime        string   `bun:"runtime,notnull"`
	Entrypoint     string   `bun:"entrypoint,notnull,default:'index.main'"`
	TimeoutSeconds int      `bun:"timeout_seconds,notnull,default:15"`
	Spec           string   `bun:"spec,notnull,default:'shared-1x'"`
	Enabled        bool     `bun:"enabled,notnull"`
	DeclaredScopes []string `bun:"declared_scopes,array,notnull,default:'{}'"`
	// ——池策略（P0.5 执行器 v2，迁移 000014；CHECK 下限与迁移同源）——
	MinInstances           int `bun:"min_instances,notnull,default:0"`
	MaxInstances           int `bun:"max_instances,notnull,default:2"`
	IdleTTLSeconds         int `bun:"idle_ttl_seconds,notnull,default:300"`
	MaxRequestsPerInstance int `bun:"max_requests_per_instance,notnull,default:1000"`
	// LatestReadyDeploymentID 是最新 ready 部署的冗余投影（热路径清账）；
	// 可空，NULL 回退全量列表逻辑。
	LatestReadyDeploymentID string `bun:"latest_ready_deployment_id,nullzero"`
	// ——客户端调用面策略（P2，迁移 000016；CHECK 下限与迁移同源）——
	ClientCallable         bool      `bun:"client_callable,notnull,default:false"`
	ClientAnonymousAllowed bool      `bun:"client_anonymous_allowed,notnull,default:false"`
	ClientPerUserLimit     int       `bun:"client_per_user_limit,notnull,default:0"`
	ClientLimitWindow      string    `bun:"client_limit_window,notnull,default:'day'"`
	CreatedAt              time.Time `bun:"created_at,notnull"`
	UpdatedAt              time.Time `bun:"updated_at,notnull"`
}

type FunctionDeployment struct {
	bun.BaseModel `bun:"table:function_deployments,alias:fd"`

	ID         string `bun:"id,pk"`
	FunctionID string `bun:"function_id,notnull"`
	ProjectID  string `bun:"project_id,notnull"`
	Size       int64  `bun:"size,notnull,default:0"`
	Status     string `bun:"status,notnull,default:'pending'"`
	Error      string `bun:"error,notnull,default:''"`
	// TemplateVersion 是构建所用镜像模板版本（P0.5；0 = v1 模板/未知）。
	TemplateVersion int32     `bun:"template_version"`
	CreatedAt       time.Time `bun:"created_at,notnull"`
	UpdatedAt       time.Time `bun:"updated_at,notnull"`
}

type FunctionVariable struct {
	bun.BaseModel `bun:"table:function_variables,alias:fv"`

	ID         string `bun:"id,pk"`
	FunctionID string `bun:"function_id,notnull"`
	ProjectID  string `bun:"project_id,notnull"`
	Key        string `bun:"key,notnull"`
	Value      string `bun:"value,notnull"`
	// kind ∈ {text, secret}（迁移 000013 CHECK 约束）；P0 只写 text，secret
	// 的 API 面与注入解析随后续阶段开放。
	Kind string `bun:"kind,notnull,default:'text'"`
}

type FunctionExecution struct {
	bun.BaseModel `bun:"table:function_executions,alias:fe"`

	ID                string `bun:"id,pk"`
	FunctionID        string `bun:"function_id,notnull"`
	ProjectID         string `bun:"project_id,notnull"`
	DeploymentID      string `bun:"deployment_id,notnull"`
	Status            string `bun:"status,notnull,default:'queued'"`
	Response          string `bun:"response,notnull,default:''"`
	ResponseTruncated bool   `bun:"response_truncated,notnull,default:false"`
	Stdout            string `bun:"stdout,notnull,default:''"`
	StdoutTruncated   bool   `bun:"stdout_truncated,notnull,default:false"`
	Stderr            string `bun:"stderr,notnull,default:''"`
	StderrTruncated   bool   `bun:"stderr_truncated,notnull,default:false"`
	StatusCode        int    `bun:"status_code,notnull,default:0"`
	DurationMS        int64  `bun:"duration_ms,notnull,default:0"`
	Error             string `bun:"error,notnull,default:''"`
	// TimeoutSeconds 是函数超时的行内快照（P0.5 两写预占；NULL = 旧行，
	// 孤儿恢复回退 1h）。
	TimeoutSeconds *int `bun:"timeout_seconds"`
	// ——触发器来源（P1，迁移 000015；INSERT 期写入、之后不可变）——
	// TriggerSource 形如 http:{trigger_id} / cron:{trigger_id}；空 = server 面。
	// 该路由不经 gRPC 拦截器链，执行记录即审计载体（设计 §3）。
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
