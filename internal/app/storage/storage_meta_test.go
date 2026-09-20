package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// countingGetStore 包装内存对象存储，统计 Get 调用次数（P2 修复第 1 项：
// 断言元数据路径不再调用 store.Get）。
type countingGetStore struct {
	*testutil.MemObjectStore
	gets int
}

func (s *countingGetStore) Get(ctx context.Context, bucket, key string) (rc io.ReadCloser, err error) {
	s.gets++
	return s.MemObjectStore.Get(ctx, bucket, key)
}

// newMetaTestUC 组装纯内存元数据读/删除测试环境（无 Postgres/MinIO）：
// listableFileRepo 支持按 ID/桶查询，principal 为 keys 特权主体。
func newMetaTestUC(t *testing.T) (context.Context, *Storage, *countingGetStore, *listableFileRepo) {
	t.Helper()
	store := &countingGetStore{MemObjectStore: testutil.NewMemObjectStore()}
	files := &listableFileRepo{files: map[string]*domainstorage.File{}}
	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", Name: "p1", InternalID: 1}},
		store:       store,
		buckets: &memBucketRepo{byID: map[string]*domainstorage.Bucket{
			"b1": {ID: "b1", ProjectID: "p1", Public: false},
		}},
		files: files,
	}
	return context.Background(), uc, store, files
}

// P2 修复第 1 项（GetFile 元数据路径泄漏 S3 连接）：gRPC GetFile 元数据读取
// 走 GetFileMeta，不得打开对象内容流——store.Get 零调用即证明没有
// GetObject+Stat 建立的 HTTP 连接可泄漏。
func TestGetFileMeta_DoesNotOpenObjectStream(t *testing.T) {
	ctx, uc, store, files := newMetaTestUC(t)
	files.files["f1"] = &domainstorage.File{ID: "f1", ProjectID: "p1", BucketID: "b1", Name: "a.txt"}
	// 对照组需要真实对象：GetFile 走 store.Get 命中。
	require.NoError(t, store.Put(ctx, domainstorage.DefaultBucketName,
		objectKey("p1", "b1", "f1"), strings.NewReader("hello"), 5, ""))

	// 对照组：GetFile 确实经过 store.Get（计数器有效）。
	got, reader, err := uc.GetFile(ctx, "p1", "b1", "f1", keysPrincipal())
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, reader)
	require.NoError(t, reader.Close())
	require.Equal(t, 1, store.gets, "对照组：GetFile 打开一次对象流")

	// 元数据路径：对象不在存储中也不影响读取，且 store.Get 零调用。
	meta, err := uc.GetFileMeta(ctx, "p1", "b1", "f1", keysPrincipal())
	require.NoError(t, err)
	require.NotNil(t, meta)
	require.Equal(t, "f1", meta.ID)
	require.Equal(t, "a.txt", meta.Name)
	require.Equal(t, 1, store.gets, "GetFileMeta 不得调用 store.Get")
}

// GetFileMeta 沿用与 GetFile 相同的定位与鉴权链。
func TestGetFileMeta_AuthzParity(t *testing.T) {
	ctx, uc, _, files := newMetaTestUC(t)
	files.files["f1"] = &domainstorage.File{ID: "f1", ProjectID: "p1", BucketID: "b1", Name: "a.txt", OwnerUserID: "u1"}

	// 非属主端用户 → PermissionDenied（与 GetFile 一致）。
	_, err := uc.GetFileMeta(ctx, "p1", "b1", "f1", endUserFilePrincipal("u2"))
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// 缺 bucket → NotFound。
	_, err = uc.GetFileMeta(ctx, "p1", "missing", "f1", keysPrincipal())
	require.Equal(t, codes.NotFound, status.Code(err))

	// 缺文件 → NotFound。
	_, err = uc.GetFileMeta(ctx, "p1", "b1", "nope", keysPrincipal())
	require.Equal(t, codes.NotFound, status.Code(err))
}

