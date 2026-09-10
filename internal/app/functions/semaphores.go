package functions

import (
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
)

// Semaphores 持有 Functions 的两类全局配额信号量。
// Wire 以此类型区分 build/run 两个同类型但不同配额的 Semaphore。
type Semaphores struct {
	Build semaphore.Semaphore
	Run   semaphore.Semaphore
}

// ProvideSemaphores 基于 Redis 构造分布式信号量；Redis 不可用时回退内存。
// build: 4 并发，TTL 6 分钟（覆盖 workerRebuildTimeout 5m + 余量）
// run: 16 并发，TTL 400 秒（覆盖 function TimeoutSeconds 上限 300s + 60s + 余量）
func ProvideSemaphores(client *redis.Client, cfg *config.AppConfig) Semaphores {
	// 配置可覆盖（预留）：functions.semaphore.{build,run}.ttl / max
	// 当前固定默认值，满足 W-F 要求。
	var buildTTL = 360 * time.Second
	var runTTL = 400 * time.Second
	if client == nil {
		return Semaphores{
			Build: semaphore.NewInMemory(maxConcurrentBuilds),
			Run:   semaphore.NewInMemory(maxConcurrentRuns),
		}
	}
	// 允许通过 config 调整 TTL（若未来开放）；当前未进 config.proto，保持固定。
	_ = cfg
	buildSem := semaphore.NewRedis(client, "torchwood:sem:build", maxConcurrentBuilds, buildTTL)
	runSem := semaphore.NewRedis(client, "torchwood:sem:run", maxConcurrentRuns, runTTL)
	return Semaphores{
		Build: buildSem,
		Run:   runSem,
	}
}
