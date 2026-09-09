package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	infrastorage "github.com/torchwoodcloud/torchwood/internal/infra/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestUploadSessionStore 返回 miniredis 支撑的 UploadSessionStore（含真实 TTL 语义）。
func newTestUploadSessionStore(t *testing.T) (*miniredis.Miniredis, domainstorage.UploadSessionStore) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return mr, infrastorage.NewRedisUploadSessionStore(rdb)
}

func newUploadsUC(t *testing.T) (context.Context, *Storage, string, *miniredis.Miniredis, *testutil.MemObjectStore) {
	t.Helper()
	ctx := serverWriteCtx() // G6-4：CreateBucket 需要 Server API 写主体
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	usersRepo := bunrepo.NewUserRepository(db)
	for _, u := range []users.User{
		{ID: "user-1", Email: "user-1@torchwood.local", Status: users.StatusActive},
		{ID: "user-2", Email: "user-2@torchwood.local", Status: users.StatusActive},
	} {
		uu := u
		require.NoError(t, usersRepo.Insert(ctx, projectID, &uu))
	}

	cfg := &config.AppConfig{}
	store := testutil.NewMemObjectStore()
	mr, upStore := newTestUploadSessionStore(t)
	uc := NewStorage(cfg, bunrepo.NewProjectRepository(db), store, upStore, bunrepo.NewBucketRepository(db), bunrepo.NewFileRepository(db))
	return ctx, uc, projectID, mr, store
}

func mustCreateBucket(t *testing.T, ctx context.Context, uc *Storage, projectID string) string {
	t.Helper()
	bucket, err := uc.CreateBucket(ctx, CreateBucketCommand{ProjectID: projectID, Name: "chunk-bucket"})
	require.NoError(t, err)
	return bucket.ID
}

func uploadFullChunks(t *testing.T, ctx context.Context, uc *Storage, projectID string, session *domainstorage.UploadSession, content []byte) {
	t.Helper()
	principal := databases.Principal{Roles: []string{"keys"}}
	for i := 1; i <= session.PartCount; i++ {
		start := (i - 1) * int(session.ChunkSize)
		end := start + int(session.ChunkSize)
		if end > len(content) {
			end = len(content)
		}
		_, err := uc.UploadChunk(ctx, projectID, session.ID, i, bytes.NewReader(content[start:end]), int64(end-start), "", principal)
		require.NoError(t, err)
	}
}

// TestUploads_FullFlow 24MiB 两片全流程（16MiB + 8MiB，非末片 == chunkSize 满足
// ≥5MiB 语义）：complete → GetFile 内容一致 + mime 归一化。
func TestUploads_FullFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	content := make([]byte, 24<<20) // 24 MiB = 16MiB + 8MiB 两片
	_, err := rand.Read(content)
	require.NoError(t, err)

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "movie.mp4",
		MimeType:  "text/html", // 危险 mime → complete 后文档归一化为 octet-stream
		Size:      int64(len(content)),
	}, principal)
	require.NoError(t, err)
	require.Equal(t, 2, session.PartCount)
	require.Equal(t, int64(16<<20), session.ChunkSize)
	require.Equal(t, "application/octet-stream", session.MimeType)
	require.WithinDuration(t, time.Now().Add(24*time.Hour), session.ExpiresAt, time.Minute)

	uploadFullChunks(t, ctx, uc, projectID, session, content)

	file, err := uc.CompleteUpload(ctx, projectID, session.ID, "user-1", principal)
	require.NoError(t, err)
	require.Equal(t, session.FileID, file.ID)
	require.Equal(t, "movie.mp4", file.Name)
	require.Equal(t, "application/octet-stream", file.MimeType)
	require.Equal(t, int64(len(content)), file.Size)

	gotMeta, reader, err := uc.GetFile(ctx, projectID, bucketID, file.ID, principal)
	require.NoError(t, err)
	require.NotNil(t, gotMeta)
	defer func() { _ = reader.Close() }()
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, content, got, "合并后文件内容必须与原始文件完全一致")

	// 会话已清理：Get nil。
	s2, err := uc.GetUploadSession(ctx, projectID, session.ID, principal)
	require.Error(t, err)
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Nil(t, s2)
}

