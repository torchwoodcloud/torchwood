package functions_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
)

// RedisRebuildDedup 单元（在途重建跨进程去重，rebuild_dedup_redis.go）：
// SETNX 互斥、键命名空间与 TTL、Release 幂等、TTL 过期解封。

func TestRedisRebuildDedup_TryAcquireRelease(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	d := infrafunctions.NewRedisRebuildDedup(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	ok, err := d.TryAcquire(ctx, "p1/fn1/dep1", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "首个调用方抢到键")
	require.Len(t, mr.Keys(), 1)
	require.Contains(t, mr.Keys()[0], "torchwood:fnrebuild:", "torchwood: 命名空间前缀")
	ttl := mr.TTL(mr.Keys()[0])
	require.Greater(t, ttl, time.Duration(0), "键带 TTL（崩溃残留兜底）")

	ok, err = d.TryAcquire(ctx, "p1/fn1/dep1", time.Minute)
	require.NoError(t, err)
	require.False(t, ok, "SETNX：在途期间其余调用方让路")

	require.NoError(t, d.Release(ctx, "p1/fn1/dep1"))
	require.NoError(t, d.Release(ctx, "p1/fn1/dep1"), "Release 幂等：重复释放不报错")
	ok, err = d.TryAcquire(ctx, "p1/fn1/dep1", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "释放后可重抢")

	mr.FastForward(2 * time.Minute)
	ok, err = d.TryAcquire(ctx, "p1/fn1/dep1", time.Minute)
	require.NoError(t, err)
	require.True(t, ok, "TTL 过期后键解封")
}

// TestRedisRebuildDedup_NonPositiveTTLClamped 非正 ttl 兜底：调用方组装
// 缺陷不得落成永不过期的键（残留键会永久封锁该部署的自愈重建）。
func TestRedisRebuildDedup_NonPositiveTTLClamped(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	d := infrafunctions.NewRedisRebuildDedup(redis.NewClient(&redis.Options{Addr: mr.Addr()}))

	ok, err := d.TryAcquire(context.Background(), "p1/fn1/dep1", 0)
	require.NoError(t, err)
	require.True(t, ok)
	ttl := mr.TTL(mr.Keys()[0])
	require.Greater(t, ttl, time.Duration(0), "非正 ttl 被钳到下限，键必带过期时间")
}
