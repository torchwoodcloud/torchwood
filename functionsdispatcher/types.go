// Package functionsdispatcher 是执行器 v2 的分发通路（P0.5，owner 拍板
// 方案③：独立 functions-dispatcher 进程，docs/design/
// functions-execution-identity-and-triggers.md §6「分发通路」）。
//
// 职责：专职持有 docker.sock（compose 挂载），按需 join per-project 函数
// 网络（自身容器 NetworkConnect），承接 Build/Execute/RemoveImage 全部
// daemon 操作——server/worker 零 daemon 依赖，host-root 等价凭证收敛到
// 非 API 面进程。池管理（常驻实例注册表/冷启动收敛/有界排队/idle 回收/
// 幽灵对账）见 pool.go。
//
// 无状态进程、初期单副本；注册表在 Redis（torchwood:fninst:*），需要 HA
// 时双副本 + Redis 仲裁（设计 §6）。
package functionsdispatcher

import (
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

const (
	// runnerPort 是 runner 在容器内监听的 HTTP 端口（与 infra/functions/runner
	// 包常量一致，本地重复定义避免仅为取常量而拉入 runner 依赖；构建与
	// health/invoke 另经 daemon/pool 使用该包，注释互指）。
	runnerPort = 18080

	// registryKeyPrefix 是实例注册表键前缀：torchwood:fninst:{project}:{function}
	// 为 Redis Hash（field = instance_id，value = InstanceRecord JSON）。
	registryKeyPrefix = "torchwood:fninst:"

	// spawnLockPrefix 是同函数并发 spawn 收敛锁前缀（SETNX，防 daemon 重启
	// 后全量冷启动风暴）。
	spawnLockPrefix = "torchwood:fnspawn:"

	// 默认池参数（config 可覆盖）。
	defaultMaxResidentInstances = 8  // 每 daemon 常驻总量上限（Q11 拍板）
	defaultQueueDepth           = 32 // 单函数排队深度上限
	defaultQueueHeadTimeout     = 10 * time.Second
	defaultBootTimeout          = 60 * time.Second
	defaultReaperInterval       = 15 * time.Second
	defaultPollInterval         = 25 * time.Millisecond

	// leaseTTL 是 dispatch 认领时的租约时长：判活 = dispatch 续租 + busy
	// 标记；busy 实例不因租约过期被误杀（超期过久视为残留，见 reaper）。
	leaseTTL = 10 * time.Minute

	// stuckBusyGrace 是 busy 实例租约过期后的宽限：超过即视为请求方已消失
	// （dispatcher 崩溃重启等），强杀实例防永久占用。
	stuckBusyGrace = 10 * time.Minute

	// maxBuildBodyBytes 是构建请求 zip 的 base64 解码上限（对齐
	// CreateDeployment 50MiB 的 multipart 限额）。
	maxBuildBodyBytes = 50 << 20

	// maxLogTailBytes 是 dispatch 响应 stdout/stderr 尾部截断（对齐平台
	// 64KB 输出截断口径）。
	maxLogTailBytes = 64 << 10

	// maxInvokeResponseBytes 是单次 runner invoke 响应的读取上限：v4 fetch
	// 风格封套 = stdout 64KB + stderr 64KB + body_base64（64KB body → 88KB）
	// + headers + JSON 开销，64KB 级旧上限会截断 JSON 导致解析静默归零——
	// 取 1MB 覆盖（v3 §2.4：Response body 一期 64KB 截断）。
	maxInvokeResponseBytes = 1 << 20

	// maxTriggerEnvelopeHeaderBytes 是触发器封套分发 header（base64 JSON，
	// 不含 body）的编码后上限（v3 §2.3 对抗审查修正：Node http 解析默认
	// 16KB，base64 膨胀后须显式限界；封套正常体积 <4KB，超限 400）。
	maxTriggerEnvelopeHeaderBytes = 12 << 10

	// spawnWarnInterval 是 spawn 失败告警的限频窗口（per function）：排队
	// 请求按 PollInterval 反复重试 trySpawn，失败现场告警按窗口收敛。
	spawnWarnInterval = 5 * time.Second

	// defaultTimeoutBudget 是实例累计超时熔断阈值默认值（v3 §1.4 超时熔断：
	// 「超时不杀」拆掉了串行模型顺带消灭僵尸负载的保护，累计超时达阈值的
	// 实例杀掉重建；config functions.dispatcher.timeout_budget 可覆盖，
	// <=0 取本默认）。
	defaultTimeoutBudget = 5
)

// BuildRequest 是 POST /v1/dispatch/builds 入参：zip 字节内联（base64）——
// server/worker 与 dispatcher 无共享文件系统假设（compose 拓扑下各自容器
// /tmp 独立），构建是低频管理操作，内网传输 ≤50MiB 可接受。
type BuildRequest struct {
	ProjectID    string `json:"project_id"`
	FunctionID   string `json:"function_id"`
	DeploymentID string `json:"deployment_id"`
	ZipBase64    string `json:"zip_base64"`
	// FunctionTimeoutSeconds 用于部署更新时旧池 drain 的宽限上限
	// （drain ≤ 函数超时，设计 §6）。
	FunctionTimeoutSeconds int64 `json:"function_timeout_seconds,omitempty"`
}

// BuildResponse 是 builds 出参；Error 非空 = 构建失败（含日志尾部）。
type BuildResponse struct {
	Error string `json:"error,omitempty"`
}

// PoolPolicy 是单次执行携带的函数池策略（dispatcher 无 DB 依赖，策略由
// 调用方从函数记录读出随请求传递）。零值字段取平台默认。
type PoolPolicy struct {
	MinInstances           int `json:"min_instances,omitempty"`
	MaxInstances           int `json:"max_instances,omitempty"`
	IdleTTLSeconds         int `json:"idle_ttl_seconds,omitempty"`
	MaxRequestsPerInstance int `json:"max_requests_per_instance,omitempty"`
	// Concurrency 是单实例并发上限（v3 §1.1；spawn 时固化进 InstanceRecord）。
	// 本切片（v3 切片一）恒 1：DB 列/proto/透传链在切片二（A2）接入，本切片
	// 不做 DB 迁移、不动 proto，<=0 归一化为 1。
	Concurrency int `json:"concurrency,omitempty"`
}

// ExecuteRequest 是 POST /v1/dispatch/executions 入参：执行规格（镜像名、
// project、function、deployment、timeout、env 映射、data、池策略）。
type ExecuteRequest struct {
	Image          string            `json:"image"`
	ProjectID      string            `json:"project_id"`
	FunctionID     string            `json:"function_id"`
	DeploymentID   string            `json:"deployment_id"`
	Runtime        string            `json:"runtime,omitempty"`
	Spec           string            `json:"spec,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
	Env            map[string]string `json:"env,omitempty"`
	// ExecutionToken 是本次执行的短期平台凭证（P0 链路不变、只换注入通道：
	// v2 经分发 header 传给 runner，不再进容器 env；常驻的是容器不是凭证）。
	ExecutionToken string `json:"execution_token,omitempty"`
	// ExecutionID 是平台执行 ID（v3 §1.2/§1.5：经分发 header
	// x-tw-execution-id 透传给 runner，供 ctx.executionId / 日志关联）。
	// A2 才从 app 侧传入，本切片零值即不发 header。
	ExecutionID string     `json:"execution_id,omitempty"`
	Data        string     `json:"data"`
	Pool        PoolPolicy `json:"pool"`
	// ——HTTP 触发器封套通道（v3 §2.3/D10）——TriggerEnvelope 非空 = 分发
	// 请求改走封套模式：①封套元数据经分发 header `x-tw-trigger-envelope`
	//（base64 JSON，不含 body；编码后 ≤12KB，Dispatch 入口校验超限
	// InvalidArgument）；②HTTP body 改发 RawBody（忽略 Data）——runner v4
	// fetch 风格据此还原 Request（url = http://trigger{path}?{raw_query}），
	// main 风格重组 TW_DATA（与现状等价，D9 双轨）。曾考虑 body 外层包装
	// `{"_tw_trigger":...}`，否决——键空间污染破坏「TW_DATA 即用户数据」
	// 契约（v3 §2.3）。
	TriggerEnvelope *domainfunctions.TriggerEnvelope `json:"trigger_envelope,omitempty"`
	// RawBody 是触发器原始 body（app 层从已过 413 校验的请求体填充；
	// RawBodyIsB64 = RawBody 携带 base64 文本、分发前需解码——调用方手持
	// body_base64 免先解码的场景）。仅 TriggerEnvelope 非空时消费。
	RawBody      []byte `json:"raw_body,omitempty"`
	RawBodyIsB64 bool   `json:"raw_body_is_b64,omitempty"`
	// EgressUntrusted 是 egress 分类结果（P2 安全切片，设计 Security #6）：
	// true = 不可信函数容器挂 internal 变体网络（tw-func-<project>-int，
	// docker internal: true——出网全 deny）；false = 常规网络。分类在 app 层
	// 完成（函数属性），dispatcher 只消费。
	EgressUntrusted bool `json:"egress_untrusted,omitempty"`
}

// ExecuteResponse 是 executions 出参。
type ExecuteResponse struct {
	// Status 是 "ok" | "error"：error = 函数执行失败（含超时）；dispatcher
	// 层面的失败（鉴权/参数/池超限）走 HTTP 状态码。
	Status     string `json:"status"`
	Response   string `json:"response,omitempty"`
	StdoutTail string `json:"stdout_tail,omitempty"`
	StderrTail string `json:"stderr_tail,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	// StatusCode 语义随模板演进：v1 = 容器退出码（v2/v3 ok 恒 0、失败置 1）；
	// v4 fetch 风格（v3 §2.2）承载函数 HTTP status（非零 = 合法结果，
	// TriggerEnvelope 请求且 runner ok 时 ≥200）。main 风格保持 0。
	StatusCode int    `json:"status_code"`
	Error      string `json:"error,omitempty"`
	// ——fetch 风格扩展（v3 §2.2，TriggerEnvelope 请求且 runner ok 时填充；
	// main 风格恒空）——HTTPHeaders 是函数设置的响应头（runner 已滤
	// hop-by-hop/date/server），供触发器 sync 模式透传（OQ7：仅此路径回传，
	// invoke 路径不回传）；ResponseB64 是无损响应 body（base64，64KB 截断在
	// runner，truncated 随截断发生）。Response 恒为 body 的解码文本
	//（best-effort），文本场景照旧可用。
	HTTPHeaders map[string]string `json:"http_headers,omitempty"`
	ResponseB64 string            `json:"response_b64,omitempty"`
}

// RemoveImageRequest 是 POST /v1/dispatch/images/remove 入参（幂等）。
type RemoveImageRequest struct {
	FunctionID   string `json:"function_id"`
	DeploymentID string `json:"deployment_id"`
}
