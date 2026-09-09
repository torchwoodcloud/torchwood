package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
)

func mrValue(t *testing.T, mr *miniredis.Miniredis, key string) string {
	t.Helper()
	val, err := mr.Get(key)
	require.NoError(t, err)
	return val
}

func TestRedisRefreshRotationStore_RegisterRotateOK(t *testing.T) {
	t.Parallel()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := auth.NewRedisRefreshRotationStore(rdb)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-1", time.Hour))
	require.Equal(t, "tid-1", mrValue(t, mr, key))

	result, err := store.Rotate(ctx, key, "tid-1", "tid-2", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateOK, result)
	require.Equal(t, "tid-2", mrValue(t, mr, key))
}

func TestRedisRefreshRotationStore_RotateMismatchKeepsValue(t *testing.T) {
	t.Parallel()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := auth.NewRedisRefreshRotationStore(rdb)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-new", time.Hour))

	// Presenting the old (already rotated) token id must not overwrite the store.
	result, err := store.Rotate(ctx, key, "tid-old", "tid-attacker", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateMismatch, result)
	require.Equal(t, "tid-new", mrValue(t, mr, key))
}

func TestRedisRefreshRotationStore_RotateMissing(t *testing.T) {
	t.Parallel()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	store := auth.NewRedisRefreshRotationStore(rdb)
	result, err := store.Rotate(context.Background(), domainauth.RefreshRotationKey("proj-1", "gone"), "tid-1", "tid-2", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateMissing, result)
}
