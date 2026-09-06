package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	domainstorage "github.com/torchwooddev/torchwood/internal/domain/storage"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// stubProjectRepo 是 projects.Repository 的最小桩（仅 GetProject 可用，
// 其余方法嵌入接口后调用即 panic——本文件测试路径不会触及）。
type stubProjectRepo struct {
	projects.Repository
	p *projects.Project
}

func (r *stubProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if r.p == nil || r.p.ID != id {
		return nil, nil
	}
	return r.p, nil
}

// failingStore 包装内存对象存储，可按需注入 EnsureBucket/Put/Delete 失败；
// 未注入错误时委托给底层实现。
type failingStore struct {
	*testutil.MemObjectStore
	ensureErr error
	putErr    error
	deleteErr error
}

func (s *failingStore) EnsureBucket(ctx context.Context, name string) error {
	if s.ensureErr != nil {
		return s.ensureErr
	}
	return s.MemObjectStore.EnsureBucket(ctx, name)
}

func (s *failingStore) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, contentType string) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.MemObjectStore.Put(ctx, bucket, key, data, size, contentType)
}

func (s *failingStore) Delete(ctx context.Context, bucket, key string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	return s.MemObjectStore.Delete(ctx, bucket, key)
}

// newCreateFileUnitUC 组装纯内存 Storage（无 Postgres/MinIO），CreateFile
// 路径所需依赖全部为桩。
type memBucketRepo struct {
	byID map[string]*domainstorage.Bucket
}

func (r *memBucketRepo) Insert(context.Context, string, *domainstorage.Bucket) error { return nil }
func (r *memBucketRepo) GetByID(_ context.Context, _, id string) (*domainstorage.Bucket, error) {
	if r.byID == nil {
		return &domainstorage.Bucket{ID: id}, nil
	}
	return r.byID[id], nil
}
func (r *memBucketRepo) List(context.Context, string) ([]*domainstorage.Bucket, error) {
	return nil, nil
}
func (r *memBucketRepo) Count(context.Context, string) (int64, error) { return 0, nil }
func (r *memBucketRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *memBucketRepo) Delete(context.Context, string, string) error { return nil }

type memFileRepo struct {
	created []string
	deleted []string
}

func (r *memFileRepo) Insert(_ context.Context, _ string, file *domainstorage.File) error {
	r.created = append(r.created, file.ID)
	return nil
}
func (r *memFileRepo) GetByID(context.Context, string, string) (*domainstorage.File, error) {
	return nil, nil
}
func (r *memFileRepo) ListByBucket(context.Context, string, string) ([]*domainstorage.File, error) {
	return nil, nil
}
func (r *memFileRepo) Count(context.Context, string) (int64, error) { return 0, nil }
func (r *memFileRepo) Update(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *memFileRepo) Delete(_ context.Context, _, id string) error {
	r.deleted = append(r.deleted, id)
	return nil
}
func (r *memFileRepo) SumSize(context.Context, string) (int64, error) { return 0, nil }

func newCreateFileUnitUC(store *failingStore, files *memFileRepo) *Storage {
	return &Storage{
		cfg: &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{
			ID: "p1", Name: "p1", InternalID: 1,
		}},
		store:   store,
		buckets: &memBucketRepo{},
		files:   files,
	}
}

func keysPrincipal() databases.Principal { return databases.Principal{Roles: []string{"keys"}} }

// G6-2/R07-P1-2：EnsureBucket 失败发生在创建文档之前 → 不得产生孤儿文档。
func TestCreateFile_EnsureBucketFailure_NoOrphanDocument(t *testing.T) {
	files := &memFileRepo{}
	uc := newCreateFileUnitUC(&failingStore{
		MemObjectStore: testutil.NewMemObjectStore(),
		ensureErr:      errors.New("s3 down"),
	}, files)

	_, err := uc.CreateFile(context.Background(), CreateFileCommand{
		ProjectID: "p1", BucketID: "b1", Name: "a.txt", MimeType: "text/plain",
	}, bytes.NewReader([]byte("x")), 1, keysPrincipal())
	require.Error(t, err)
	require.ErrorContains(t, err, "ensure storage bucket")
	require.Empty(t, files.created, "EnsureBucket 失败时不得创建文件元数据")
	require.Empty(t, files.deleted)
}

