package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// Round3 H6-4：INCR + 首次 EXPIRE 原子化——三个限流器首次计数后键必须带
// TTL（修复前 INCR 与 EXPIRE 之间的崩溃会留下无 TTL 计数键永久锁死）。
func TestRateLimiters_FirstIncrSetsTTL(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	ctx := context.Background()

	t.Run("login throttle email counter", func(t *testing.T) {
		throttle := auth.NewRedisLoginThrottle(rdb)
		require.NoError(t, throttle.RecordFailure(ctx, "ns-1", "a@b.c", "1.2.3.4", true))
		ttl, err := rdb.TTL(ctx, "Torchwood:login:fail:ns-1:email:a@b.c").Result()
		require.NoError(t, err)
		require.Greater(t, ttl, time.Duration(0), "首次失败计数必须带 TTL（窗口内滑动）")
	})

	t.Run("login throttle ip counter", func(t *testing.T) {
		throttle := auth.NewRedisLoginThrottle(rdb)
		require.NoError(t, throttle.RecordFailure(ctx, "ns-2", "a@b.c", "5.6.7.8", true))
		ttl, err := rdb.TTL(ctx, "Torchwood:login:fail:ns-2:ip:5.6.7.8").Result()
		require.NoError(t, err)
		require.Greater(t, ttl, time.Duration(0), "首次 IP 失败计数必须带 TTL")
	})

	t.Run("generic rate limiter", func(t *testing.T) {
		rl := auth.NewRedisRateLimiter(rdb)
		require.NoError(t, rl.Allow(ctx, "probe", 100, time.Minute))
		ttl, err := rdb.TTL(ctx, "Torchwood:ratelimit:probe").Result()
		require.NoError(t, err)
		require.Greater(t, ttl, time.Duration(0), "首次计数必须带 TTL（1min 窗口）")
	})

	t.Run("otp ip window", func(t *testing.T) {
		store := auth.NewRedisOTPChallengeStore(rdb, &config.AppConfig{})
		require.NoError(t, store.CheckSendRateLimit(ctx, "p-1", "a@b.c", "9.9.9.9"))
		ttl, err := rdb.TTL(ctx, "Torchwood:otp:ip:p-1:9.9.9.9").Result()
		require.NoError(t, err)
		require.Greater(t, ttl, time.Duration(0), "OTP IP 窗口首次计数必须带 TTL（1h 窗口）")
		// 同一目标二次发送被 cooldown 拦（既有语义保持）。
		err = store.CheckSendRateLimit(ctx, "p-1", "a@b.c", "9.9.9.9")
		require.Error(t, err, "60s cooldown 内重复发送必须拒绝")
	})
}
