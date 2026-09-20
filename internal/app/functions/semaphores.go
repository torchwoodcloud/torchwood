package functions

import (
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
)

// Semaphores 持有 Functions 的全局构建原语（执行并发不设全局信号量
// ——常驻实例池由 dispatcher 内部管控；原 run 信号量随 v1 docker
// 执行器一并移除）。
type Semaphores struct {
	Build semaphore.Semaphore
	// RebuildDedup 是镜像缺失自动重建的在途去重端口（跨进程，rebuild.go
	// /rebuild_dedup.go；nil = 回落进程内 map，单副本语义）。经
	// ProvideSemaphores 透传进 Functions——构造器位形保持不变（既有调用方
	// Semaphores{} 零值即回落语义）。
	RebuildDedup RebuildDedup
}

// ProvideSemaphores 基于 Redis 构造分布式构建信号量；Redis 不可用时回退内存。
// build: 4 并发，TTL 6 分钟（覆盖 workerRebuildTimeout 5m + 余量）。dedup
// 透传装配（infra/functions Redis 实现，组合根 wire.Bind 注入）。
func ProvideSemaphores(client *redis.Client, cfg *config.AppConfig, dedup RebuildDedup) Semaphores {
	// 配置可覆盖（预留）：functions.semaphore.build.ttl / max
	// 当前固定默认值，满足 W-F 要求。
	buildTTL := 360 * time.Second
	if client == nil {
		return Semaphores{
			Build:        semaphore.NewInMemory(maxConcurrentBuilds),
			RebuildDedup: dedup,
		}
	}
	// 允许通过 config 调整 TTL（若未来开放）；当前未进 config.proto，保持固定。
	_ = cfg
	return Semaphores{
		Build:        semaphore.NewRedis(client, "torchwood:sem:build", maxConcurrentBuilds, buildTTL),
		RebuildDedup: dedup,
	}
}
