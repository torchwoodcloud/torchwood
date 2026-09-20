package storage

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newFinalPartUC 组装纯内存 UploadChunk 测试环境（miniredis 会话 + 内存对象库，
// 无 Postgres），返回会话存储供测试直接建会话（自定义 Size/ChunkSize/PartCount）。
func newFinalPartUC(t *testing.T) (context.Context, *Storage, domainstorage.UploadSessionStore) {
	t.Helper()
	_, upStore := newTestUploadSessionStore(t)
	uc := &Storage{
		cfg:         &config.AppConfig{},
		projectRepo: &stubProjectRepo{p: &projects.Project{ID: "p1", Name: "p1", InternalID: 1}},
		store:       testutil.NewMemObjectStore(),
		buckets:     &memBucketRepo{},
		files:       &memFileRepo{},
		uploads:     upStore,
	}
	return context.Background(), uc, upStore
}

// createFinalPartSession 以指定 Size/ChunkSize/PartCount 建会话。
func createFinalPartSession(t *testing.T, upStore domainstorage.UploadSessionStore, id string, size, chunkSize int64, partCount int) {
	t.Helper()
	session := &domainstorage.UploadSession{
		ID: id, ProjectID: "p1", BucketID: "b1", FileID: "f-" + id,
		Name: "x.bin", Size: size, ChunkSize: chunkSize, PartCount: partCount,
		Received: map[int]bool{}, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	require.NoError(t, upStore.Create(context.Background(), session))
}

// P2 修复第 3 项：末片大小精确校验——
// 精确余量（Size-(PartCount-1)*ChunkSize）通过，±1 偏差拒绝。
func TestUploadChunk_FinalPartSizeExact(t *testing.T) {
	ctx, uc, upStore := newFinalPartUC(t)
	principal := keysPrincipal()

	// 3MiB / 1MiB 片 × 3：末片期望 3MiB - 2×1MiB = 1MiB。
	createFinalPartSession(t, upStore, "up-3p", 3<<20, 1<<20, 3)
	_, err := uc.UploadChunk(ctx, "p1", "up-3p", 1, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "", principal)
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, "p1", "up-3p", 2, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "", principal)
	require.NoError(t, err)

	// 末片精确余量 → 通过。
	_, err = uc.UploadChunk(ctx, "p1", "up-3p", 3, bytes.NewReader(make([]byte, 1<<20)), 1<<20, "", principal)
	require.NoError(t, err)

	// 末片偏差（±1）→ InvalidArgument，且错误带期望字节数。
	_, err = uc.UploadChunk(ctx, "p1", "up-3p", 3, bytes.NewReader(make([]byte, (1<<20)-1)), (1<<20)-1, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "final part size must be exactly 1048576")
	_, err = uc.UploadChunk(ctx, "p1", "up-3p", 3, bytes.NewReader(make([]byte, 1<<20+1)), 1<<20+1, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "final part size must be exactly 1048576")

	// 单片会话：末片即唯一片，期望 = Size（512KiB < chunkSize 1MiB 亦精确校验）。
	createFinalPartSession(t, upStore, "up-1p", 512<<10, 1<<20, 1)
	_, err = uc.UploadChunk(ctx, "p1", "up-1p", 1, bytes.NewReader(make([]byte, 512<<10)), 512<<10, "", principal)
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, "p1", "up-1p", 1, bytes.NewReader(make([]byte, 512<<10+1)), 512<<10+1, "", principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "final part size must be exactly 524288")
}
