package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// newMinioTestStore 构造指向真实 MinIO 的 store；
// TORCHWOOD_TEST_MINIO_ENDPOINT 未设时跳过（本地 docker compose / CI minio service）。
func newMinioTestStore(t *testing.T) (*minioObjectStore, string) {
	t.Helper()
	endpoint := os.Getenv("TORCHWOOD_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("TORCHWOOD_TEST_MINIO_ENDPOINT not set; skipping minio integration test")
	}
	accessKey := os.Getenv("TORCHWOOD_TEST_MINIO_ACCESS_KEY_ID")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("TORCHWOOD_TEST_MINIO_SECRET_ACCESS_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	cfg := &config.AppConfig{}
	cfg.Storage = &config.Storage{S3: &config.Storage_S3{
		Endpoint:        endpoint,
		AccessKeyId:     accessKey,
		SecretAccessKey: secretKey,
		Bucket:          "torchwood-test-" + idgen.UUID().String(),
	}}
	store, err := NewMinioObjectStore(cfg)
	require.NoError(t, err)
	m := store.(*minioObjectStore)
	ctx := context.Background()
	require.NoError(t, m.EnsureBucket(ctx, m.bucketName()))
	t.Cleanup(func() {
		_ = m.client.RemoveBucket(ctx, m.bucketName())
	})
	return m, m.bucketName()
}

// TestMinio_ComposeTwoParts 真实 ComposeObject 合并 2×6MiB 分片：
// GetObject 校验内容与字节数完全一致。
func TestMinio_ComposeTwoParts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	m, bucket := newMinioTestStore(t)
	ctx := context.Background()

	part1 := make([]byte, 6<<20)
	part2 := make([]byte, 6<<20)
	_, err := rand.Read(part1)
	require.NoError(t, err)
	_, err = rand.Read(part2)
	require.NoError(t, err)

	require.NoError(t, m.Put(ctx, bucket, "src/1", bytes.NewReader(part1), int64(len(part1)), "application/octet-stream"))
	require.NoError(t, m.Put(ctx, bucket, "src/2", bytes.NewReader(part2), int64(len(part2)), "application/octet-stream"))

	dst := "out/composed.bin"
	require.NoError(t, m.Compose(ctx, bucket, dst, []string{"src/1", "src/2"}))

	reader, err := m.Get(ctx, bucket, dst)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	want := append(append([]byte{}, part1...), part2...)
	require.Equal(t, len(want), len(got), "合并后字节数必须等于两片之和")
	require.Equal(t, want, got, "合并后内容必须按序一致")
}

// TestMinio_ComposeSmallNonFinalPart 非末片 < 5MiB → ComposeObject 报错兜底。
func TestMinio_ComposeSmallNonFinalPart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	m, bucket := newMinioTestStore(t)
	ctx := context.Background()

	small := make([]byte, 1<<20)
	big := make([]byte, 6<<20)
	_, err := rand.Read(small)
	require.NoError(t, err)
	_, err = rand.Read(big)
	require.NoError(t, err)

	require.NoError(t, m.Put(ctx, bucket, "src/small", bytes.NewReader(small), int64(len(small)), ""))
	require.NoError(t, m.Put(ctx, bucket, "src/big", bytes.NewReader(big), int64(len(big)), ""))

	err = m.Compose(ctx, bucket, "out/bad.bin", []string{"src/small", "src/big"})
	require.Error(t, err, "非末片 1MiB < 5MiB 必须被 ComposeObject 拒绝")
}

// TestMinio_ListPrefix 真实 ListObjects：前缀过滤 + LastModified 非零。
func TestMinio_ListPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	m, bucket := newMinioTestStore(t)
	ctx := context.Background()

	require.NoError(t, m.Put(ctx, bucket, "p1/f1", bytes.NewReader([]byte("a")), 1, ""))
	require.NoError(t, m.Put(ctx, bucket, "p1/f2/chunks/001", bytes.NewReader([]byte("b")), 1, ""))
	require.NoError(t, m.Put(ctx, bucket, "p2/f3", bytes.NewReader([]byte("c")), 1, ""))

	objects, err := m.List(ctx, bucket, "p1/")
	require.NoError(t, err)
	require.Len(t, objects, 2, "p1/ 前缀应只返回 2 个对象")
	keys := map[string]time.Time{}
	for _, o := range objects {
		keys[o.Key] = o.LastModified
	}
	_, ok := keys["p1/f1"]
	require.True(t, ok)
	_, ok = keys["p1/f2/chunks/001"]
	require.True(t, ok)
	for _, o := range objects {
		require.False(t, o.LastModified.IsZero(), "LastModified 必须非零")
	}
}

// TestMinio_GetMissingObjectMapsErrObjectNotFound：对象缺失经 ToErrorResponse 的
// 结构化 Code（NoSuchKey/NotFound）归一为 ErrObjectNotFound 哨兵。
func TestMinio_GetMissingObjectMapsErrObjectNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	m, bucket := newMinioTestStore(t)

	_, err := m.Get(context.Background(), bucket, "no/such/key")
	require.ErrorIs(t, err, domainstorage.ErrObjectNotFound)
}

// TestMinio_StreamPrefix：StreamPrefix 与 List 枚举结果一致；缺失前缀返回空。
func TestMinio_StreamPrefix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	m, bucket := newMinioTestStore(t)
	ctx := context.Background()

	require.NoError(t, m.Put(ctx, bucket, "sp/a", bytes.NewReader([]byte("a")), 1, ""))
	require.NoError(t, m.Put(ctx, bucket, "sp/b/c", bytes.NewReader([]byte("b")), 1, ""))
	require.NoError(t, m.Put(ctx, bucket, "other/x", bytes.NewReader([]byte("c")), 1, ""))

	var keys []string
	require.NoError(t, m.StreamPrefix(ctx, bucket, "sp/", func(o domainstorage.ObjectMeta) error {
		keys = append(keys, o.Key)
		return nil
	}))
	require.Len(t, keys, 2)
	require.ElementsMatch(t, []string{"sp/a", "sp/b/c"}, keys)

	// fn 返回错误即终止枚举。
	stopped := errors.New("stop")
	require.ErrorIs(t, m.StreamPrefix(ctx, bucket, "sp/", func(domainstorage.ObjectMeta) error {
		return stopped
	}), stopped)

	// PurgePrefix 走流式路径清尾。
	purger := NewObjectPurger(m)
	n, err := purger.PurgePrefix(ctx, bucket, "sp/")
	require.NoError(t, err)
	require.Equal(t, 2, n)
	objects, err := m.List(ctx, bucket, "sp/")
	require.NoError(t, err)
	require.Empty(t, objects)
}
