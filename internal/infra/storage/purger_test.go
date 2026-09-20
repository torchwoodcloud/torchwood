package storage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// deleteInjectingStore 包装内存对象存储：按 key 注入 Delete 失败。
type deleteInjectingStore struct {
	*testutil.MemObjectStore
	deleteErrs map[string]error
}

func (s *deleteInjectingStore) Delete(_ context.Context, bucket, key string) error {
	if err, ok := s.deleteErrs[key]; ok {
		return err
	}
	return s.MemObjectStore.Delete(context.Background(), bucket, key)
}

// streamingStore 包装内存对象存储，提供 PrefixStreamer（模拟 minio 流式枚举），
// 供 Purger 的类型断言选中流式路径。
type streamingStore struct {
	deleteInjectingStore
	streamCalls int
}

func (s *streamingStore) StreamPrefix(ctx context.Context, bucket, prefix string, fn func(domainstorage.ObjectMeta) error) error {
	s.streamCalls++
	objects, err := s.List(ctx, bucket, prefix)
	if err != nil {
		return err
	}
	for _, o := range objects {
		if err := fn(o); err != nil {
			return err
		}
	}
	return nil
}

// putTestObjects 在内存库放 n 个前缀下的对象。
func putTestObjects(t *testing.T, store *testutil.MemObjectStore, n int) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, store.EnsureBucket(ctx, domainstorage.DefaultBucketName))
	for i := 0; i < n; i++ {
		require.NoError(t, store.Put(ctx, domainstorage.DefaultBucketName,
			"pfx/obj-"+string(rune('a'+i)), strings.NewReader("x"), 1, ""))
	}
}

// P2 修复第 8 项：未提供 PrefixStreamer 的 store（回退 List 路径）全量清理成功。
func TestPurgePrefix_FallbackListPath(t *testing.T) {
	mem := testutil.NewMemObjectStore()
	putTestObjects(t, mem, 3)
	p := NewObjectPurger(&deleteInjectingStore{MemObjectStore: mem})

	n, err := p.PurgePrefix(context.Background(), domainstorage.DefaultBucketName, "pfx/")
	require.NoError(t, err)
	require.Equal(t, 3, n)

	objects, err := mem.List(context.Background(), domainstorage.DefaultBucketName, "pfx/")
	require.NoError(t, err)
	require.Empty(t, objects)
}

// 流式路径：PrefixStreamer 被选中（streamCalls==1），单对象删除失败累计跳过、
// 继续清理其余对象，末尾汇总错误携带失败 key 与成功/失败计数。
func TestPurgePrefix_StreamedPathContinuesPastDeleteFailures(t *testing.T) {
	mem := testutil.NewMemObjectStore()
	putTestObjects(t, mem, 4)
	s := &streamingStore{
		deleteInjectingStore: deleteInjectingStore{
			MemObjectStore: mem,
			deleteErrs: map[string]error{
				"pfx/obj-a": errors.New("s3 slow down"),
				"pfx/obj-c": errors.New("s3 timeout"),
			},
		},
	}
	p := NewObjectPurger(s)

	n, err := p.PurgePrefix(context.Background(), domainstorage.DefaultBucketName, "pfx/")
	require.Error(t, err, "存在删除失败时必须汇总报错")
	require.Equal(t, 2, n, "失败的 2 个对象跳过后其余照常清理")
	require.Equal(t, 1, s.streamCalls, "应走流式枚举路径")

	require.Contains(t, err.Error(), "deleted 2 objects, 2 deletions failed")
	require.Contains(t, err.Error(), "pfx/obj-a")
	require.Contains(t, err.Error(), "pfx/obj-c")
}

// 汇总错误明细条数封顶（防大前缀长故障时错误消息无限膨胀），计数仍如实。
func TestPurgePrefix_SummaryCapsListedErrors(t *testing.T) {
	mem := testutil.NewMemObjectStore()
	putTestObjects(t, mem, 12)
	s := &streamingStore{
		deleteInjectingStore: deleteInjectingStore{
			MemObjectStore: mem,
			deleteErrs:     map[string]error{},
		},
	}
	// 12 个 key 全部注入失败（key 名在 putTestObjects 中为 pfx/obj-a..l）。
	for _, c := range "abcdefghijkl" {
		s.deleteErrs["pfx/obj-"+string(c)] = errors.New("boom")
	}
	p := NewObjectPurger(s)

	n, err := p.PurgePrefix(context.Background(), domainstorage.DefaultBucketName, "pfx/")
	require.Error(t, err)
	require.Zero(t, n)
	require.Contains(t, err.Error(), "12 deletions failed")
	require.Equal(t, 10, strings.Count(err.Error(), "delete object "), "明细至多列出 10 条")
}

// 流式枚举中 ctx 取消 → 枚举终止且错误透传。
func TestPurgePrefix_StreamedPathStopsOnContextCancel(t *testing.T) {
	mem := testutil.NewMemObjectStore()
	putTestObjects(t, mem, 2)
	s := &streamingStore{deleteInjectingStore: deleteInjectingStore{MemObjectStore: mem}}
	p := NewObjectPurger(s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.PurgePrefix(ctx, domainstorage.DefaultBucketName, "pfx/")
	require.ErrorIs(t, err, context.Canceled)
}
