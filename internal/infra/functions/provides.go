package functions

import (
	"github.com/google/wire"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

var ProviderSet = wire.NewSet(
	// dispatcher 是唯一执行器（v1 docker 执行器已移除）：经 functions-dispatcher
	// 分发（docker.sock 收敛到 dispatcher 进程，server/worker 零 daemon 依赖）。
	NewDispatcherExecutor,
	wire.Bind(new(domainfunctions.Executor), new(*DispatcherExecutor)),
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
	// 二期阶段 3（git 部署源）：functions-packer HTTP 客户端（url 未配置
	// 仍可装配——PackGit 调用时才报 FailedPrecondition，增量启用）。
	NewPackerClient,
	wire.Bind(new(domainfunctions.SourcePacker), new(*PackerClient)),
)
