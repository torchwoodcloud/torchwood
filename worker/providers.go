package worker

import (
	"github.com/google/wire"
)

// ProviderSet 收纳 worker 业务作业的构造器，由 cmd/worker/provides.go
// 汇入总 ProviderSet——业务实现随仓库根 worker/ 包，cmd 骨架只留装配与
// 入口。OrphanChunkCleaner → *appstorage.Storage 的端口绑定留在总集
// （wire 要求 Bind 与具体类型 provider 同集求值），见 provides.go。
var ProviderSet = wire.NewSet(
	// NewWorkerWithEventTriggers 注入事件触发器消费组（v3 切片 D）；测试
	// 仍可用 NewWorker（无事件消费）。
	NewWorkerWithEventTriggers,
	NewChunkCleaner,
	NewStreamTrimmer,
	NewOutboxWorkerService,
	NewPaymentCloser,
	NewAssetExpirer,
	NewSubscriptionBiller,
	NewUsageRollupWorker,
	NewLeaderboardsCleaner,
	NewLeaderboardsSettler,
)
