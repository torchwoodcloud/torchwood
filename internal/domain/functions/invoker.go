package functions

import "context"

// Invocation 是一次函数调用的完整载荷（v4：Executor.Execute 的继任形态，
// docs/design/functions-v4.md §2/§4）。
//
// 与旧 Execution 的差异（退役字段随执行面外包消解）：
//   - 池策略五字段（Min/MaxInstances、IdleTTL、MaxRequests、Concurrency）
//     退役——常驻服务的容量归 substrate（fleetly 副本数 / k8s HPA / 用户自管）；
//   - Runtime / Spec / Env / SourcePath 退役——运行时与 env 归部署侧
//     （fleetly CLI 下发 / 用户自管，D9）；
//   - 新增 TargetURL / TargetSecret——单一 endpoint target（D8'）；
//   - 新增 Method——触发器路由收编进 JSON-RPC method（D1）。
type Invocation struct {
	FunctionID string
	ProjectID  string
	// TargetURL 是函数常驻服务的调用终点（https 强制；开发环境可放行 http）。
	TargetURL string
	// TargetSecret 是签名密钥明文（仅内存/请求生命周期；落库侧加密存储，
	// API 面掩码——见 infra 适配器）。
	TargetSecret string
	// Method 是 JSON-RPC method（MethodInvoke / MethodCronTick /
	// MethodEventDocument / MethodHTTPRequest）。
	Method string
	// Data 是 invoke / cron.tick / event.document 的 params JSON 文本
	//（http.request 走 TriggerEnvelope + RawBody，Data 忽略）。
	Data string
	// TriggerEnvelope / RawBody / RawBodyIsB64 是 http.request 的 params
	// 组成（HTTPRequestParams）；其余 method 忽略。
	TriggerEnvelope *TriggerEnvelope
	RawBody         []byte
	RawBodyIsB64    bool
	// TimeoutSeconds 是本次调用预算（透传为请求超时）。
	TimeoutSeconds int64
	// ExecutionID 是 JSON-RPC id（请求关联；服务方回显）。
	ExecutionID string
	// Source 是触发来源（trigger_source 列原值：client / http:{id} /
	// cron:{id} / event:{id} / server）。
	Source string
	// InvokingUserID 是调用用户（空 = 非用户触发的系统语义）。
	InvokingUserID string
}

// InvocationResult 是调用的结果（v4：ExecutionResult 的继任形态）。
//
// 字段命名与旧 ExecutionResult 对齐（StatusCode/Headers/ResponseB64/
// Response），HTTP 触发器透传面（finishTransparent）按本结构收敛、只换
// 数据来源不改消费代码。旧 Stdout/Stderr 退役——日志归 substrate 侧
// （fleetly logs / 用户服务自管），torchwood 只见网关往返结果。
type InvocationResult struct {
	// StatusCode 是 http.request 的透传 HTTP status（fetch 语义：非零是
	// 一等结果）；其余 method 恒 0。
	StatusCode int
	// Headers 是 http.request 的透传响应头（已滤 hop-by-hop）；其余方法空。
	Headers map[string]string
	// Response 是函数返回值文本（invoke/cron/event 的 result JSON；
	// http.request 为 body 解码文本 best-effort）。
	Response string
	// ResponseB64 是 http.request 的无损响应 body（base64）；其余方法空。
	ResponseB64 string
	// FunctionError 是函数应用层错误（JSON-RPC error 对象，合法结果——
	// 调用方据此区分「函数报错」与「送达失败」；nil = 函数成功）。
	FunctionError *RPCError
	// DurationMS 是网关往返时长（计量口径：function_duration_ms）。
	DurationMS int64
}

// Invoker 是函数调用端口（v4：Executor 的继任者，仅剩单一方法——
// Build/ImportImage/RemoveImage 随执行面外包退役）。
//
// 语义契约（v4 §4/§5）：
//   - err 非 nil = 平台语义失败（连不通/超时/验签协议不符/响应超限），
//     以 grpc status 上抛（Unavailable / DeadlineExceeded / …）；
//   - err == nil = 送达成功，函数成败看 InvocationResult.FunctionError；
//   - 实现侧必须经 ssrfguard 守卫客户端出站、注入签名与身份头。
type Invoker interface {
	Invoke(ctx context.Context, inv Invocation) (*InvocationResult, error)
}
