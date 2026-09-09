package storage

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/storage"
)

func newRedisUploadSessionTestStore(t *testing.T) (*miniredis.Miniredis, storage.UploadSessionStore) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, NewRedisUploadSessionStore(rdb)
}

func sampleUploadSession() *storage.UploadSession {
	return &storage.UploadSession{
		ID:          "upload-1",
		ProjectID:   "project-1",
		BucketID:    "bucket-1",
		FileID:      "file-1",
		OwnerUserID: "user-1",
		Name:        "movie.mp4",
		MimeType:    "video/mp4",
		Size:        12 << 20,
		Metadata:    map[string]string{"k": "v"},
		Permissions: []string{"read:any", "update:keys"},
		ChunkSize:   16 << 20,
		PartCount:   2,
		Received:    map[int]bool{},
		CreatedAt:   time.Now().Add(-time.Minute),
		ExpiresAt:   time.Now().Add(storage.UploadSessionTTL),
	}
}

func TestRedisUploadSession_CreateGetRoundtrip(t *testing.T) {
	mr, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	want := sampleUploadSession()
	require.NoError(t, store.Create(ctx, want))

	got, err := store.Get(ctx, want.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, want.ID, got.ID)
	require.Equal(t, want.ProjectID, got.ProjectID)
	require.Equal(t, want.BucketID, got.BucketID)
	require.Equal(t, want.FileID, got.FileID)
	require.Equal(t, want.OwnerUserID, got.OwnerUserID)
	require.Equal(t, want.Name, got.Name)
	require.Equal(t, want.MimeType, got.MimeType)
	require.Equal(t, want.Size, got.Size)
	require.Equal(t, want.ChunkSize, got.ChunkSize)
	require.Equal(t, want.PartCount, got.PartCount)
	require.Equal(t, want.Metadata, got.Metadata)
	require.Equal(t, want.Permissions, got.Permissions)
	require.WithinDuration(t, want.CreatedAt, got.CreatedAt, time.Second)
	require.WithinDuration(t, want.ExpiresAt, got.ExpiresAt, time.Second)
	require.Empty(t, got.Received)

	// 不存在的会话 → nil, nil。
	missing, err := store.Get(ctx, "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	// TTL 24h。
	ttl := mr.TTL(uploadSessionKey(want.ID))
	require.InDelta(t, float64(storage.UploadSessionTTL), float64(ttl), float64(time.Minute))
}

func TestRedisUploadSession_MarkChunkIdempotentAndTTLRefresh(t *testing.T) {
	mr, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	up := sampleUploadSession()
	require.NoError(t, store.Create(ctx, up))

	require.NoError(t, store.MarkChunk(ctx, up.ID, 1))
	require.NoError(t, store.MarkChunk(ctx, up.ID, 1))

	got, err := store.Get(ctx, up.ID)
	require.NoError(t, err)
	require.Equal(t, map[int]bool{1: true}, got.Received)

	// TTL 刷新：前进 12h 后 MarkChunk 恢复为满 TTL（未刷新则会过期）。
	mr.FastForward(12 * time.Hour)
	require.NoError(t, store.MarkChunk(ctx, up.ID, 2))
	ttl := mr.TTL(uploadSessionKey(up.ID))
	require.InDelta(t, float64(storage.UploadSessionTTL), float64(ttl), float64(time.Minute),
		"MarkChunk 应刷新 TTL 至满 24h")

	got, err = store.Get(ctx, up.ID)
	require.NoError(t, err)
	require.Equal(t, map[int]bool{1: true, 2: true}, got.Received)
}

func TestRedisUploadSession_MarkChunkNoOrphanAfterDelete(t *testing.T) {
	_, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	up := sampleUploadSession()
	require.NoError(t, store.Create(ctx, up))
	require.NoError(t, store.Delete(ctx, up.ID))

	// 会话已删：MarkChunk 不得重建孤儿 parts key。
	require.NoError(t, store.MarkChunk(ctx, up.ID, 1))
	got, err := store.Get(ctx, up.ID)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRedisUploadSession_Expiry(t *testing.T) {
	mr, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	up := sampleUploadSession()
	require.NoError(t, store.Create(ctx, up))

	mr.FastForward(25 * time.Hour)
	got, err := store.Get(ctx, up.ID)
	require.NoError(t, err)
	require.Nil(t, got, "TTL 过期后会话应不存在")
}

func TestRedisUploadSession_Delete(t *testing.T) {
	_, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	up := sampleUploadSession()
	require.NoError(t, store.Create(ctx, up))
	require.NoError(t, store.MarkChunk(ctx, up.ID, 1))
	require.NoError(t, store.Delete(ctx, up.ID))

	got, err := store.Get(ctx, up.ID)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestRedisUploadSession_CountChunks(t *testing.T) {
	_, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	up := sampleUploadSession()
	require.NoError(t, store.Create(ctx, up))

	// 未上传任何分片 → 0。
	n, err := store.CountChunks(ctx, up.ID)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// SADD 后计数准确（含重复标记幂等）。
	require.NoError(t, store.MarkChunk(ctx, up.ID, 1))
	require.NoError(t, store.MarkChunk(ctx, up.ID, 1))
	require.NoError(t, store.MarkChunk(ctx, up.ID, 2))
	n, err = store.CountChunks(ctx, up.ID)
	require.NoError(t, err)
	require.Equal(t, 2, n, "重复标记不重复计数")

	// 会话删除后 → 0。
	require.NoError(t, store.Delete(ctx, up.ID))
	n, err = store.CountChunks(ctx, up.ID)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	// 不存在的会话 → 0。
	n, err = store.CountChunks(ctx, "nope")
	require.NoError(t, err)
	require.Equal(t, 0, n)
}

func TestRedisUploadSession_LockCompleteMutex(t *testing.T) {
	_, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	// 会话不存在也可加锁（锁与会话生命周期独立）；返回随机 token。
	token, locked, err := store.LockComplete(ctx, "upload-lock")
	require.NoError(t, err)
	require.True(t, locked)
	require.NotEmpty(t, token)

	// 第二次加锁互斥。
	_, locked, err = store.LockComplete(ctx, "upload-lock")
	require.NoError(t, err)
	require.False(t, locked)

	// 锁持有者二次确认通过；非持有者 token 被拒绝。
	owner, err := store.IsLockOwner(ctx, "upload-lock", token)
	require.NoError(t, err)
	require.True(t, owner)
	owner, err = store.IsLockOwner(ctx, "upload-lock", "wrong-token")
	require.NoError(t, err)
	require.False(t, owner)

	// 释放后可再次加锁。
	require.NoError(t, store.UnlockComplete(ctx, "upload-lock", token))
	owner, err = store.IsLockOwner(ctx, "upload-lock", token)
	require.NoError(t, err)
	require.False(t, owner, "锁释放后原 token 不再持有")
	token2, locked, err := store.LockComplete(ctx, "upload-lock")
	require.NoError(t, err)
	require.True(t, locked)
	require.NotEqual(t, token, token2, "每次加锁生成新 token")
}

// J2-3/E-P1-2：UnlockComplete 是 Lua compare-and-del——非持有者 token 不得删除
// 锁，仅持有者 token 释放生效（防止误删已被其他 complete 重新持有的锁）。
func TestRedisUploadSession_UnlockCompleteCompareAndDel(t *testing.T) {
	_, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	token, locked, err := store.LockComplete(ctx, "upload-cad")
	require.NoError(t, err)
	require.True(t, locked)

	// 错误 token：锁保持持有、仍互斥。
	require.NoError(t, store.UnlockComplete(ctx, "upload-cad", "wrong-token"))
	owner, err := store.IsLockOwner(ctx, "upload-cad", token)
	require.NoError(t, err)
	require.True(t, owner, "非持有者 token 释放不得删除锁")
	_, locked, err = store.LockComplete(ctx, "upload-cad")
	require.NoError(t, err)
	require.False(t, locked, "错误 token 释放后锁仍应互斥")

	// 正确 token：释放生效，可重新加锁。
	require.NoError(t, store.UnlockComplete(ctx, "upload-cad", token))
	token2, locked, err := store.LockComplete(ctx, "upload-cad")
	require.NoError(t, err)
	require.True(t, locked)
	require.NotEqual(t, token, token2)

	// 锁 key 不存在时释放为幂等 no-op（GET 返回 nil ≠ token → 不删除）。
	require.NoError(t, store.UnlockComplete(ctx, "upload-missing", token2))
}

// J2-1/E-P1-2：释放路径的 WithoutCancel 在调用方（internal/app/storage/uploads.go
// 的 defer）构造；此处验证 compare-and-del 脚本在已取消 ctx 替换为独立 ctx 后
// 正常生效（DEL 尊重 ctx，取消的 ctx 会失败——正是调用方 detach 的原因）。
func TestRedisUploadSession_UnlockCompleteWithDetachedContext(t *testing.T) {
	_, store := newRedisUploadSessionTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	token, locked, err := store.LockComplete(ctx, "upload-detach")
	require.NoError(t, err)
	require.True(t, locked)

	// 已取消的请求 ctx：DEL/EVAL 失败，锁保留（复现事故路径）。
	cancel()
	require.Error(t, store.UnlockComplete(ctx, "upload-detach", token))
	owner, err := store.IsLockOwner(context.Background(), "upload-detach", token)
	require.NoError(t, err)
	require.True(t, owner, "ctx 取消时释放失败，锁应保留")

	// WithoutCancel 重建的独立 ctx（调用方 defer 的做法）：释放成功。
	detached, dcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer dcancel()
	require.NoError(t, store.UnlockComplete(detached, "upload-detach", token))
	owner, err = store.IsLockOwner(context.Background(), "upload-detach", token)
	require.NoError(t, err)
	require.False(t, owner, "独立 ctx 下 compare-and-del 应删除锁")
}

func TestRedisUploadSession_LockCompleteTTLExpiry(t *testing.T) {
	mr, store := newRedisUploadSessionTestStore(t)
	ctx := context.Background()

	token, ok, err := store.LockComplete(ctx, "upload-lock-ttl")
	require.NoError(t, err)
	require.True(t, ok)

	mr.FastForward(2 * time.Hour)
	// 锁 TTL 1h 已过期 → 原 token 不再是持有者，可重新获取。
	owner, err := store.IsLockOwner(ctx, "upload-lock-ttl", token)
	require.NoError(t, err)
	require.False(t, owner, "锁 TTL 过期后原 token 失效")
	_, ok, err = store.LockComplete(ctx, "upload-lock-ttl")
	require.NoError(t, err)
	require.True(t, ok, "锁 TTL 1h 过期后应可重新获取")
}
