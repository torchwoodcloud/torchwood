package functions

import "github.com/google/wire"

var ProviderSet = wire.NewSet(
	// v1 回退执行器（每请求一容器）与 v2 dispatcher 客户端都构造（惰性），
	// 由 ProvideExecutor 按 functions.executor 配置择一绑定 Executor 端口
	// （P0.5 执行器 v2；二选一，不同时启用）。
	NewDockerExecutor,
	NewDispatcherExecutor,
	ProvideExecutor,
	// P0 执行身份：Redis 执行 token 服务（server 同步路径经 validator 校验、
	// worker 异步路径经 Functions 聚合铸造/吊销；实现在本包以保 worker 依赖
	// 图不触 infra/auth，见 cmd/worker/import_guard_test.go）。
	NewRedisExecutionTokenService,
	// P1 触发器模块：HTTP 触发器 per-IP 固定窗口限频（组合根经
	// NewTriggerIPLimiter 适配到 serverhttp.TriggerIPLimiter 窄接口）。
	NewTriggerIPRateLimiter,
	// P2 客户端调用面：每用户限频 Redis 固定窗口（端口绑定在 infra
	// ProviderSet：wire.Bind(domainfunctions.ClientQuotaLimiter)）。
	NewClientQuotaLimiter,
)
