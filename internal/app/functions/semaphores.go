package functions

import (
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
)

// Semaphores 持有 Functions 的全局构建配额信号量（执行并发不设全局信号量
// ——常驻实例池由 functions-dispatcher 内部管控；原 run 信号量随 v1 docker
// 执行器一并移除）。
type Semaphores struct {
	Build semaphore.Semaphore
}

// ProvideSemaphores 基于 Redis 构造分布式构建信号量；Redis 不可用时回退内存。
// build: 4 并发，TTL 6 分钟（覆盖 workerRebuildTimeout 5m + 余量）。
func ProvideSemaphores(client *redis.Client, cfg *config.AppConfig) Semaphores {
	// 配置可覆盖（预留）：functions.semaphore.build.ttl / max
	// 当前固定默认值，满足 W-F 要求。
	buildTTL := 360 * time.Second
	if client == nil {
		return Semaphores{
			Build: semaphore.NewInMemory(maxConcurrentBuilds),
		}
	}
	// 允许通过 config 调整 TTL（若未来开放）；当前未进 config.proto，保持固定。
	_ = cfg
	return Semaphores{
		Build: semaphore.NewRedis(client, "torchwood:sem:build", maxConcurrentBuilds, buildTTL),
	}
}
