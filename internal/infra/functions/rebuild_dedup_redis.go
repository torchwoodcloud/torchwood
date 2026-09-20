package functions

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// rebuildDedupKeyPrefix 是在途重建去重键前缀（torchwood: 命名空间与
// exec-token / sem:build / fnq 同款；key 由 app 层拼逻辑标识
// project/function/deployment）。
const rebuildDedupKeyPrefix = "torchwood:fnrebuild:"

// rebuildDedupMinTTL 是 TTL 下限兜底：调用方传非正 ttl（组装缺陷）不得
// 落成永不过期的键——残留键会永久封锁该部署的自愈重建。
const rebuildDedupMinTTL = time.Minute

// RedisRebuildDedup 是 RebuildDedup 端口的 Redis 实现（在途重建跨进程
// 去重）：SETNX + TTL 抢占，DEL 释放。键残留兜底靠 TTL（构建超时预算 +
// 余量，由调用方组装），崩溃未释放的键到期自然解封。
type RedisRebuildDedup struct {
	rdb *redis.Client
}

func NewRedisRebuildDedup(rdb *redis.Client) *RedisRebuildDedup {
	return &RedisRebuildDedup{rdb: rdb}
}

// TryAcquire 原子抢占去重键（SETNX）：true = 抢到（本副本负责重建），
// false = 已有在途（其他副本接管，让路）。Redis 故障原样上抛——fail-open
// 裁决在 app 层（去重是效率优化，不阻断自愈链）。
func (d *RedisRebuildDedup) TryAcquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if ttl < rebuildDedupMinTTL {
		ttl = rebuildDedupMinTTL
	}
	return d.rdb.SetNX(ctx, rebuildDedupKeyPrefix+key, 1, ttl).Result()
}

// Release 主动释放去重键（构建结束无论成败都须释放；DEL 对不存在的键
// 幂等）。极小竞态窗口：TTL 过期后他副本重抢、本副本迟到的 DEL 会误删
// 新键——后果只是多一次幂等重建（终态收敛 ready），不为对称性引入
// token/Lua 复杂度。
func (d *RedisRebuildDedup) Release(ctx context.Context, key string) error {
	return d.rdb.Del(ctx, rebuildDedupKeyPrefix+key).Err()
}
