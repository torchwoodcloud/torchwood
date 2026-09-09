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

func newRotationStore(t *testing.T) (*miniredis.Miniredis, *auth.RedisRefreshRotationStore) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	return mr, auth.NewRedisRefreshRotationStore(rdb)
}

func TestRedisRefreshRotationStore_RegisterRotateOK(t *testing.T) {
	t.Parallel()
	mr, store := newRotationStore(t)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-1", time.Hour))
	require.Equal(t, "tid-1", mrValue(t, mr, key))

	result, currentID, err := store.Rotate(ctx, key, "tid-1", "tid-2", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateOK, result)
	require.Empty(t, currentID)
	require.Equal(t, "tid-2", mrValue(t, mr, key))
	// 旧 current 降级进宽限槽,窗口内仍可被识别。
	require.Equal(t, "tid-1", mrValue(t, mr, key+":prev"))
}

func TestRedisRefreshRotationStore_GraceReuseKeepsChain(t *testing.T) {
	t.Parallel()
	mr, store := newRotationStore(t)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	// 正常轮换 X -> Y 后,另一持有方仍拿着 X 刷新(多标签页竞态/响应丢失重试):
	// 宽限命中,链不推进,回传当前 id Y。
	require.NoError(t, store.Register(ctx, key, "tid-x", time.Hour))
	result, _, err := store.Rotate(ctx, key, "tid-x", "tid-y", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateOK, result)

	result, currentID, err := store.Rotate(ctx, key, "tid-x", "tid-z", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateGraceReuse, result)
	require.Equal(t, "tid-y", currentID)
	require.Equal(t, "tid-y", mrValue(t, mr, key))
	require.Equal(t, "tid-x", mrValue(t, mr, key+":prev"))
}

func TestRedisRefreshRotationStore_GraceWindowExpires(t *testing.T) {
	t.Parallel()
	mr, store := newRotationStore(t)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-x", time.Hour))
	_, _, err := store.Rotate(ctx, key, "tid-x", "tid-y", time.Hour)
	require.NoError(t, err)

	// 宽限窗口过后,旧 id 恢复判重用(防重放语义保留)。
	mr.FastForward(auth.GraceWindow + time.Second)
	result, currentID, err := store.Rotate(ctx, key, "tid-x", "tid-z", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateMismatch, result)
	require.Empty(t, currentID)
	require.Equal(t, "tid-y", mrValue(t, mr, key))
}

func TestRedisRefreshRotationStore_RegisterClearsGrace(t *testing.T) {
	t.Parallel()
	mr, store := newRotationStore(t)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-x", time.Hour))
	_, _, err := store.Rotate(ctx, key, "tid-x", "tid-y", time.Hour)
	require.NoError(t, err)

	// 新登录开新链并清宽限槽:改密/重登后旧链 id 不得再续签。
	require.NoError(t, store.Register(ctx, key, "tid-new", time.Hour))
	result, currentID, err := store.Rotate(ctx, key, "tid-x", "tid-z", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateMismatch, result)
	require.Empty(t, currentID)
	require.Equal(t, "tid-new", mrValue(t, mr, key))
}

func TestRedisRefreshRotationStore_RegisterSameIDKeepsGrace(t *testing.T) {
	t.Parallel()
	mr, store := newRotationStore(t)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-x", time.Hour))
	_, _, err := store.Rotate(ctx, key, "tid-x", "tid-y", time.Hour)
	require.NoError(t, err)

	// 轮换签发路径的幂等 Register(签发 jti == 当前值,对应宽限续签/轮换签发)
	// 不得清宽限槽,否则宽限语义失效。
	require.NoError(t, store.Register(ctx, key, "tid-y", time.Hour))
	result, currentID, err := store.Rotate(ctx, key, "tid-x", "tid-z", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateGraceReuse, result)
	require.Equal(t, "tid-y", currentID)
	require.Equal(t, "tid-y", mrValue(t, mr, key))
}

func TestRedisRefreshRotationStore_RotateMismatchKeepsValue(t *testing.T) {
	t.Parallel()
	mr, store := newRotationStore(t)
	ctx := context.Background()
	key := domainauth.RefreshRotationKey("proj-1", "sess-1")

	require.NoError(t, store.Register(ctx, key, "tid-new", time.Hour))

	// Presenting the old (already rotated) token id must not overwrite the store.
	result, currentID, err := store.Rotate(ctx, key, "tid-old", "tid-attacker", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateMismatch, result)
	require.Empty(t, currentID)
	require.Equal(t, "tid-new", mrValue(t, mr, key))
}

func TestRedisRefreshRotationStore_RotateMissing(t *testing.T) {
	t.Parallel()
	_, store := newRotationStore(t)
	result, currentID, err := store.Rotate(context.Background(), domainauth.RefreshRotationKey("proj-1", "gone"), "tid-1", "tid-2", time.Hour)
	require.NoError(t, err)
	require.Equal(t, domainauth.RotateMissing, result)
	require.Empty(t, currentID)
}
