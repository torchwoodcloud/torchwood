package client

import (
	"context"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	infraauth "github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCheckAnonymousSessionRateLimit(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	a := &Account{rateLimiter: infraauth.NewRedisRateLimiter(rdb)}
	ctx := context.Background()

	for i := 0; i < anonymousSessionIPLimit; i++ {
		require.NoError(t, a.checkAnonymousSessionRateLimit(ctx, "203.0.113.1"))
	}
	err = a.checkAnonymousSessionRateLimit(ctx, "203.0.113.1")
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.ResourceExhausted, st.Code())

	// 其他 IP 不受影响。
	require.NoError(t, a.checkAnonymousSessionRateLimit(ctx, "203.0.113.2"))

	// 空 IP fail-closed（M5 C8）：无客户端 IP 无法记账，不再静默放行。
	err = a.checkAnonymousSessionRateLimit(ctx, "")
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
}

// M5 C8：限流器未装配时 fail-closed（匿名注册无频控即不应开放），
// 不再 nil 容忍静默放行。
func TestCheckAnonymousSessionRateLimit_NilLimiterFailsClosed(t *testing.T) {
	t.Parallel()

	a := &Account{}
	err := a.checkAnonymousSessionRateLimit(context.Background(), "203.0.113.1")
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
}