func TestCreateFile_PutFailure_RollsBackDocument(t *testing.T) {
	files := &memFileRepo{}
	uc := newCreateFileUnitUC(&failingStore{
		MemObjectStore: testutil.NewMemObjectStore(),
		putErr:         errors.New("upload failed"),
	}, files)

	_, err := uc.CreateFile(context.Background(), CreateFileCommand{
		ProjectID: "p1", BucketID: "b1", Name: "a.txt", MimeType: "text/plain",
	}, bytes.NewReader([]byte("x")), 1, keysPrincipal())
	require.Error(t, err)
	require.ErrorContains(t, err, "upload file")
	require.Len(t, files.created, 1, "Put 前元数据已创建")
	require.Equal(t, files.created, files.deleted, "Put 失败后元数据必须回滚删除")
}

func TestCreateFile_Success(t *testing.T) {
	files := &memFileRepo{}
	memStore := testutil.NewMemObjectStore()
	uc := newCreateFileUnitUC(&failingStore{MemObjectStore: memStore}, files)

	file, err := uc.CreateFile(context.Background(), CreateFileCommand{
		ProjectID: "p1", BucketID: "b1", Name: "a.txt", MimeType: "text/plain",
	}, bytes.NewReader([]byte("hello")), 5, keysPrincipal())
	require.NoError(t, err)
	require.NotEmpty(t, file.ID)
	require.Len(t, files.created, 1)
	require.Empty(t, files.deleted)
	reader, err := memStore.Get(context.Background(), domainstorage.DefaultBucketName, objectKey("p1", "b1", file.ID))
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	got := make([]byte, 5)
	_, err = reader.Read(got)
	require.NoError(t, err)
	require.Equal(t, "hello", string(got))
}

