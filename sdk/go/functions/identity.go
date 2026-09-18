package functions

import "context"

// Identity 是执行身份六件（分发 header / env 投影，与 runner ctx、fetch env
// 同源）。Start 系列 / Mux 的 handler ctx 已自动注入，函数内经 FromContext
// 读取；并发下各请求 ctx 相互隔离（无 runner.js 进程 env 覆盖问题）。
type Identity struct {
	// ExecutionToken 是本次执行的短期凭证（x-tw-execution-token；Bearer
	// twx_… 形态，供平台 API 鉴权，执行结束即失效）。
	ExecutionToken string
	// APIBaseURL 是平台 API base（env TW_API_BASE_URL），如
	// https://api.example.com——Client 据此发起平台调用。
	APIBaseURL string
	// ExecutionID 是平台执行 ID（x-tw-execution-id；日志关联）。
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
type dispatchCtxKey struct{}

// WithIdentity 把执行身份注入 context（Start 系列已自动注入；导出用于测试
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

// withDispatch / dispatchFromContext 是契约循环 → Mux 的分发信息通道
// （原始 body + 触发器封套）：Listen 包裹的 handler 经参数直达，Mux 的
// http.Handler 形态经 ctx 携带。
func withDispatch(ctx context.Context, d *dispatchInfo) context.Context {
	return context.WithValue(ctx, dispatchCtxKey{}, d)
}

func dispatchFromContext(ctx context.Context) (*dispatchInfo, bool) {
	if ctx == nil {
		return nil, false
	}
	d, ok := ctx.Value(dispatchCtxKey{}).(*dispatchInfo)
	return d, ok
}
