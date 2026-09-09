package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
)

func TestRedisOneTimeTokenStore_RegisterAndConsume(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := auth.NewRedisOneTimeTokenStore(rdb)
	ctx := context.Background()

	key := "Torchwood:jwt:one-time:jti-1"
	ok, err := store.Register(ctx, key, "session-1", 5*time.Minute)
	require.NoError(t, err)
	require.True(t, ok)

	// 同一 jti 不可重复注册（碰撞守卫）。
	ok, err = store.Register(ctx, key, "session-2", 5*time.Minute)
	require.NoError(t, err)
	require.False(t, ok)

	// 原子消费：GETDEL 一次性。
	value, err := store.Consume(ctx, key)
	require.NoError(t, err)
	require.Equal(t, "session-1", value)
	value, err = store.Consume(ctx, key)
	require.NoError(t, err)
	require.Empty(t, value)

	// 过期记录视为已消费。
	ok, err = store.Register(ctx, "Torchwood:jwt:one-time:jti-2", "s", time.Second)
	require.NoError(t, err)
	require.True(t, ok)
	mr.FastForward(2 * time.Second)
	value, err = store.Consume(ctx, "Torchwood:jwt:one-time:jti-2")
	require.NoError(t, err)
	require.Empty(t, value)
}
