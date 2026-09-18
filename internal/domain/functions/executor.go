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
	Runtime      string // e.g. node-18.0（常驻执行器仅 node；python runner 未实现）
	SourcePath   string // path or archive location of function source
	Entrypoint   string // e.g. "index.main"
	Spec         string // 资源规格（shared-1x / shared-2x）
	Timeout      int64  // seconds
	Env          map[string]string
	Data         string // JSON payload
	// ——池策略（P0.5 执行器 v2）：dispatcher 用其管理常驻实例池。零值时
	// dispatcher 侧取平台默认。
	MinInstances           int
	MaxInstances           int
	IdleTTLSeconds         int
	MaxRequestsPerInstance int
	// Concurrency 是单实例并发上限（v3 实例内多路复用，docs/design/
	// functions-v3.md §1.1/§1.5）：默认 1、上限 16；经 dispatcher 客户端进
	// ExecuteRequest.Pool.Concurrency，spawn 时固化进 InstanceRecord。
	Concurrency int
	// ExecutionID 是平台执行 ID（v3 §1.2/§1.5）：dispatcher 客户端经分发
	// header x-tw-execution-id 透传给 runner（ctx.executionId / 日志关联）；
	// 空则不发 header。
	ExecutionID string
	// BuildNode 是该 deployment 首个构建落成的 dispatcher 节点 ID（四期
	// 4a-1，设计 §4 M3/M5：deployment.build_node 随执行规格透传）。本阶段
	// 只透传落类型——dispatcher 侧不消费（local 路由按 build_node 固定路由
	// 目标节点是 4a-2），空串 = 无亲和（存量行/镜像导入路径）。
	BuildNode string
	// ——调用身份投影（runner v5，mlbridge fn-rpc 设计 §2.5 第 1 项）——
	// 把执行记录的调用身份随执行规格贯通到 runner ctx（source /
	// invokingUserId；projectId 由 ProjectID 字段承载），handler 据此做 op
	// 级鉴权与审计——「身份由平台注入」原则在 handler 侧的补全。
	// Source 是触发来源（trigger_source 列原值：client / http:{trigger_id} /
	// cron:{trigger_id} / event:{trigger_id}；server 面为 "server"——app 层
	// buildExecution 把空 trigger_source 映射为该字面值）。dispatcher 经分发
	// header x-tw-source 透传给 runner（ctx.source）；空则不发 header。
	Source string
	// InvokingUserID 是调用用户 id（客户端调用面 invoking_user_id 列）；
	// 空串 = 非用户触发（server 面/触发器路径，系统语义——函数不得把空值
	// 当作匿名调用者放行）。dispatcher 经分发 header x-tw-invoking-user-id
	// 透传给 runner（ctx.invokingUserId）；空则不发 header。
	InvokingUserID string
	// EgressUntrusted 是 egress 分类结果（P2 安全切片，设计 Security #6）：
	// true = 不可信函数（client_callable 或存在 http/cron 触发器），容器
	// attach internal 变体网络（tw-func-<project>-int，出网全 deny）；
	// false = 可信（server key 触发），保持常规网络。分类在 app 层完成
	// （函数属性而非单次调用属性），executor 只消费。
	EgressUntrusted bool
	// ——HTTP 触发器封套通道（v3 §2.3/D10）——恒填充（app 不探测 runner
	// 风格）：TriggerEnvelope 非空时 dispatcher 改发 RawBody 作分发 HTTP body
	// （忽略 Data），封套元数据经分发 header 传递；runner fetch 风格还原
	// Request、main 风格重组 TW_DATA（与现状等价）。RawBodyIsB64 = RawBody
	// 携带的是 base64 文本（调用方手持 body_base64 免先解码的场景）。
	TriggerEnvelope *TriggerEnvelope
	RawBody         []byte
	RawBodyIsB64    bool
}

