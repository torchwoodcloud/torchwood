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

func TestRedisNonceStore_PutGetConsume(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	store := auth.NewRedisNonceStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	require.NoError(t, store.Put(ctx, "k1", "v1", time.Minute))
	got, err := store.Get(ctx, "k1")
	require.NoError(t, err)
	require.Equal(t, "v1", got)

	got, err = store.Consume(ctx, "k1")
	require.NoError(t, err)
	require.Equal(t, "v1", got)
	got, err = store.Consume(ctx, "k1")
	require.NoError(t, err)
	require.Empty(t, got)

	ok, err := store.PutNX(ctx, "k2", "a", time.Minute)
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = store.PutNX(ctx, "k2", "b", time.Minute)
	require.NoError(t, err)
	require.False(t, ok)

	require.NoError(t, store.Put(ctx, "k3", "ttl", time.Second))
	mr.FastForward(2 * time.Second)
	got, err = store.Get(ctx, "k3")
	require.NoError(t, err)
	require.Empty(t, got)
}
