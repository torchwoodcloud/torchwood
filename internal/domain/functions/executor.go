package functions

import "context"

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
	// EgressUntrusted 是 egress 分类结果（P2 安全切片，设计 Security #6）：
	// true = 不可信函数（client_callable 或存在 http/cron 触发器），容器
	// attach internal 变体网络（tw-func-<project>-int，出网全 deny）；
	// false = 可信（server key 触发），保持常规网络。分类在 app 层完成
	// （函数属性而非单次调用属性），executor 只消费。
	EgressUntrusted bool
}

// ExecutionResult is the output of a function invocation.
type ExecutionResult struct {
	StatusCode int
	Stdout     string
	Stderr     string
	Response   string
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
