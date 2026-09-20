package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// idemFileRepo 幂等测试用内存 FileRepository：可注入「文档已存在」（doc）、
// 「首查不存在、Insert 冲突后已存在」（docAfterFirstGet，模拟锁过期竞态窗口）与
// 「Insert 失败」（insertErr，已映射为 AlreadyExists 或任意真错误）。
type idemFileRepo struct {
	doc              *domainstorage.File
	docAfterFirstGet bool
	insertErr        error
	created          []string
	gets             int
}

func (r *idemFileRepo) Insert(_ context.Context, _ string, file *domainstorage.File) error {
	r.created = append(r.created, file.ID)
	return r.insertErr
}

func (r *idemFileRepo) GetByID(context.Context, string, string) (*domainstorage.File, error) {
	r.gets++
	if r.docAfterFirstGet && r.gets == 1 {
		// 幂等重入预检查时刻文档尚未存在（另一 complete 在锁过期后才成功）。
		return nil, nil
	}
	return r.doc, nil
}

func (r *idemFileRepo) ListByBucket(context.Context, string, string) ([]*domainstorage.File, error) {
	return nil, nil
}

func (r *idemFileRepo) Count(context.Context, string) (int64, error) { return 0, nil }

func (r *idemFileRepo) Update(context.Context, string, string, map[string]any) error { return nil }

func (r *idemFileRepo) Delete(context.Context, string, string) error { return nil }

func (r *idemFileRepo) SumSize(context.Context, string) (int64, error) { return 0, nil }

// recordingStore 包装 MemObjectStore，记录 Delete 调用的 key（断言 dstKey 未被删）。
type recordingStore struct {
	*testutil.MemObjectStore
	deleted []string
}

func (s *recordingStore) Delete(ctx context.Context, bucket, key string) error {
	s.deleted = append(s.deleted, key)
	return s.MemObjectStore.Delete(ctx, bucket, key)
}

// newIdempotentUploadUC 组装纯内存 CompleteUpload 测试环境：单分片会话已收齐、
// 分片对象在库，返回记录型对象存储与文件仓库。
func newIdempotentUploadUC(t *testing.T, uploadID, fileID string) (context.Context, *Storage, *recordingStore, *idemFileRepo, domainstorage.UploadSessionStore) {
	t.Helper()
	_, upStore := newTestUploadSessionStore(t)
	ctx := context.Background()
	session := &domainstorage.UploadSession{
		ID: uploadID, ProjectID: "p1", BucketID: "b1", FileID: fileID,
		Name: "x.bin", Size: 1 << 20, ChunkSize: 1 << 20, PartCount: 1,
		Received: map[int]bool{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(ctx, session))
	// Redis Create 不落 Received（parts 由 MarkChunk 维护），需显式标记分片。
	require.NoError(t, upStore.MarkChunk(ctx, uploadID, 1))

	store := &recordingStore{MemObjectStore: testutil.NewMemObjectStore()}
	require.NoError(t, store.Put(ctx, domainstorage.DefaultBucketName,
		chunkKey("p1", "b1", fileID, 1), bytes.NewReader(make([]byte, 1<<20)), 1<<20, ""))

	files := &idemFileRepo{}
	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", InternalID: 1}},
		store:       store,
		buckets:     &memBucketRepo{},
		files:       files,
		uploads:     upStore,
	}
	return ctx, uc, store, files, upStore
}

