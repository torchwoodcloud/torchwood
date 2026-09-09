package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRedisRateLimiter_WindowBudget(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	limiter := auth.NewRedisRateLimiter(rdb)
	ctx := context.Background()

	for i := 0; i < 20; i++ {
		require.NoError(t, limiter.Allow(ctx, "anonymous:ip:203.0.113.1", 20, time.Hour))
	}
	err = limiter.Allow(ctx, "anonymous:ip:203.0.113.1", 20, time.Hour)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.ResourceExhausted, st.Code())

	// 其他 IP 不受影响。
	require.NoError(t, limiter.Allow(ctx, "anonymous:ip:203.0.113.2", 20, time.Hour))

	// 窗口过期后恢复。
	mr.FastForward(time.Hour + time.Second)
	require.NoError(t, limiter.Allow(ctx, "anonymous:ip:203.0.113.1", 20, time.Hour))
}

// TestRedisRateLimiter_RetryInfoDetail：超限错误携带 google.rpc.RetryInfo
// detail 且 RetryDelay 为窗口剩余秒数（Round4 J3-6）。
func TestRedisRateLimiter_RetryInfoDetail(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	limiter := auth.NewRedisRateLimiter(rdb)
	ctx := context.Background()
	const window = time.Hour

	for i := 0; i < 5; i++ {
		require.NoError(t, limiter.Allow(ctx, "api:apikey:k1", 5, window))
	}
	mr.FastForward(10 * time.Minute) // 消耗部分窗口

	err = limiter.Allow(ctx, "api:apikey:k1", 5, window)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.ResourceExhausted, st.Code())

	var ri *errdetails.RetryInfo
	for _, d := range st.Details() {
		if r, isRetry := d.(*errdetails.RetryInfo); isRetry {
			ri = r
		}
	}
	require.NotNil(t, ri, "超限错误应携带 RetryInfo detail")
	retryAfter := ri.GetRetryDelay().AsDuration()
	require.Greater(t, retryAfter, window-15*time.Minute, "剩余窗口应扣除已消耗时间")
	require.LessOrEqual(t, retryAfter, window-10*time.Minute)
}

func TestRedisRateLimiter_EmptyKeyNoop(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	limiter := auth.NewRedisRateLimiter(rdb)

	for i := 0; i < 30; i++ {
		require.NoError(t, limiter.Allow(context.Background(), "", 1, time.Minute))
	}
}