// ExecutionResult is the output of a function invocation.
type ExecutionResult struct {
	// StatusCode 语义随模板演进：dispatcher（失败由 err 承载，成功恒 0）；
	// v4 fetch 风格（§2.2）= 函数返回的 HTTP status（非零是合法结果——
	// 自定义状态码是一等结果，不映射执行失败）。
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

// BuildSpec 是构建链载荷（Go 一期定稿，设计
// docs/design/functions-runtimes-and-sources.md §0「构建链载荷与接口定稿」
// ——四期纪律①以此为基线，多机路由/广播演化收敛在适配器内部，不再改
// Build 签名）。server/worker 经此把部署上下文全量携带到执行器，dispatcher
// 据此做 runtime 一致性对账（D7）、旧池 drain 精确化（D14）与部署后验证
// spawn（D10）。
type BuildSpec struct {
	ProjectID    string
	FunctionID   string
	DeploymentID string
	// ZipPath 是 zip 源的本地路径（server/worker 共享盘，单机假设）。
	ZipPath string
	// Runtime 是 fn.runtime 原值——D7 一致性校验的比对基准（执行器/daemon
	// 侧把 zip 探测结果与其对账，不一致 InvalidArgument；空值跳过对账）。
	Runtime string
	// FunctionTimeoutSeconds 是函数超时（fn.timeout_seconds 原值）：部署
	// 更新时旧池 drain 的宽限上限（drain ≤ 函数超时，设计 §6；零值 = 不触发
	// drain——历史行为）。
	FunctionTimeoutSeconds int64
	// Env 是验证 spawn 携带的函数 variables（与执行链 env 组装同源：经
	// sanitizeEnv 剔除非法键 + 注入 TW_API_BASE_URL；不含 TW_EXECUTION_TOKEN
	// ——构建/验证期无执行身份）。仅 verify 消费。
	Env map[string]string
	// EgressUntrusted 是 egress 分类结果（与 Execution.EgressUntrusted 同
	// 语义）：untrusted 函数的验证实例挂 internal 变体网络（对抗审查 A1：
	// 否则部署期给不可信镜像一跳出网窗口，执行期 egress 约束被部署期旁路）。
	EgressUntrusted bool
	// Verify 是 config functions.dispatcher.verify_build 的解析值（默认
	// true）：构建成功后 spawn 池外验证实例做 /_tw/health 探针（D10）。
	Verify bool
}

// ImportImageSpec 是镜像导入链载荷（三期阶段 1 定稿，设计 §3）：BYO 镜像
// 源免构建路径的全量上下文随端口携带——dispatcher 侧据此 pull（digest 钉死）
// → retag 进平台命名 → 强制契约验证 spawn。字段与 BuildSpec 同源对齐
// （Env/EgressUntrusted/FunctionTimeoutSeconds 语义一致）。
type ImportImageSpec struct {
	ProjectID    string
	FunctionID   string
	DeploymentID string
	// Reference 是用户提交的原始镜像引用（host/repo[:tag|@sha256:...]）：
	// digest 钉死与 retag 的输入，与部署行 source_url 同值。
	Reference string
	// ExpectedDigest 是预期 digest（部署行 source_ref 已钉死值）：首次导入为
	// 空；worker 补构建 / ready 门禁复检场景非空——实现本地已持有该 digest
	// 的平台镜像时应零 pull 直接确认（幂等语义，设计 §3「本地命中则零
	// pull」），实际解析结果与该值不一致时报错（防 tag 漂移）。
	ExpectedDigest string
	// RegistryUsername/RegistryToken 是一次性 registry 凭证（不落库）：仅
	// 首次导入随请求携带；补构建/复检路径为空——私有镜像补拉失败标 failed
	// 属声明边界（设计 §3）。
	RegistryUsername string
	RegistryToken    string
	// FunctionTimeoutSeconds 是函数超时（fn.timeout_seconds 原值）：与
	// BuildSpec 同语义——旧池 drain 宽限上限（镜像部署也会触发旧池换版）。
	FunctionTimeoutSeconds int64
	// Env 是契约验证 spawn 携带的函数 variables（与 BuildSpec.Env 同源组装：
	// sanitizeEnv 剔除非法键 + TW_API_BASE_URL 注入；不含执行身份 token）。
	Env map[string]string
	// EgressUntrusted 是 egress 分类结果（与 BuildSpec.EgressUntrusted 同
	// 语义）：untrusted 函数的验证实例挂 internal 变体网络（对抗审查 A1）。
	EgressUntrusted bool
}

// Executor is the function runtime port.
//
// 事务边界（redesign §4.8 Phase 2 形态乙，阶段③-b 定稿）：函数代码运行在
// 外部 Docker 容器（进程隔离），与 server 不共享 ctx/事务连接——函数内的
// 多写原子性不经执行器承载，统一由 DatabasesService/ExecuteTransactions
// （execute-tx）RPC 提供（函数经 API/SDK 调用即可，批内事件序 = op 序）。
// 不做跨进程事务魔法（两阶段/补偿协调器不在 POC 范围）。
type Executor interface {
	// Build 将 zip 代码包构建为镜像（解压校验 → runtime 对账 → 生成
	// Dockerfile → docker build）；构建上下文全量随 BuildSpec 携带（一期
	// 定稿形态，见 BuildSpec 注释）。返回执行构建的 dispatcher 节点 ID
	//（四期 4a-1，设计 §4 M5 构建亲和：调用方落 deployment.build_node，
	// local 路由模式下执行/补构建固定路由该节点；签名扩展是 M5 亲和通道
	// ——路由决策本身仍收敛在适配器内部，不受影响）；构建失败返回空串。
	Build(ctx context.Context, spec BuildSpec) (buildNode string, err error)
	// ImportImage 拉取引用镜像并导入为平台镜像（三期阶段 1 端口定稿，设计
	// §3）：pull（用户引用带 tag 时构建期钉死为 digest）→ retag 为平台镜像
	// 名（ImageName(functionID, deploymentID)，本地原始引用不残留）→ 强制
	// 契约验证 spawn → 返回钉死的 digest（调用方落 source_ref）。幂等：
	// spec.ExpectedDigest 非空且本地已持有时零 pull 直接确认（worker 补构建
	// / ready 门禁复检）。
	ImportImage(ctx context.Context, spec ImportImageSpec) (digest string, err error)
	Execute(ctx context.Context, exec Execution) (*ExecutionResult, error)
	// RemoveImage 删除构建产物镜像（幂等，失败由调用方记日志）。
	RemoveImage(ctx context.Context, functionID, deploymentID string) error
}