// TestUploads_CompleteUpload_IdempotentWhenDocumentExists（complete 非幂等数据损坏
// 缺陷修复）：首次 complete 在 files.Insert 成功后、分片清理开始前被取消（网关
// 60s 超时/进程崩溃）→ 会话与分片保留而文档已存在。重试 complete 必须幂等成功
// 返回已有文档，且绝不删 dstKey（旧实现回滚分支会删掉已落地对象 → files 行在、
// 下载永久 404）。
func TestUploads_CompleteUpload_IdempotentWhenDocumentExists(t *testing.T) {
	ctx, uc, store, files, upStore := newIdempotentUploadUC(t, "up-idem", "f-idem")
	// 上次 attempt 已合成的最终对象。
	require.NoError(t, store.Put(ctx, domainstorage.DefaultBucketName,
		objectKey("p1", "b1", "f-idem"), bytes.NewReader([]byte("composed")), 8, ""))

	dstKey := objectKey("p1", "b1", "f-idem")
	existing := &domainstorage.File{
		ID: "f-idem", ProjectID: "p1", BucketID: "b1", Name: "original.bin",
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute),
	}
	files.doc = existing

	file, err := uc.CompleteUpload(ctx, "p1", "up-idem", "", keysPrincipal())
	require.NoError(t, err, "文档已存在时重试 complete 必须幂等成功")
	require.Equal(t, existing, file, "必须返回已存在的那条 file 记录")
	require.Empty(t, files.created, "幂等路径不得再次 Insert")

	// 核心断言：dstKey 未被删（旧实现此处误删 → 下载 404）。
	require.NotContains(t, store.deleted, dstKey)
	got, gerr := store.Get(ctx, domainstorage.DefaultBucketName, dstKey)
	require.NoError(t, gerr, "已落地对象必须保留")
	require.NoError(t, got.Close())

	// 清理照常 best-effort：残留分片与会话回收。
	_, cerr := store.Get(ctx, domainstorage.DefaultBucketName, chunkKey("p1", "b1", "f-idem", 1))
	require.Error(t, cerr, "残留分片应被清理")
	s2, gerr2 := upStore.Get(ctx, "up-idem")
	require.NoError(t, gerr2)
	require.Nil(t, s2, "会话应被清理")
}

// TestUploads_CompleteUpload_InsertConflictReturnsExistingDocument（锁过期窗口）：
// 幂等预检查时文档尚未存在 → Compose 后 Insert 因 fileID 主键冲突失败（另一
// complete 已在锁 TTL 过期后成功）→ 幂等返回已存在文档且不删 dstKey（旧实现此处
// 进入回滚分支误删对象，正是本次修复的数据损坏缺陷）。
func TestUploads_CompleteUpload_InsertConflictReturnsExistingDocument(t *testing.T) {
	ctx, uc, store, files, upStore := newIdempotentUploadUC(t, "up-race", "f-race")
	dstKey := objectKey("p1", "b1", "f-race")
	files.doc = &domainstorage.File{
		ID: "f-race", ProjectID: "p1", BucketID: "b1", Name: "winner.bin",
		CreatedAt: time.Now().Add(-time.Minute), UpdatedAt: time.Now().Add(-time.Minute),
	}
	files.docAfterFirstGet = true
	// 对齐 files_repo 的 23505 → AlreadyExists 映射。
	files.insertErr = status.Error(codes.AlreadyExists, "file already exists")

	file, err := uc.CompleteUpload(ctx, "p1", "up-race", "", keysPrincipal())
	require.NoError(t, err, "Insert 主键冲突必须幂等返回已存在文档")
	require.Equal(t, "winner.bin", file.Name)
	require.Len(t, files.created, 1, "冲突前执行过一次 Insert")

	// 核心断言：冲突不触发回滚，dstKey 保留。
	require.NotContains(t, store.deleted, dstKey)
	got, gerr := store.Get(ctx, domainstorage.DefaultBucketName, dstKey)
	require.NoError(t, gerr, "Compose 重建的对象必须保留")
	require.NoError(t, got.Close())

	// 清理照常 best-effort。
	s2, gerr2 := upStore.Get(ctx, "up-race")
	require.NoError(t, gerr2)
	require.Nil(t, s2, "会话应被清理")
}

// TestUploads_CompleteUpload_AlreadyExistsWithoutRecord_NeverDeletesObject：
// Insert 报 AlreadyExists 但 GetByID 未命中/失败（DB 抖动，理论上不可达）时，
// 仍不得回滚删 dstKey——23505 只能来自 id 主键、文档必然在，删对象即数据损坏；
// 应原样返回错误让调用方重试（重试命中幂等重入检查）。
func TestUploads_CompleteUpload_AlreadyExistsWithoutRecord_NeverDeletesObject(t *testing.T) {
	ctx, uc, store, files, _ := newIdempotentUploadUC(t, "up-nohit", "f-nohit")
	dstKey := objectKey("p1", "b1", "f-nohit")
	files.insertErr = status.Error(codes.AlreadyExists, "file already exists")

	_, err := uc.CompleteUpload(ctx, "p1", "up-nohit", "", keysPrincipal())
	require.Error(t, err)
	require.Contains(t, err.Error(), "create file document")
	require.NotContains(t, store.deleted, dstKey, "AlreadyExists 分支绝不回滚删对象")
}

