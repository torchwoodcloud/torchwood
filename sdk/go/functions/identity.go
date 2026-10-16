package functions

import "context"

// Identity 是执行身份六件（v4 签名调用的身份投影，docs/design/functions-v4.md
// §4）：token / source / invoking-user / project 四件走身份头，executionId
// 走载荷关联键（RPC 轴 = JSON-RPC id、事件轴 = CloudEvent id），apiBaseUrl
// 走部署侧注入的 env TW_API_BASE_URL（NewServer 构建期读入）。统一入口
// NewServer 已自动注入每请求身份，函数内经 FromContext 读取；并发下各请求
// ctx 相互隔离。
type Identity struct {
	// ExecutionToken 是本次执行的短期凭证（x-tw-execution-token；Bearer
	// twx_… 形态，供平台 API 鉴权，执行结束即失效）。
	ExecutionToken string
	// APIBaseURL 是平台 API base（部署侧注入的 env TW_API_BASE_URL），如
	// https://api.example.com——Client 据此发起平台调用。
	APIBaseURL string
	// ExecutionID 是本次执行的关联键（RPC 轴 = JSON-RPC id；事件轴 =
	// CloudEvent id——日志关联 / 幂等键推荐来源）。
	ExecutionID string
	// Source 是调用来源（x-tw-source；恒非空——server / client /
	// {http|cron|event}:{trigger_id}；header 缺省回落 "server"）。
	Source string
	// InvokingUserID 是触发执行的端用户（x-tw-invoking-user-id；空 = 非用户
	// 触发，系统语义）。
	InvokingUserID string
	// ProjectID 是执行所属项目（x-tw-project-id）。
	ProjectID string
}

type identityCtxKey struct{}

// WithIdentity 把执行身份注入 context（NewServer 已自动注入；导出用于测试
// 与自定义包装）。
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// FromContext 读取执行身份；未注入（含 nil ctx）时返回零值 Identity。
func FromContext(ctx context.Context) Identity {
	if ctx == nil {
		return Identity{}
	}
	id, _ := ctx.Value(identityCtxKey{}).(Identity)
	return id
}
