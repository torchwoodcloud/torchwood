package bunrepo_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestFileRepository_SumSizeOwnerNullAndBucketCascade(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	usersRepo := bunrepo.NewUserRepository(db)
	user := seedSysUser(t, ctx, usersRepo, projectID, &domainusers.User{
		ID:           "u-file",
		Email:        "file@torchwood.local",
		PasswordHash: "h",
		Name:         "File",
		Status:       domainusers.StatusActive,
	})
	buckets := bunrepo.NewBucketRepository(db)
	files := bunrepo.NewFileRepository(db)

	require.NoError(t, buckets.Insert(ctx, projectID, &domainstorage.Bucket{
		ID:          "b1",
		Name:        "docs",
		Permissions: []string{"read:any"},
		Public:      false,
	}))
	gotB, err := buckets.GetByID(ctx, projectID, "b1")
	require.NoError(t, err)
	require.Equal(t, projectID, gotB.ProjectID)
	require.Equal(t, "docs", gotB.Name)
	require.Equal(t, []string{"read:any"}, gotB.Permissions)
	require.False(t, gotB.Public)

	require.NoError(t, buckets.Update(ctx, projectID, "b1", map[string]any{
		"name":        "docs-2",
		"public":      true,
		"permissions": []string{"user:u-file"},
	}))
	gotB, err = buckets.GetByID(ctx, projectID, "b1")
	require.NoError(t, err)
	require.Equal(t, "docs-2", gotB.Name)
	require.True(t, gotB.Public)
	require.Equal(t, []string{"user:u-file"}, gotB.Permissions)

	require.NoError(t, files.Insert(ctx, projectID, &domainstorage.File{
		ID:          "f1",
		BucketID:    "b1",
		Name:        "a.bin",
		MimeType:    "application/octet-stream",
		Size:        100,
		Metadata:    map[string]string{"k": "v"},
		OwnerUserID: user.ID,
	}))
	require.NoError(t, files.Insert(ctx, projectID, &domainstorage.File{
		ID:       "f2",
		BucketID: "b1",
		Name:     "b.bin",
		Size:     50,
	}))

	gotF, err := files.GetByID(ctx, projectID, "f1")
	require.NoError(t, err)
	require.Equal(t, projectID, gotF.ProjectID)
	require.Equal(t, "b1", gotF.BucketID)
	require.Equal(t, user.ID, gotF.OwnerUserID)
	require.Equal(t, int64(100), gotF.Size)
	require.Equal(t, "v", gotF.Metadata["k"])

	// SQL 下推（P2 修复第 6 项）：owner 过滤与 LIMIT/OFFSET 由 SQL 侧执行。
	filtered, total, err := files.ListByBucket(ctx, projectID, "b1", user.ID, 0, 0)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Equal(t, int64(1), total)
	require.Equal(t, "f1", filtered[0].ID)
	paged, total, err := files.ListByBucket(ctx, projectID, "b1", "", 1, 1)
	require.NoError(t, err)
	require.Len(t, paged, 1)
	require.Equal(t, int64(2), total)
	require.Equal(t, "f1", paged[0].ID, "created_at DESC, id DESC 序第一页为 f2，offset=1 取 f1")
	clamped, total, err := files.ListByBucket(ctx, projectID, "b1", "", 1000, 0)
	require.NoError(t, err)
	require.Len(t, clamped, 2, "超上限 limit 由 repo clamp，不影响返回")
	require.Equal(t, int64(2), total)

	sum, err := files.SumSize(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(150), sum)

	require.NoError(t, files.Update(ctx, projectID, "f1", map[string]any{
		"name":      "a2.bin",
		"mime_type": "text/plain",
		"metadata":  map[string]string{"k": "v2"},
	}))
	gotF, err = files.GetByID(ctx, projectID, "f1")
	require.NoError(t, err)
	require.Equal(t, "a2.bin", gotF.Name)
	require.Equal(t, "text/plain", gotF.MimeType)
	require.Equal(t, "v2", gotF.Metadata["k"])
	require.Equal(t, int64(100), gotF.Size, "Update 不得覆盖 size")
	require.Equal(t, user.ID, gotF.OwnerUserID)

	err = files.Update(ctx, projectID, "f1", map[string]any{"size": int64(1)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	require.NoError(t, usersRepo.Delete(ctx, projectID, user.ID))
	gotF, err = files.GetByID(ctx, projectID, "f1")
	require.NoError(t, err)
	require.NotNil(t, gotF)
	require.Empty(t, gotF.OwnerUserID, "users FK ON DELETE SET NULL")
	sum, err = files.SumSize(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(150), sum)

	listed, total, err := files.ListByBucket(ctx, projectID, "b1", "", 0, 0)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	require.Equal(t, int64(2), total)

	require.NoError(t, buckets.Delete(ctx, projectID, "b1"))
	gotF, err = files.GetByID(ctx, projectID, "f1")
	require.NoError(t, err)
	require.Nil(t, gotF, "buckets FK CASCADE 应删 files")
	listed, total, err = files.ListByBucket(ctx, projectID, "b1", "", 0, 0)
	require.NoError(t, err)
	require.Empty(t, listed)
	require.Zero(t, total)
	sum, err = files.SumSize(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, int64(0), sum)

	err = files.Insert(ctx, "", &domainstorage.File{ID: "fx", BucketID: "b1", Name: "x"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