// P2 修复第 1 项配套（原第 5 项错误映射）：文档在而对象丢失的不一致态，
// GetFile 统一映射 NotFound（旧实现透传底层错误，HTTP 侧 500 无法与暂态故障
// 区分重试）。
func TestGetFile_ObjectMissingMapsNotFound(t *testing.T) {
	// countingGetStore 底层内存库中无对象 → Get 返回 ErrObjectNotFound 包装。
	ctx, uc, _, files := newMetaTestUC(t)
	files.files["f1"] = &domainstorage.File{ID: "f1", ProjectID: "p1", BucketID: "b1", Name: "a.txt"}

	_, _, err := uc.GetFile(ctx, "p1", "b1", "f1", keysPrincipal())
	require.Equal(t, codes.NotFound, status.Code(err))
	require.ErrorContains(t, err, "file content not found")
}

// P2 修复第 2 项（DeleteFile 孤儿对象）：对象删除失败必须返回错误且保留 DB 行
// （可重试）；「对象不存在」类错误视为删除成功照常删行。
func TestDeleteFile_ObjectDeleteFailureKeepsRow(t *testing.T) {
	ctx, uc, _, files := newMetaTestUC(t)
	files.files["f1"] = &domainstorage.File{ID: "f1", ProjectID: "p1", BucketID: "b1", Name: "a.txt"}
	uc.store = &failingStore{MemObjectStore: testutil.NewMemObjectStore(), deleteErr: errors.New("s3 down")}

	err := uc.DeleteFile(ctx, "p1", "b1", "f1", keysPrincipal())
	require.Error(t, err)
	require.ErrorContains(t, err, "delete file object")
	require.Empty(t, files.deleted, "对象删除失败时不得删除文件元数据行")
}

func TestDeleteFile_ObjectMissingStillDeletesRow(t *testing.T) {
	ctx, uc, _, files := newMetaTestUC(t)
	files.files["f1"] = &domainstorage.File{ID: "f1", ProjectID: "p1", BucketID: "b1", Name: "a.txt"}
	// NoSuchKey 类错误（ErrObjectNotFound 哨兵）视为删除成功。
	uc.store = &failingStore{MemObjectStore: testutil.NewMemObjectStore(),
		deleteErr: domainstorage.ErrObjectNotFound}

	require.NoError(t, uc.DeleteFile(ctx, "p1", "b1", "f1", keysPrincipal()))
	require.Equal(t, []string{"f1"}, files.deleted, "对象不存在视为删除成功，照常删行")
}

func TestDeleteFile_Success(t *testing.T) {
	ctx, uc, _, files := newMetaTestUC(t)
	files.files["f1"] = &domainstorage.File{ID: "f1", ProjectID: "p1", BucketID: "b1", Name: "a.txt"}

	require.NoError(t, uc.DeleteFile(ctx, "p1", "b1", "f1", keysPrincipal()))
	require.Equal(t, []string{"f1"}, files.deleted)
}

// P2 修复第 7 项：normalizeMimeType 把 image/svg+xml（含带参数形态）降级为
// application/octet-stream——存储侧不再保留 SVG mime，inline 内联契约不再单点
// 依赖 serverhttp 的 inlineSafeMime。
func TestNormalizeMimeType_SvgDowngraded(t *testing.T) {
	require.Equal(t, "application/octet-stream", normalizeMimeType("image/svg+xml"))
	require.Equal(t, "application/octet-stream", normalizeMimeType("image/svg+xml; charset=utf-8"))
	// 对照：普通图片 mime 不受影响。
	require.Equal(t, "image/png", normalizeMimeType("image/png"))
	require.Equal(t, "application/octet-stream", normalizeMimeType(""))
	require.True(t, strings.HasPrefix(normalizeMimeType("text/html; charset=utf-8"), "application/octet-stream"))
}