// TestUploads_CompleteUpload_TrueInsertFailureRollsBackObject：文档确不存在的真
// 插入失败仍走回滚删最终对象（避免留下无文档孤儿对象），且会话保留可重试。
func TestUploads_CompleteUpload_TrueInsertFailureRollsBackObject(t *testing.T) {
	ctx, uc, store, files, upStore := newIdempotentUploadUC(t, "up-truefail", "f-truefail")
	dstKey := objectKey("p1", "b1", "f-truefail")
	files.insertErr = errors.New("db down")

	_, err := uc.CompleteUpload(ctx, "p1", "up-truefail", "", keysPrincipal())
	require.Error(t, err)
	require.Contains(t, err.Error(), "create file document")
	require.Contains(t, store.deleted, dstKey, "真插入失败必须回滚删最终对象")
	_, gerr := store.Get(ctx, domainstorage.DefaultBucketName, dstKey)
	require.Error(t, gerr, "回滚后 dstKey 应不存在")

	s2, gerr2 := upStore.Get(ctx, "up-truefail")
	require.NoError(t, gerr2)
	require.NotNil(t, s2, "回滚路径不删会话（可重试）")
}

// TestUploads_CompleteIdempotentAfterLostCleanup 集成（真实 PG files 表链路）：
// 首次 complete 成功后手工重建会话，模拟「Insert 成功后、清理完成前崩溃」——
// 分片已删、对象已合成、文档已落地。重试 complete 幂等成功（旧实现：Compose 因
// 分片缺失失败或 Insert 主键冲突后误删对象），已落地对象保留可下载。
func TestUploads_CompleteIdempotentAfterLostCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, objStore := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	content := bytes.Repeat([]byte("z"), 6<<20)
	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "idem.bin",
		Size:      int64(len(content)),
	}, principal)
	require.NoError(t, err)
	uploadFullChunks(t, ctx, uc, projectID, session, content)

	file1, err := uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.NoError(t, err)

	// 模拟「Insert 成功后、清理完成前崩溃」：文档与对象在，分片已删，会话残留。
	resumed := &domainstorage.UploadSession{
		ID: session.ID, ProjectID: session.ProjectID, BucketID: session.BucketID,
		FileID: session.FileID, Name: session.Name, MimeType: session.MimeType,
		Size: session.Size, ChunkSize: session.ChunkSize, PartCount: session.PartCount,
		Received: map[int]bool{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, uc.uploads.Create(ctx, resumed))
	for i := 1; i <= session.PartCount; i++ {
		require.NoError(t, uc.uploads.MarkChunk(ctx, session.ID, i))
	}

	file2, err := uc.CompleteUpload(ctx, projectID, session.ID, "", principal)
	require.NoError(t, err, "文档已存在时重试 complete 必须幂等成功（而非冲突报错）")
	require.Equal(t, file1.ID, file2.ID)

	// 已落地对象未被回滚删除，仍可下载。
	_, reader, err := uc.GetFile(ctx, projectID, bucketID, file1.ID, principal)
	require.NoError(t, err, "对象必须保留（旧实现此处被误删 → 下载 404）")
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.NoError(t, reader.Close())
	require.Equal(t, content, got)

	// 残留会话清理照常 best-effort。
	s2, gerr := uc.GetUploadSession(ctx, projectID, session.ID, principal)
	require.Error(t, gerr)
	require.Nil(t, s2)

	// 分片对象确已缺失（第一次 complete 已清理），证明幂等成功不依赖分片齐全。
	_, cerr := objStore.Get(ctx, domainstorage.DefaultBucketName,
		chunkKey(projectID, bucketID, session.FileID, 1))
	require.Error(t, cerr)
}
