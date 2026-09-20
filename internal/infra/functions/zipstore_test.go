package functions

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// 本文件覆盖部署代码包持久层适配器（bucketZipStore）：缺省/自定义桶名、
// 键布局 <project>/<function>/<deployment>.zip、写路径懒 ensure、miss →
// ErrZipNotFound 哨兵映射（storage.ErrObjectNotFound 转译）、Remove 幂等。

func TestBucketZipStore_DefaultBucketAndKeyLayout(t *testing.T) {
	objects := testutil.NewMemObjectStore()
	s := NewBucketZipStore(&config.AppConfig{}, objects)

	require.NoError(t, s.Put(context.Background(), "p1", "fn_1", "dep_1", []byte("PK\x03\x04zip")))

	// 缺省桶 = domainfunctions.DefaultZipBucketName；写路径懒 ensure 建桶。
	metas, err := objects.List(context.Background(), domainfunctions.DefaultZipBucketName, "")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	require.Equal(t, "p1/fn_1/dep_1.zip", metas[0].Key, "键布局沿用本地 zip 路径结构")
}

func TestBucketZipStore_ConfigOverridesBucket(t *testing.T) {
	objects := testutil.NewMemObjectStore()
	cfg := &config.AppConfig{Functions: &config.Functions{
		Storage: &config.Functions_Storage{Bucket: "custom-fn-zips"},
	}}
	s := NewBucketZipStore(cfg, objects)

	require.NoError(t, s.Put(context.Background(), "p1", "fn_1", "dep_1", []byte("zip")))

	metas, err := objects.List(context.Background(), "custom-fn-zips", "")
	require.NoError(t, err)
	require.Len(t, metas, 1)
	defaultMetas, _ := objects.List(context.Background(), domainfunctions.DefaultZipBucketName, "")
	require.Empty(t, defaultMetas, "不落缺省桶")
}

func TestBucketZipStore_RoundTripAndNotFoundSentinel(t *testing.T) {
	s := NewBucketZipStore(&config.AppConfig{}, testutil.NewMemObjectStore())
	ctx := context.Background()

	code := []byte("PK\x03\x04code-package")
	require.NoError(t, s.Put(ctx, "p1", "fn_1", "dep_1", code))
	got, err := s.Get(ctx, "p1", "fn_1", "dep_1")
	require.NoError(t, err)
	require.Equal(t, code, got)

	// miss（键不存在）→ ErrZipNotFound 哨兵（调用方据此走回退/放弃分支，
	// 与暂态传输错误相区分）。
	_, err = s.Get(ctx, "p1", "fn_1", "dep_other")
	require.ErrorIs(t, err, domainfunctions.ErrZipNotFound)

	require.NoError(t, s.Remove(ctx, "p1", "fn_1", "dep_1"))
	require.NoError(t, s.Remove(ctx, "p1", "fn_1", "dep_1"), "Remove 幂等：删不存在对象不是错误")
	_, err = s.Get(ctx, "p1", "fn_1", "dep_1")
	require.ErrorIs(t, err, domainfunctions.ErrZipNotFound)
}

func TestBucketZipStore_EmptyIDsRejected(t *testing.T) {
	s := NewBucketZipStore(&config.AppConfig{}, testutil.NewMemObjectStore())
	ctx := context.Background()

	err := s.Put(ctx, "", "fn_1", "dep_1", []byte("zip"))
	require.Error(t, err)
	_, err = s.Get(ctx, "p1", "", "dep_1")
	require.Error(t, err)
	err = s.Remove(ctx, "p1", "fn_1", "")
	require.Error(t, err)
}

// TestBucketZipStore_TransportErrorNotMappedAsNotFound 非 miss 形态的取回
// 错误不得被误映射为 ErrZipNotFound（误判会让暂态故障走「放弃重建」分支）。
func TestBucketZipStore_TransportErrorNotMappedAsNotFound(t *testing.T) {
	s := NewBucketZipStore(&config.AppConfig{}, &errObjectStore{MemObjectStore: testutil.NewMemObjectStore()})
	_, err := s.Get(context.Background(), "p1", "fn_1", "dep_1")
	require.Error(t, err)
	require.False(t, errors.Is(err, domainfunctions.ErrZipNotFound))
}

// errObjectStore 是 Get 恒定传输错误的 ObjectStore 桩（其余方法内存语义）。
type errObjectStore struct {
	*testutil.MemObjectStore
}

func (errObjectStore) Get(context.Context, string, string) (io.ReadCloser, error) {
	return nil, errors.New("connection reset by peer")
}