// G6-4/R06-P1：CreateBucket use-case 守卫对齐 CreateUser（RequireServerPrincipal）：
// 匿名 Unauthenticated、端用户 PermissionDenied；console admin / API key 主体放行
// 进入业务校验（空 name → InvalidArgument 证明守卫已过）。
func TestCreateBucket_RequiresServerWriteActor(t *testing.T) {
	uc := NewStorage(&config.AppConfig{}, nil, nil, nil, nil, nil)

	_, err := uc.CreateBucket(context.Background(), CreateBucketCommand{ProjectID: "p1", Name: "b"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	userCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "u1", ActorKind: shared.ActorKindEndUser, UserID: "u1",
	})
	_, err = uc.CreateBucket(userCtx, CreateBucketCommand{ProjectID: "p1", Name: "b"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	adminCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "a1", ActorKind: shared.ActorKindAdmin, UserID: "a1", Roles: []string{"member"},
	})
	_, err = uc.CreateBucket(adminCtx, CreateBucketCommand{ProjectID: "p1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "member admin 放行进入业务校验")

	keyCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "k1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
	})
	_, err = uc.CreateBucket(keyCtx, CreateBucketCommand{ProjectID: "p1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "API key 主体放行进入业务校验")
}

// markBeforeLockStore 包装 UploadSessionStore：在 LockComplete 之前模拟并发
// UploadChunk 补传分片（复现 G6-6/R07-P2-4 的锁前快照过期场景）。
type markBeforeLockStore struct {
	domainstorage.UploadSessionStore
	mark func()
}

func (m *markBeforeLockStore) LockComplete(ctx context.Context, uploadID string) (string, bool, error) {
	m.mark()
	return m.UploadSessionStore.LockComplete(ctx, uploadID)
}

// G6-6/R07-P2-4：CompleteUpload 加锁成功后必须重新读取会话再判缺片——
// 初次快照（锁前）缺片、加锁前其他 goroutine 已补传时，旧代码误报 missing
// chunks，新代码基于锁内最新状态完成。
func TestUploads_CompleteUpload_RevalidatesSessionAfterLock(t *testing.T) {
	_, upStore := newTestUploadSessionStore(t)
	ctx := context.Background()
	session := &domainstorage.UploadSession{
		ID: "up1", ProjectID: "p1", BucketID: "b1", FileID: "f1",
		Name: "x.bin", Size: 1 << 20, ChunkSize: 1 << 20, PartCount: 1,
		Received: map[int]bool{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(ctx, session))

	files := &memFileRepo{}
	store := testutil.NewMemObjectStore()
	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       store,
		buckets:     &memBucketRepo{},
		files:       files,
		uploads: &markBeforeLockStore{
			UploadSessionStore: upStore,
			// 模拟并发 UploadChunk：加锁前一刻既标记 Redis 会话又上传分片对象。
			mark: func() {
				_ = upStore.MarkChunk(ctx, "up1", 1)
				_ = store.Put(ctx, domainstorage.DefaultBucketName,
					chunkKey("p1", "b1", "f1", 1), bytes.NewReader(make([]byte, 1<<20)), 1<<20, "")
			},
		},
	}

	file, err := uc.CompleteUpload(ctx, "p1", "up1", "", keysPrincipal())
	require.NoError(t, err)
	require.Equal(t, "f1", file.ID)
	require.Len(t, files.created, 1, "锁内重新读会话后不再误报缺片，文件行创建成功")
}

// G6-6/R07-P2-5：AbortUpload 在 complete 锁被占用时返回 FailedPrecondition 且
// 不删除会话/分片（提示重试）。
func TestUploads_AbortUpload_RejectedWhileCompleting(t *testing.T) {
	_, upStore := newTestUploadSessionStore(t)
	ctx := context.Background()
	session := &domainstorage.UploadSession{
		ID: "up2", ProjectID: "p1", BucketID: "b1", FileID: "f2",
		Name: "x.bin", Size: 1 << 20, ChunkSize: 1 << 20, PartCount: 1,
		Received: map[int]bool{1: true}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(ctx, session))

	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       testutil.NewMemObjectStore(),
		uploads:     upStore,
	}

	// 先以其他持有者身份占用锁。
	otherToken, locked, err := upStore.LockComplete(ctx, "up2")
	require.NoError(t, err)
	require.True(t, locked)

	err = uc.AbortUpload(ctx, "p1", "up2", "", keysPrincipal())
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.ErrorContains(t, err, "retry later")

	got, err := upStore.Get(ctx, "up2")
	require.NoError(t, err)
	require.NotNil(t, got, "complete 进行中 abort 不得删除会话")

	// 锁释放后重试成功（compare-and-del 需持锁 token）。
	require.NoError(t, upStore.UnlockComplete(ctx, "up2", otherToken))
	require.NoError(t, uc.AbortUpload(ctx, "p1", "up2", "", keysPrincipal()))
	got, err = upStore.Get(ctx, "up2")
	require.NoError(t, err)
	require.Nil(t, got, "锁释放后 abort 成功删除会话")
}

// deleteFailingSessionStore 包装 UploadSessionStore：Delete（删会话）恒失败，
// 模拟 complete 主流程成功后清理会话失败（J2-2 best-effort 验证）。
type deleteFailingSessionStore struct {
	domainstorage.UploadSessionStore
}

func (m *deleteFailingSessionStore) Delete(context.Context, string) error {
	return errors.New("redis down")
}

// cancelAfterLockStore 包装 UploadSessionStore：LockComplete 成功后立刻取消
// 请求 ctx，模拟 grpc_gateway 的 60s TimeoutHandler 在 complete/abort 进行中
// 触发超时取消（J2-1：defer 释锁必须不受请求 ctx 取消影响）。
type cancelAfterLockStore struct {
	domainstorage.UploadSessionStore
	cancel context.CancelFunc
}

func (m *cancelAfterLockStore) LockComplete(ctx context.Context, uploadID string) (string, bool, error) {
	token, ok, err := m.UploadSessionStore.LockComplete(ctx, uploadID)
	if ok {
		m.cancel()
	}
	return token, ok, err
}

// J2-2/E-P1-2：主流程（Compose + files.Insert）成功后的清理是 best-effort——
// 删分片对象与删会话失败仅 Warn，CompleteUpload 仍成功返回。
func TestUploads_CompleteUpload_CleanupBestEffort(t *testing.T) {
	_, upStore := newTestUploadSessionStore(t)
	ctx := context.Background()
	session := &domainstorage.UploadSession{
		ID: "up-be", ProjectID: "p1", BucketID: "b1", FileID: "f-be",
		Name: "x.bin", Size: 1 << 20, ChunkSize: 1 << 20, PartCount: 1,
		Received: map[int]bool{1: true}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(ctx, session))
	// Redis Create 不落 Received（parts 由 MarkChunk 维护），需显式标记分片。
	require.NoError(t, upStore.MarkChunk(ctx, session.ID, 1))

	store := testutil.NewMemObjectStore()
	require.NoError(t, store.Put(ctx, domainstorage.DefaultBucketName,
		chunkKey("p1", "b1", "f-be", 1), bytes.NewReader(make([]byte, 1<<20)), 1<<20, ""))

	files := &memFileRepo{}
	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       &failingStore{MemObjectStore: store, deleteErr: errors.New("s3 down")},
		buckets:     &memBucketRepo{},
		files:       files,
		uploads:     &deleteFailingSessionStore{UploadSessionStore: upStore},
	}

	file, err := uc.CompleteUpload(ctx, "p1", "up-be", "", keysPrincipal())
	require.NoError(t, err, "清理失败不得影响 complete 成功返回")
	require.Equal(t, "f-be", file.ID)
	require.Len(t, files.created, 1)

	// 会话删除失败 → 残留由 24h TTL 兜底；合并对象保留可下载。
	got, err := upStore.Get(ctx, "up-be")
	require.NoError(t, err)
	require.NotNil(t, got, "会话删除失败时残留由 TTL 兜底")
	composed, gerr := store.Get(ctx, domainstorage.DefaultBucketName, objectKey("p1", "b1", "f-be"))
	require.NoError(t, gerr)
	require.NoError(t, composed.Close())
}