// TestUploads_ResumeAfterMissingChunks 缺片 complete → FailedPrecondition（含列表），
// 补片后 complete 成功（续传路径）。
func TestUploads_ResumeAfterMissingChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	content := make([]byte, 24<<20)
	_, err := rand.Read(content)
	require.NoError(t, err)

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "partial.bin",
		Size:      int64(len(content)),
	}, principal)
	require.NoError(t, err)

	// 只传第 1 片（16MiB）。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(content[:16<<20]), 16<<20, "", principal)
	require.NoError(t, err)

	_, err = uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "missing chunks: [2]")

	// 会话保留：查询 received 正确。
	s2, err := uc.GetUploadSession(ctx, projectID, session.ID, principal)
	require.NoError(t, err)
	require.Equal(t, map[int]bool{1: true}, s2.Received)

	// 补第 2 片（8MiB）后 complete 成功。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 2, bytes.NewReader(content[16<<20:]), 8<<20, "", principal)
	require.NoError(t, err)
	file, err := uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.NoError(t, err)
	_, reader, err := uc.GetFile(ctx, projectID, bucketID, file.ID, principal)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, content, got)
}

// TestUploads_Validation 越界/超大 chunk/非末片 size 不符/超大 size → InvalidArgument。
func TestUploads_Validation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	content := make([]byte, 24<<20)
	_, err := rand.Read(content)
	require.NoError(t, err)

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "v.bin",
		Size:      int64(len(content)),
	}, principal)
	require.NoError(t, err)

	// 分片号越界（0 与 partCount+1）。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 0, bytes.NewReader(nil), 0, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 3, bytes.NewReader(nil), 0, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 非末片 size != chunkSize（第 1 片传 1MiB）。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(content[:1<<20]), 1<<20, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "non-final")

	// 超大 chunk（> 16MiB）。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(content[:16<<20]), 16<<20, "", principal)
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 2, bytes.NewReader(content[16<<20:]), (8<<20)+(16<<20), "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "exceeds maximum size")

	// 末片越界 size（0 与 > chunkSize）。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 2, bytes.NewReader(nil), 0, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 2, bytes.NewReader(nil), (16<<20)+1, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 大小 < 5MiB 的末片合法（5MiB 约束仅对非末片）。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 2, bytes.NewReader(content[16<<20:16<<20+100]), 100, "", principal)
	require.NoError(t, err)

	// size > MaxUploadSize → InvalidArgument。
	_, err = uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "huge.bin",
		Size:      domainstorage.MaxUploadSize + 1,
	}, principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "file too large")

	// size ≤ 0 → InvalidArgument。
	_, err = uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "empty.bin",
		Size:      0,
	}, principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestUploads_SessionNotFoundAndExpired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, mr, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	// 不存在 → NotFound。
	_, err := uc.GetUploadSession(ctx, projectID, "nope", principal)
	require.Equal(t, codes.NotFound, status.Code(err))

	// 过期 → NotFound（FastForward 25h）。
	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "expire.bin",
		Size:      1 << 20,
	}, principal)
	require.NoError(t, err)
	mr.FastForward(25 * time.Hour)
	_, err = uc.GetUploadSession(ctx, projectID, session.ID, principal)
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "", principal)
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestUploads_AbortCleansSessionAndChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, objStore := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "abort.bin",
		Size:      12 << 20,
	}, principal)
	require.NoError(t, err)
	uploadFullChunks(t, ctx, uc, projectID, session, make([]byte, 12<<20))

	require.NoError(t, uc.AbortUpload(ctx, projectID, session.ID, "", principal))

	// 会话已删。
	_, err = uc.GetUploadSession(ctx, projectID, session.ID, principal)
	require.Equal(t, codes.NotFound, status.Code(err))

	// 分片对象已清。
	for i := 1; i <= session.PartCount; i++ {
		_, err := objStore.Get(ctx, domainstorage.DefaultBucketName, chunkKey(projectID, bucketID, session.FileID, i))
		require.Error(t, err, "abort 后分片对象应删除")
	}
}

