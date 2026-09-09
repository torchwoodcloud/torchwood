package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
)

// TestValidateIndex_HNSWMetric（会话 #10 预决策 2）：hnsw 索引的 metric
// 校验——单列/orders 拒绝/metric 白名单/缺省 COSINE 合法；非 hnsw 设置
// distance_metric 拒绝。vector 属性本体校验见
// TestCreateAttribute_VectorDims。
func TestValidateIndex_HNSWMetric(t *testing.T) {
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)

	// 三 metric + 缺省 COSINE 均合法。
	for _, idx := range []databases.Index{
		{ID: "a", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "COSINE"},
		{ID: "b", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2"},
		{ID: "c", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "INNER_PRODUCT"},
		{ID: "d", Type: "hnsw", Attributes: []string{"emb"}},
	} {
		require.NoError(t, uc.ValidateIndex(idx), "metric=%s", idx.DistanceMetric)
	}

	for _, tc := range []struct {
		idx  databases.Index
		want string
	}{
		{
			idx:  databases.Index{ID: "m", Type: "hnsw", Attributes: []string{"a", "b"}, DistanceMetric: "COSINE"},
			want: "exactly one attribute",
		},
		{
			idx:  databases.Index{ID: "m", Type: "hnsw", Attributes: []string{"emb"}, Orders: []string{"desc"}},
			want: "does not support orders",
		},
		{
			idx:  databases.Index{ID: "m", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "MANHATTAN"},
			want: "distance_metric must be COSINE, L2, or INNER_PRODUCT",
		},
		{
			idx:  databases.Index{ID: "m", Type: "key", Attributes: []string{"emb"}, DistanceMetric: "COSINE"},
			want: "only valid for hnsw",
		},
		{
			idx:  databases.Index{ID: "m", Type: "unique", Attributes: []string{"emb"}, DistanceMetric: "L2"},
			want: "only valid for hnsw",
		},
	} {
		err := uc.ValidateIndex(tc.idx)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "type=%s metric=%s", tc.idx.Type, tc.idx.DistanceMetric)
		require.Contains(t, err.Error(), tc.want)
	}
}

// TestCreateAttribute_VectorDims（会话 #10 预决策 1）：dims 域 2..2000、
// default/array 拒绝、非 vector 设 dims 拒绝（app 层口径；adapter 二道
// 防线由 documentdb 集成测试覆盖）。
func TestCreateAttribute_VectorDims(t *testing.T) {
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)
	ctx := platformAdminCtx(context.Background())

	for _, attr := range []databases.Attribute{
		{Key: "v", Type: "vector"},
		{Key: "v", Type: "vector", Dims: 1},
		{Key: "v", Type: "vector", Dims: 2001},
		{Key: "v", Type: "vector", Dims: 3, Default: "x"},
		{Key: "v", Type: "vector", Dims: 3, Array: true},
		{Key: "v", Type: "string", Dims: 3},
	} {
		err := uc.CreateAttribute(ctx, "proj-1", "app", "coll", attr)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "attr=%+v", attr)
	}
}