// J2-1/E-P1-2：complete 的请求 ctx 在加锁后取消（网关 60s 超时）→
// CompleteUpload 返回错误，但 defer 释锁用独立 ctx，锁不得残留 1h。
func TestUploads_CompleteUpload_UnlockSurvivesContextCancel(t *testing.T) {
	_, upStore := newTestUploadSessionStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	session := &domainstorage.UploadSession{
		ID: "up-cancel", ProjectID: "p1", BucketID: "b1", FileID: "f-cancel",
		Name: "x.bin", Size: 1 << 20, ChunkSize: 1 << 20, PartCount: 1,
		Received: map[int]bool{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(ctx, session))

	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       testutil.NewMemObjectStore(),
		buckets:     &memBucketRepo{},
		files:       &memFileRepo{},
		uploads:     &cancelAfterLockStore{UploadSessionStore: upStore, cancel: cancel},
	}

	_, err := uc.CompleteUpload(ctx, "p1", "up-cancel", "", keysPrincipal())
	require.Error(t, err, "ctx 取消后锁内重读会话失败，complete 返回错误")

	// 锁必须已释放：可立即重新加锁（而非残留 1h completeLockTTL）。
	token, locked, lerr := upStore.LockComplete(context.Background(), "up-cancel")
	require.NoError(t, lerr)
	require.True(t, locked, "请求 ctx 取消不得导致 complete 锁残留")
	require.NoError(t, upStore.UnlockComplete(context.Background(), "up-cancel", token))
}

// J2-1/E-P1-2：abort 同理——请求 ctx 取消后 abort 返回错误（会话删除是主语义），
// 但锁释放走独立 ctx，不得残留。
func TestUploads_AbortUpload_UnlockSurvivesContextCancel(t *testing.T) {
	_, upStore := newTestUploadSessionStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	session := &domainstorage.UploadSession{
		ID: "up-abort-cancel", ProjectID: "p1", BucketID: "b1", FileID: "f-abort",
		Name: "x.bin", Size: 1 << 20, ChunkSize: 1 << 20, PartCount: 1,
		Received: map[int]bool{1: true}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(ctx, session))

	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       testutil.NewMemObjectStore(),
		uploads:     &cancelAfterLockStore{UploadSessionStore: upStore, cancel: cancel},
	}

	err := uc.AbortUpload(ctx, "p1", "up-abort-cancel", "", keysPrincipal())
	require.Error(t, err, "ctx 取消后删会话失败，abort 返回错误")

	token, locked, lerr := upStore.LockComplete(context.Background(), "up-abort-cancel")
	require.NoError(t, lerr)
	require.True(t, locked, "请求 ctx 取消不得导致 abort 锁残留")
	require.NoError(t, upStore.UnlockComplete(context.Background(), "up-abort-cancel", token))
}