func TestUploads_DuplicateChunkIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "dup.bin",
		Size:      6 << 20,
	}, principal)
	require.NoError(t, err)

	first := bytes.Repeat([]byte("a"), 6<<20)
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(first), 6<<20, "", principal)
	require.NoError(t, err)
	// 同号覆盖（幂等）：第二次上传不同内容，最终以最后一次为准。
	second := bytes.Repeat([]byte("b"), 6<<20)
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(second), 6<<20, "", principal)
	require.NoError(t, err)

	file, err := uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.NoError(t, err)
	_, reader, err := uc.GetFile(ctx, projectID, bucketID, file.ID, principal)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, second, got)
}

func TestUploads_CompleteMutex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "mutex.bin",
		Size:      6 << 20,
	}, principal)
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(make([]byte, 6<<20)), 6<<20, "", principal)
	require.NoError(t, err)

	// 先手动持有锁 → CompleteUpload 返回 FailedPrecondition。
	holdToken, locked, err := uc.uploads.LockComplete(ctx, session.ID)
	require.NoError(t, err)
	require.True(t, locked)
	_, err = uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "already being completed")

	// 释放锁后 complete 成功（compare-and-del 需持锁 token）。
	require.NoError(t, uc.uploads.UnlockComplete(ctx, session.ID, holdToken))
	file, err := uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.NoError(t, err)
	require.Equal(t, session.FileID, file.ID)
}

func TestUploads_ProjectMismatchDenied(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "cross.bin",
		Size:      1 << 20,
	}, principal)
	require.NoError(t, err)

	// 另一项目 ID 访问 → PermissionDenied（纵深防御）。
	_, err = uc.GetUploadSession(ctx, "other-project", session.ID, principal)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = uc.UploadChunk(ctx, "other-project", session.ID, 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "", principal)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = uc.CompleteUpload(ctx, "other-project", session.ID, "", principal)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// TestUploads_OwnerBinding 会话 owner 校验：非 owner 端用户被拒；owner 本人通过；
// keys/admin 豁免；API key 创建的空 owner 会话不受约束。
func TestUploads_OwnerBinding(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, _ := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)

	// 端用户（owner="user-1"）创建会话。
	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID:   projectID,
		BucketID:    bucketID,
		Name:        "owner.bin",
		Size:        1 << 20,
		OwnerUserID: "user-1",
	}, databases.Principal{Roles: []string{"users", "user:user-1"}})
	require.NoError(t, err)
	require.Equal(t, "user-1", session.OwnerUserID)

	// 非 owner 端用户：UploadChunk/Complete/Abort 全部 PermissionDenied。
	other := databases.Principal{Roles: []string{"users", "user:user-2"}}
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "user-2", other)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = uc.CompleteUpload(ctx, projectID, session.ID, "user-2", other)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	abortErr := uc.AbortUpload(ctx, projectID, session.ID, "user-2", other)
	require.Equal(t, codes.PermissionDenied, status.Code(abortErr))

	// 会话保留（非 owner 拒绝不破坏会话）。
	_, err = uc.GetUploadSession(ctx, projectID, session.ID, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)

	// owner 本人可上传/complete。
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "user-1", databases.Principal{Roles: []string{"users", "user:user-1"}})
	require.NoError(t, err)
	_, err = uc.CompleteUpload(ctx, projectID, session.ID, "user-1", databases.Principal{Roles: []string{"users", "user:user-1"}})
	require.NoError(t, err)

	// API key（owner 空）创建的会话不受 owner 约束：端用户以 keys 角色可操作。
	keySession, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "key.bin",
		Size:      1 << 20,
	}, databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Empty(t, keySession.OwnerUserID)
	_, err = uc.UploadChunk(ctx, projectID, keySession.ID, 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "some-user", databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.NoError(t, uc.AbortUpload(ctx, projectID, keySession.ID, "some-user", databases.Principal{Roles: []string{"keys"}}))

	// keys 豁免：他人创建的会话 keys 角色可操作。
	session2, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID:   projectID,
		BucketID:    bucketID,
		Name:        "keys-exempt.bin",
		Size:        1 << 20,
		OwnerUserID: "user-9",
	}, databases.Principal{Roles: []string{"users", "user:user-9"}})
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, projectID, session2.ID, 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "", databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
}
