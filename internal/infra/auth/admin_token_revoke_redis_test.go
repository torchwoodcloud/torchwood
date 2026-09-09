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

func TestRedisAdminTokenRevokeStore(t *testing.T) {
	t.Parallel()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := auth.NewRedisAdminTokenRevokeStore(rdb)
	ctx := context.Background()
	revokedAt := time.Now().Add(-time.Minute)

	require.NoError(t, store.RevokeBefore(ctx, "admin-1", revokedAt, time.Hour))
	got, err := store.RevokedBefore(ctx, "admin-1")
	require.NoError(t, err)
	require.Equal(t, revokedAt.Unix(), got.Unix())

	later := time.Now()
	require.NoError(t, store.RevokeBefore(ctx, "admin-1", later, time.Hour))
	got, err = store.RevokedBefore(ctx, "admin-1")
	require.NoError(t, err)
	require.Equal(t, later.Unix(), got.Unix())
}
