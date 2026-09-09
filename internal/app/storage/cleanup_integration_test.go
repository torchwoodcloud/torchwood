package storage

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCleanupOrphanChunks 造一个 >48h 的孤儿分片对象 + 一个 <24h 活跃分片
// + 一个无 /chunks/ 段的旧文件对象 → 只删孤儿分片。
func TestCleanupOrphanChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, objStore := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	// 活跃会话分片（<24h，不应被清理）：真实 UploadChunk。
	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "active.bin",
		Size:      24 << 20,
	}, principal)
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(make([]byte, 16<<20)), 16<<20, "", principal)
	require.NoError(t, err)
	activeChunk := chunkKey(projectID, bucketID, session.FileID, 1)

	// 孤儿分片对象（>48h，应被清理）。
	orphanFile := "ghost-file-id"
	orphanChunk := chunkKey(projectID, bucketID, orphanFile, 3)
	require.NoError(t, objStore.Put(ctx, domainstorage.DefaultBucketName, orphanChunk, bytes.NewReader(make([]byte, 1<<20)), 1<<20, ""))
	require.NoError(t, objStore.SetObjectTime(domainstorage.DefaultBucketName, orphanChunk, time.Now().Add(-49*time.Hour)))

	// 旧文件对象（无 /chunks/ 段，不应被清理）。
	oldFileKey := projectID + "/" + bucketID + "/old-file-id"
	require.NoError(t, objStore.Put(ctx, domainstorage.DefaultBucketName, oldFileKey, bytes.NewReader([]byte("x")), 1, ""))
	require.NoError(t, objStore.SetObjectTime(domainstorage.DefaultBucketName, oldFileKey, time.Now().Add(-49*time.Hour)))

	removed, err := uc.CleanupOrphanChunks(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, removed, "只应删除 >48h 的孤儿分片")

	// 孤儿分片已删；活跃分片与旧文件对象仍在。
	_, err = objStore.Get(ctx, domainstorage.DefaultBucketName, orphanChunk)
	require.Error(t, err, "孤儿分片应被清理")
	_, err = objStore.Get(ctx, domainstorage.DefaultBucketName, activeChunk)
	require.NoError(t, err, "活跃会话分片不应被清理")
	_, err = objStore.Get(ctx, domainstorage.DefaultBucketName, oldFileKey)
	require.NoError(t, err, "无 /chunks/ 段的对象不应被清理")
}

// TestDeleteBucket_RemovesOrphanChunks DeleteBucket 后残留分片对象被同步清理，
// 且文件元数据文档（files 集合）一并删除（R07 结论 3）。
func TestDeleteBucket_RemovesOrphanChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx, uc, projectID, _, objStore := newUploadsUC(t)
	bucketID := mustCreateBucket(t, ctx, uc, projectID)
	principal := databases.Principal{Roles: []string{"keys"}}

	// 上传分片但未 complete（会话残留）。
	session, err := uc.CreateUploadSession(ctx, CreateUploadCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "leftover.bin",
		Size:      24 << 20,
	}, principal)
	require.NoError(t, err)
	_, err = uc.UploadChunk(ctx, projectID, session.ID, 1, bytes.NewReader(make([]byte, 16<<20)), 16<<20, "", principal)
	require.NoError(t, err)
	chunk := chunkKey(projectID, bucketID, session.FileID, 1)
	_, err = objStore.Get(ctx, domainstorage.DefaultBucketName, chunk)
	require.NoError(t, err)

	// 已 complete 的完整文件（文档 + 对象）。
	file, err := uc.CreateFile(ctx, CreateFileCommand{
		ProjectID: projectID,
		BucketID:  bucketID,
		Name:      "complete.txt",
		MimeType:  "text/plain",
	}, bytes.NewReader([]byte("hello")), 5, principal)
	require.NoError(t, err)
	_, _, err = uc.GetFile(ctx, projectID, bucketID, file.ID, principal)
	require.NoError(t, err, "DeleteBucket 前文件可读")

	require.NoError(t, uc.DeleteBucket(ctx, projectID, bucketID, principal))

	// 分片对象被同步清理。
	_, err = objStore.Get(ctx, domainstorage.DefaultBucketName, chunk)
	require.Error(t, err, "DeleteBucket 应清理残留分片对象")

	// files 文档已删（显式断言，R07 结论 3）：GetFile 必须 NotFound 而非孤儿残留。
	_, _, err = uc.GetFile(ctx, projectID, bucketID, file.ID, principal)
	require.Equal(t, codes.NotFound, status.Code(err), "DeleteBucket 应删除 files 文档")
	// 文件对象已删。
	_, err = objStore.Get(ctx, domainstorage.DefaultBucketName, objectKey(projectID, bucketID, file.ID))
	require.Error(t, err, "DeleteBucket 应删除文件对象")

	// bucket 元数据已删（ListBuckets 不再返回）。
	buckets, _, _, err := uc.ListBuckets(ctx, projectID, databases.Query{}, principal)
	require.NoError(t, err)
	for _, b := range buckets {
		require.NotEqual(t, bucketID, b.ID)
	}
}