// listableFileRepo 支持按 ID / 桶查询的内存 FileRepository（owner 隔离测试用）。
type listableFileRepo struct {
	memFileRepo
	files map[string]*domainstorage.File
}

func (r *listableFileRepo) Insert(_ context.Context, _ string, file *domainstorage.File) error {
	_ = r.memFileRepo.Insert(context.TODO(), "", file)
	if r.files == nil {
		r.files = map[string]*domainstorage.File{}
	}
	r.files[file.ID] = file
	return nil
}

func (r *listableFileRepo) GetByID(_ context.Context, _, id string) (*domainstorage.File, error) {
	if f, ok := r.files[id]; ok {
		cp := *f
		return &cp, nil
	}
	return nil, nil
}

func (r *listableFileRepo) ListByBucket(_ context.Context, _, bucketID string) ([]*domainstorage.File, error) {
	var out []*domainstorage.File
	for _, f := range r.files {
		if f.BucketID == bucketID {
			out = append(out, f)
		}
	}
	return out, nil
}

func endUserFilePrincipal(userID string) databases.Principal {
	return databases.Principal{Roles: []string{"users", "user:" + userID}}
}

// TestStorage_EndUserIsolation（A8）：私有桶内文件仅 owner 可读/列/删；
// public 桶对所有端用户开放；特权主体不受限。
func TestStorage_EndUserIsolation(t *testing.T) {
	files := &listableFileRepo{files: map[string]*domainstorage.File{}}
	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       testutil.NewMemObjectStore(),
		buckets: &memBucketRepo{byID: map[string]*domainstorage.Bucket{
			"private": {ID: "private", ProjectID: "p1", Public: false},
			"pub":     {ID: "pub", ProjectID: "p1", Public: true},
		}},
		files: files,
	}

	ctx := context.Background()
	mine, err := uc.CreateFile(ctx, CreateFileCommand{ProjectID: "p1", BucketID: "private", Name: "a.txt"}, strings.NewReader("a"), 1, endUserFilePrincipal("u1"))
	require.NoError(t, err)
	require.Equal(t, "u1", mine.OwnerUserID, "owner 应从 principal 派生")

	// 他人（u2）读/删 owner 文件：NotFound/拒绝；列表不可见。
	_, _, err = uc.GetFile(ctx, "p1", "private", mine.ID, endUserFilePrincipal("u2"))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	err = uc.DeleteFile(ctx, "p1", "private", mine.ID, endUserFilePrincipal("u2"))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	list, total, _, err := uc.ListFiles(ctx, "p1", "private", databases.Query{}, endUserFilePrincipal("u2"))
	require.NoError(t, err)
	require.Empty(t, list)
	require.Zero(t, total)

	// owner（u1）可见自己的文件。
	list, total, _, err = uc.ListFiles(ctx, "p1", "private", databases.Query{}, endUserFilePrincipal("u1"))
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, int64(1), total)

	// public 桶：他人（u2）可读。
	pubFile, err := uc.CreateFile(ctx, CreateFileCommand{ProjectID: "p1", BucketID: "pub", Name: "b.txt"}, strings.NewReader("b"), 1, endUserFilePrincipal("u1"))
	require.NoError(t, err)
	_, _, err = uc.GetFile(ctx, "p1", "pub", pubFile.ID, endUserFilePrincipal("u2"))
	require.NoError(t, err)

	// 特权主体（keys）可见私有桶全部文件。
	list, _, _, err = uc.ListFiles(ctx, "p1", "private", databases.Query{}, keysPrincipal())
	require.NoError(t, err)
	require.Len(t, list, 1)
}
