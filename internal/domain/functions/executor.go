package functions

import "context"

// TriggerEnvelope 是 HTTP 触发器封套元数据（v3 §2.3，对抗审查修正：封套经
// 独立分发 header `x-tw-trigger-envelope`（base64 JSON）传递，**不含 body**——
// body 走分发 HTTP body 通道（Execution.RawBody））。Headers 与 TW_DATA 封套
// 同一白名单（x-* + content-type + wechatpay-*，handler 构建期剥除非白名单）。
// fetch 风格 runner 据此还原 Request（url = http://trigger{path}?{raw_query}）；
// main 风格 runner 将其与 RawBody 重组为 TW_DATA 封套注入（与现状等价，
// §2.3「封套注入」/D9 双轨）。上限：编码后 12KB（dispatcher 侧强制，超限
// InvalidArgument——Node http header 默认 16KB，base64 膨胀后须显式限界）。
type TriggerEnvelope struct {
	Method   string              `json:"method"`
	Path     string              `json:"path"`
	RawQuery string              `json:"raw_query,omitempty"`
	Headers  map[string][]string `json:"headers,omitempty"`
}

// Execution describes a single function invocation.
type Execution struct {
	FunctionID   string
	DeploymentID string // 构建产物镜像标识（{registry}/func-{functionID}-{deploymentID}）
	ProjectID    string // 所属项目：决定执行容器网络（tw-func-<project.id>，Round4 J5-4）
	Runtime      string // e.g. node-18.0, python-3.11
	SourcePath   string // path or archive location of function source
	Entrypoint   string // e.g. "index.main"
	Spec         string // 资源规格（shared-1x / shared-2x）
	Timeout      int64  // seconds
	Env          map[string]string
	Data         string // JSON payload
	// ——池策略（P0.5 执行器 v2）：dispatcher executor 用其管理常驻实例池；
	// v1 docker executor 忽略。零值时 dispatcher 侧取平台默认。
	MinInstances           int
	MaxInstances           int
	IdleTTLSeconds         int
	MaxRequestsPerInstance int
	// Concurrency 是单实例并发上限（v3 实例内多路复用，docs/design/
	// functions-v3.md §1.1/§1.5）：默认 1、上限 16；经 dispatcher 客户端进
	// ExecuteRequest.Pool.Concurrency，spawn 时固化进 InstanceRecord。
	// v1 docker executor 忽略。
	Concurrency int
	// ExecutionID 是平台执行 ID（v3 §1.2/§1.5）：dispatcher 客户端经分发
	// header x-tw-execution-id 透传给 runner（ctx.executionId / 日志关联）；
	// 空则不发 header。
	ExecutionID string
	// EgressUntrusted 是 egress 分类结果（P2 安全切片，设计 Security #6）：
	// true = 不可信函数（client_callable 或存在 http/cron 触发器），容器
	// attach internal 变体网络（tw-func-<project>-int，出网全 deny）；
	// false = 可信（server key 触发），保持常规网络。分类在 app 层完成
	// （函数属性而非单次调用属性），executor 只消费。
	EgressUntrusted bool
	// ——HTTP 触发器封套通道（v3 §2.3/D10；executor=dispatcher 的 v4 路径
	// 消费，v1 docker executor 忽略）——恒填充（app 不探测 runner 风格）：
	// TriggerEnvelope 非空时 dispatcher 改发 RawBody 作分发 HTTP body
	// （忽略 Data），封套元数据经分发 header 传递；runner fetch 风格还原
	// Request、main 风格重组 TW_DATA（与现状等价）。RawBodyIsB64 = RawBody
	// 携带的是 base64 文本（调用方手持 body_base64 免先解码的场景）。
	TriggerEnvelope *TriggerEnvelope
	RawBody         []byte
	RawBodyIsB64    bool
}

// ExecutionResult is the output of a function invocation.
type ExecutionResult struct {
	// StatusCode 语义随执行器/模板演进：v1 = 容器退出码（非零 = failed）；
	// v2/v3 dispatcher = 函数失败置 1（err 已承载失败，成功恒 0）；
	// v4 fetch 风格（§2.2）= 函数返回的 HTTP status（非零是合法结果——
	// 自定义状态码是一等结果，不再映射执行失败）。
	StatusCode int
	Stdout     string
	Stderr     string
	// Response 是函数返回值的文本形态：main 风格 = JSON 文本透传；fetch
	// 风格 = 响应 body 解码后文本（best-effort，截断口径 64KB）。
	Response string
	// ResponseB64 是 fetch 风格的无损响应 body（base64；截断在 runner）。
	// main 风格恒空。
	ResponseB64 string
	// Headers 是 fetch 风格函数设置的响应头（runner 已滤 hop-by-hop）；
	// main 风格恒 nil（invoke 路径 headers 不回传，OQ7）。仅触发器 sync
	// 透传消费。
	Headers    map[string]string
	DurationMS int64
}

// Executor is the function runtime port.
//
// 事务边界（redesign §4.8 Phase 2 形态乙，阶段③-b 定稿）：函数代码运行在
// 外部 Docker 容器（进程隔离），与 server 不共享 ctx/事务连接——函数内的
// 多写原子性不经执行器承载，统一由 DatabasesService/ExecuteTransactions
// （execute-tx）RPC 提供（函数经 API/SDK 调用即可，批内事件序 = op 序）。
// 不做跨进程事务魔法（两阶段/补偿协调器不在 POC 范围）。
type Executor interface {
	// Build 将 zip 代码包构建为镜像（解压校验 → 生成 Dockerfile → docker build）。
	Build(ctx context.Context, functionID, deploymentID, zipPath string) error
	Execute(ctx context.Context, exec Execution) (*ExecutionResult, error)
	// RemoveImage 删除构建产物镜像（幂等，失败由调用方记日志）。
	RemoveImage(ctx context.Context, functionID, deploymentID string) error
}
