// vector_search 端到端（会话 #10 包 C，预决策 3/4/6）：RLS 真实路径的稀疏
// 可见行召回（iterative scan 灵魂语义——1000 行中 5 行可见仍返回 k=5 近邻
// 而非"先取全局 k 再滤"）、distances 回传与 ground truth 一致、max_distance
// 后置过滤、metric/索引/维度/算子互斥全矩阵拒绝、execute-tx data 通道可写。
package documentdb

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

func setupKNNCollection(ctx context.Context, t *testing.T) (databases.DocumentDB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "knn", "KNN", []databases.Attribute{
		{ID: "emb", Key: "emb", Type: "vector", Dims: 3},
	}, nil, nil, true))
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_cos", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "COSINE",
	}))
	return docDB, projectID
}

// knnQuery 构造 vector_search 的 domain Query。
func knnQuery(attr string, values []float64, metric string, maxDist *float64, k int32, filter *query.Filter) databases.Query {
	return databases.Query{AST: &query.Query{
		VectorSearch: &query.VectorSearch{Attribute: attr, Values: values, Metric: metric, MaxDistance: maxDist},
		PageSize:     k,
		Filter:       filter,
	}}
}

// TestVectorSearch_SparseVisibilityRecall：RLS policy 真实路径的稀疏可见行
// 召回——1000 行中仅 5 行对 principal 可见（其余 _acl 限定他角色），且全部
// 995 个不可见行比可见行更近。错误实现（iterative scan off = 先取全局
// top-5 再滤）返回 0 行；正确实现返回全部 5 个可见近邻（与 ground truth
// SQL 同序同距）。这是对本会话灵魂语义（vector 与文档同 RLS 判定管辖）的
// 锁定。
//
// 数据几何（L2 度量）：查询向量取原点，不可见行均匀铺满半径 0.05..0.5 的
// 球体（方向确定性伪随机、彼此分散），可见行在半径 0.6..1.0。注意不可见
// 行必须分散：近重复簇（全部挤在查询向量极近处）会使 HNSW 图在簇内导航
// 饱和、iterative scan 无法穿透到远处的可见行——那是 HNSW 近似的固有
// 边界而非 iterative scan 语义缺陷（开发期实证：近重复簇数据 50% 概率
// 返回 0 行；分散数据稳定全召回）。
func TestVectorSearch_SparseVisibilityRecall(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2",
	}))

	otherOnly := []databases.Permission{{Type: "read", Role: "other-role"}}
	anyRead := []databases.Permission{{Type: "read", Role: "any"}}

	// 995 个不可见行：半径 0.05..0.5 球体、方向伪随机分散。
	for i := 1; i <= 995; i++ {
		r := 0.05 + 0.45*(float64(i)/995.0)
		emb := []any{r * cos64(float64(i)*2.4), r * sin64(float64(i)*2.4), r * cos64(float64(i)*0.7)}
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID:   fmt.Sprintf("hidden-%03d", i),
			Data: map[string]any{"emb": emb},
		}, otherOnly, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	// 5 个可见行：L2 距离原点 0.6..1.0（全部比不可见行远）。
	visible := map[string][]any{
		"va": {0.6, 0.0, 0.0},
		"vb": {0.0, 0.7, 0.0},
		"vc": {0.0, 0.0, 0.8},
		"vd": {-0.9, 0.0, 0.0},
		"ve": {0.0, -1.0, 0.0},
	}
	for id, emb := range visible {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID: id, Data: map[string]any{"emb": emb},
		}, anyRead, databases.SystemPrincipal)
		require.NoError(t, err)
	}

	principal := databases.Principal{Roles: []string{"any"}}
	list, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{0, 0, 0}, "L2", nil, 5, nil), principal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 5, "iterative scan must return k=5 *visible* neighbors")
	require.Len(t, list.Distances, 5)

	// Ground truth：绕过 KNN 管道直接对 5 个可见行算距离排序（同库 SQL）。
	db := docDB.(*postgresDocumentDB)
	rows, err := db.conn(ctx).QueryContext(ctx, `
		SELECT _id, (emb <-> '[0,0,0]'::vector) AS dist
		FROM tw_`+projectID+`_app.`+knPhysical(ctx, t, docDB, projectID)+`
		WHERE _id IN ('va','vb','vc','vd','ve')
		ORDER BY dist`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var wantIDs []string
	var wantDists []float64
	for rows.Next() {
		var id string
		var dist float64
		require.NoError(t, rows.Scan(&id, &dist))
		wantIDs = append(wantIDs, id)
		wantDists = append(wantDists, dist)
	}
	require.NoError(t, rows.Err())
	require.Len(t, wantIDs, 5)

	gotIDs := make([]string, 0, len(list.Documents))
	for _, d := range list.Documents {
		gotIDs = append(gotIDs, d.ID)
	}
	require.Equal(t, wantIDs, gotIDs, "KNN result must match ground-truth distance order of visible rows")
	for i := range wantDists {
		require.InDelta(t, wantDists[i], list.Distances[i], 1e-9, "distance %d", i)
	}
	// distances 单调不减（top-k 升序）。
	for i := 1; i < len(list.Distances); i++ {
		require.LessOrEqual(t, list.Distances[i-1], list.Distances[i])
	}
}

// cos64/sin64 是确定性三角函数（测试数据生成用；math 包直接别名导出无意义，
// 显式包装表达"仅测试消费"）。
func cos64(x float64) float64 { return math.Cos(x) }
func sin64(x float64) float64 { return math.Sin(x) }

func knPhysical(ctx context.Context, t *testing.T, docDB databases.DocumentDB, projectID string) string {
	t.Helper()
	db := docDB.(*postgresDocumentDB)
	var physical string
	err := db.conn(ctx).QueryRowContext(ctx, `
		SELECT physical_name FROM catalog_collections
		WHERE project_id = ? AND database_id = 'app' AND collection_id = 'knn'`, projectID).Scan(&physical)
	require.NoError(t, err)
	return physical
}

// TestVectorSearch_FilterAndMaxDistance：普通 filter 可组合（AND）；max_distance
// 后置过滤（top-k 中距离超阈值的行被剔除，documents/distances 同步）。
func TestVectorSearch_FilterAndMaxDistance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)

	seed := map[string][]any{
		"a": {1.0, 0.0, 0.0}, // dist 0
		"b": {0.7, 0.7, 0.0}, // dist ~0.293
		"c": {0.0, 1.0, 0.0}, // dist 1
	}
	for id, emb := range seed {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID: id, Data: map[string]any{"emb": emb},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}
	principal := databases.Principal{Roles: []string{"any"}}

	// 无阈值 top-2。
	list, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "COSINE", nil, 2, nil), principal)
	require.NoError(t, err)
	require.Equal(t, []string{"a", "b"}, []string{list.Documents[0].ID, list.Documents[1].ID})

	// max_distance=0.1：top-2 中仅 a 保留（阈值不引入新行——b 距离 > 0.1）。
	md := 0.1
	list, err = docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "COSINE", &md, 2, nil), principal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "a", list.Documents[0].ID)
	require.Len(t, list.Distances, 1)

	// 与普通 filter 组合（AND）：equal 限定 _id。
	list, err = docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "COSINE", nil, 3, query.Eq("_id", "c")), principal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "c", list.Documents[0].ID)
}

// TestVectorSearch_Metrics：L2 与 INNER_PRODUCT 的 KNN 路径（三 metric 索引
// 与算子映射）。
func TestVectorSearch_Metrics(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2",
	}))
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_ip", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "INNER_PRODUCT",
	}))

	_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
		ID: "near", Data: map[string]any{"emb": []any{1.0, 0.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	_, err = docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
		ID: "far", Data: map[string]any{"emb": []any{0.0, 1.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	principal := databases.Principal{Roles: []string{"any"}}

	// L2：near 距 [1,0,0] 为 0，far 为 √2 ≈ 1.4142（<-> 是欧氏距离）。
	list, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "L2", nil, 2, nil), principal)
	require.NoError(t, err)
	require.Equal(t, "near", list.Documents[0].ID)
	require.InDelta(t, 0.0, list.Distances[0], 1e-9)
	require.Equal(t, "far", list.Documents[1].ID)
	require.InDelta(t, 1.4142135623730951, list.Distances[1], 1e-9)

	// INNER_PRODUCT：<#> = 负内积；[1,0,0]·[1,0,0]=1 → -1；[1,0,0]·[0,1,0]=0 → 0（更"远"）。
	list, err = docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "INNER_PRODUCT", nil, 2, nil), principal)
	require.NoError(t, err)
	require.Equal(t, "near", list.Documents[0].ID)
	require.InDelta(t, -1.0, list.Distances[0], 1e-9)
	require.InDelta(t, 0.0, list.Distances[1], 1e-9)
}

// TestVectorSearch_RejectionMatrix：前置校验全矩阵——非 vector 列/维度不匹配/
// 无匹配 metric 索引/vector 进普通 filter（非 isNull）/vector 进 order/
// count 组合。
func TestVectorSearch_RejectionMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)
	// 集合只有 COSINE 索引（setup 建立）。
	_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
		ID: "s1", Data: map[string]any{"emb": []any{1.0, 0.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	principal := databases.Principal{Roles: []string{"any"}}

	// 维度不匹配（2 维 vs dims=3）。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0}, "COSINE", nil, 5, nil), principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "expected 3")

	// metric 无匹配索引（L2 索引未建——本用例集合仅 COSINE）。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "INNER_PRODUCT", nil, 5, nil), principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "requires an hnsw index")

	// 非 vector 列。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "knn",
		knnQuery("_id", []float64{1, 0, 0}, "COSINE", nil, 5, nil), principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "requires a vector attribute")

	// vector 进普通 filter（非 isNull/isNotNull）。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "knn", databases.Query{AST: &query.Query{
		Filter: query.Eq("emb", "x"),
	}}, principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "isNull/isNotNull")

	// isNull/isNotNull 是 vector 属性唯一合法的普通 filter。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "knn", databases.Query{AST: &query.Query{
		Filter: query.IsNull("emb"),
	}}, principal)
	require.NoError(t, err)

	// vector 进 order。
	_, err = docDB.ListDocuments(ctx, projectID, "app", "knn", databases.Query{AST: &query.Query{
		Orders: []query.Order{{Attribute: "emb", Desc: true}},
	}}, principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "cannot be an order key")

	// count/aggregate 是整集语义，KNN top-k 不相容。
	_, err = docDB.CountDocuments(ctx, projectID, "app", "knn",
		knnQuery("emb", []float64{1, 0, 0}, "COSINE", nil, 5, nil), principal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "vector_search is not supported on count")
}

// TestVectorSearch_ExecuteTxDataChannel：execute-tx 的 data 通道对 vector 列
// 照常可写（复用 createDocument 编码，无特殊化——"明确不做"清单的确认项）。
func TestVectorSearch_ExecuteTxDataChannel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)

	results, err := docDB.ExecuteTransactions(ctx, projectID, "app", []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "knn", DocumentID: "tx1",
			Data:        map[string]any{"emb": []any{0.5, 0.5, 0.5}},
			Permissions: anyPerms()},
	}, databases.TransactionModeAtomic, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.True(t, results[0].OK)
	// 读回确认 vector 列经 execute-tx 落库且形态为 JSON 数组。
	got, err := docDB.GetDocument(ctx, projectID, "app", "knn", "tx1", databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{0.5, 0.5, 0.5}, got.Data["emb"])
}

// ---------------------------------------------------------------------------
// B2 多页 KNN：kvc: 距离游标
// ---------------------------------------------------------------------------

// setupKNNPageCollection 建三 metric 共存的分页测试集合：emb_c（COSINE）/
// emb_l2（L2）/ emb_ip（INNER_PRODUCT）各配 hnsw 索引，另有 grp 字符串属性
// 供 filter 组合用例。
func setupKNNPageCollection(ctx context.Context, t *testing.T) (databases.DocumentDB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "kpage", "KNN Pagination", []databases.Attribute{
		{ID: "emb_c", Key: "emb_c", Type: "vector", Dims: 3},
		{ID: "emb_l2", Key: "emb_l2", Type: "vector", Dims: 3},
		{ID: "emb_ip", Key: "emb_ip", Type: "vector", Dims: 3},
		{ID: "grp", Key: "grp", Type: "string"},
	}, nil, nil, true))
	for _, idx := range []databases.Index{
		{ID: "c_cos", Type: "hnsw", Attributes: []string{"emb_c"}, DistanceMetric: "COSINE"},
		{ID: "c_l2", Type: "hnsw", Attributes: []string{"emb_l2"}, DistanceMetric: "L2"},
		{ID: "c_ip", Type: "hnsw", Attributes: []string{"emb_ip"}, DistanceMetric: "INNER_PRODUCT"},
	} {
		require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "kpage", idx))
	}
	return docDB, projectID
}

// knnPage 驱动一次带 token 的 KNN 查询。
func knnPage(ctx context.Context, t *testing.T, docDB databases.DocumentDB, projectID, collection string, q databases.Query, token string) *databases.DocumentList {
	t.Helper()
	q.AST.PageToken = token
	list, err := docDB.ListDocuments(ctx, projectID, "app", collection, q, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	return list
}

// knnPaginateAll 翻页至尽：拼接 ids/distances，锁定每页 ≤ k、跨页无重复、
// 翻页有限终止。
func knnPaginateAll(ctx context.Context, t *testing.T, docDB databases.DocumentDB, projectID, collection string, q databases.Query, k int32) ([]string, []float64) {
	t.Helper()
	var ids []string
	var dists []float64
	seen := map[string]bool{}
	token := ""
	for page := 0; ; page++ {
		require.Less(t, page, 64, "pagination did not terminate")
		list := knnPage(ctx, t, docDB, projectID, collection, q, token)
		require.LessOrEqual(t, int32(len(list.Documents)), k, "page %d overflows k", page)
		for i, d := range list.Documents {
			require.Falsef(t, seen[d.ID], "duplicate document %s across pages", d.ID)
			seen[d.ID] = true
			ids = append(ids, d.ID)
			dists = append(dists, list.Distances[i])
		}
		if list.NextPageToken == "" {
			return ids, dists
		}
		token = list.NextPageToken
	}
}

// TestVectorSearch_Pagination_DeterministicStitch（B2 完成判据）：多页拼接 ==
// 单页大 k 全序（不重不漏，确定性几何锁定）。三 metric 各一例：数据几何使
// 三种距离序同为 p01→p12 且距离严格递增；INNER_PRODUCT 一并锁定 <#> 负内积
// 方向——"越大越好"的内积经 pgvector 取负后"越小越近"，与 cosine/L2 的续页
// 阈值方向（严格大于游标）一致，无需按 metric 翻转。
func TestVectorSearch_Pagination_DeterministicStitch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNPageCollection(ctx, t)

	// 12 行确定性几何：三 metric 的距离序都是 p01 最近 → p12 最远，且同
	// metric 内距离互异（tie 组语义由 TieGroup 用例单独锁定）。
	const n = 12
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("p%02d", i)
		theta := 0.08 * float64(i)
		_, err := docDB.CreateDocument(ctx, projectID, "app", "kpage", databases.Document{
			ID: id,
			Data: map[string]any{
				"emb_c":  []any{math.Cos(theta), math.Sin(theta), 0.0},
				"emb_l2": []any{0.1 * float64(i), 0.0, 0.0},
				"emb_ip": []any{1.0 - 0.05*float64(i), 0.0, 0.0},
				"grp":    "hit",
			},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}

	wantIDs := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		wantIDs = append(wantIDs, fmt.Sprintf("p%02d", i))
	}

	cases := []struct {
		name   string
		attr   string
		metric string
		vec    []float64
	}{
		{"cosine", "emb_c", "COSINE", []float64{1, 0, 0}},
		{"l2", "emb_l2", "L2", []float64{0, 0, 0}},
		{"inner_product", "emb_ip", "INNER_PRODUCT", []float64{1, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			k := int32(3)
			q := knnQuery(tc.attr, tc.vec, tc.metric, nil, k, nil)
			gotIDs, gotDists := knnPaginateAll(ctx, t, docDB, projectID, "kpage", q, k)
			require.Equal(t, wantIDs, gotIDs, "stitched pages must equal the full distance order")

			// 单页大 k 全序对照：拼接序与 distances 逐一一致。
			qAll := knnQuery(tc.attr, tc.vec, tc.metric, nil, 100, nil)
			oneShot := knnPage(ctx, t, docDB, projectID, "kpage", qAll, "")
			oneIDs := make([]string, 0, n)
			for _, d := range oneShot.Documents {
				oneIDs = append(oneIDs, d.ID)
			}
			require.Equal(t, wantIDs, oneIDs)
			require.Len(t, oneShot.Distances, n)
			for i := range gotDists {
				require.InDeltaf(t, oneShot.Distances[i], gotDists[i], 1e-9, "distance %d (%s)", i, gotIDs[i])
			}
			// 距离严格递增（数据互异 + 全序续传）。
			for i := 1; i < len(gotDists); i++ {
				require.Lessf(t, gotDists[i-1], gotDists[i], "distance order broken at %d", i)
			}
			// INNER_PRODUCT 方向锁定：距离为负内积（负值，最相似 = 最负）。
			// 容差 1e-6：pgvector 以 float4 存储向量，距离是 float4 输入上的
			// 计算结果，与 float64 字面量存在 ~1e-7 级偏差。
			if tc.attr == "emb_ip" {
				require.Negative(t, gotDists[0])
				require.InDelta(t, -0.95, gotDists[0], 1e-6)
			}

			// 非整除页深（k=5 → 5/5/2）同样不重不漏。
			q5 := knnQuery(tc.attr, tc.vec, tc.metric, nil, 5, nil)
			got5, _ := knnPaginateAll(ctx, t, docDB, projectID, "kpage", q5, 5)
			require.Equal(t, wantIDs, got5)
		})
	}
}

// TestVectorSearch_Pagination_TieGroup：同距离 tie 组的跨页不重不漏——本用例
// 是"仅阈值谓词"朴素形态的杀手：HNSW 对同距 tie 组的取舍任意，发射其中
// 真子集 + 阈值游标会永久漏行。实现采用完整距离组切页（组不完整即整组顺延，
// 游标落组起点）+ 续页 (dist,_id) 精确全序。5 行同距 + 1 行更远：拼接必须
// 等于全集（朴素形态在此丢 2 行）。
func TestVectorSearch_Pagination_TieGroup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNPageCollection(ctx, t)

	for _, id := range []string{"t-a", "t-b", "t-c", "t-d", "t-e"} {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "kpage", databases.Document{
			ID:   id,
			Data: map[string]any{"emb_l2": []any{1.0, 0.0, 0.0}},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}
	_, err := docDB.CreateDocument(ctx, projectID, "app", "kpage", databases.Document{
		ID:   "t-f",
		Data: map[string]any{"emb_l2": []any{2.0, 0.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	// 首页 k=2：raw 全为同距 tie（距离 0），tie-trim 全裁 → 0 行 + 起点游标。
	q := knnQuery("emb_l2", []float64{1, 0, 0}, "L2", nil, 2, nil)
	first := knnPage(ctx, t, docDB, projectID, "kpage", q, "")
	require.Empty(t, first.Documents, "full page of ties must be trimmed, not partially emitted")
	require.NotEmpty(t, first.NextPageToken, "trimmed full page must issue a continuation token")

	gotIDs, gotDists := knnPaginateAll(ctx, t, docDB, projectID, "kpage", q, 2)
	require.Equal(t, []string{"t-a", "t-b", "t-c", "t-d", "t-e", "t-f"}, gotIDs,
		"tie group must be re-fetched in (_id) total order with no loss/duplication")
	require.Len(t, gotDists, 6)
	require.InDelta(t, 1.0, gotDists[len(gotDists)-1], 1e-6, "t-f is the farthest row")
}

// TestVectorSearch_Pagination_FilterSparseVisibility：filter 组合 + RLS 真实
// 路径的稀疏可见行跨页（B2 灵魂用例，同会话 #10 灵魂测试形态）——995 行
// 不可见（且更近），仅 6 行可见；带 equal(grp,"hit") 过滤翻页，拼接必须等
// 于全部可见近邻的距离序。首页 HNSW×iterative scan、续页精确全序×RLS 两段
// 管道都被此用例覆盖。
func TestVectorSearch_Pagination_FilterSparseVisibility(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "knn", "KNN", []databases.Attribute{
		{ID: "emb", Key: "emb", Type: "vector", Dims: 3},
		{ID: "grp", Key: "grp", Type: "string"},
	}, nil, nil, true))
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2",
	}))

	otherOnly := []databases.Permission{{Type: "read", Role: "other-role"}}
	// 995 个不可见行：半径 0.05..0.5 球体、方向伪随机分散（全部比可见行近
	// ——错误实现"先取全局 k 再滤"在翻页任何一页都会混入不可见行）。
	for i := 1; i <= 995; i++ {
		r := 0.05 + 0.45*(float64(i)/995.0)
		emb := []any{r * cos64(float64(i)*2.4), r * sin64(float64(i)*2.4), r * cos64(float64(i)*0.7)}
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID:   fmt.Sprintf("hidden-%03d", i),
			Data: map[string]any{"emb": emb, "grp": "x"},
		}, otherOnly, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	// 6 个可见行：L2 距离原点 0.6..1.1，互异。
	visible := map[string][]any{
		"va": {0.6, 0.0, 0.0},
		"vb": {0.0, 0.7, 0.0},
		"vc": {0.0, 0.0, 0.8},
		"vd": {-0.9, 0.0, 0.0},
		"ve": {0.0, -1.0, 0.0},
		"vf": {1.1, 0.0, 0.0},
	}
	for id, emb := range visible {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID:   id,
			Data: map[string]any{"emb": emb, "grp": "hit"},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}

	q := knnQuery("emb", []float64{0, 0, 0}, "L2", nil, 2, query.Eq("grp", "hit"))
	gotIDs, gotDists := knnPaginateAll(ctx, t, docDB, projectID, "knn", q, 2)
	require.Equal(t, []string{"va", "vb", "vc", "vd", "ve", "vf"}, gotIDs,
		"pagination must cover exactly the visible neighbors in distance order")
	require.InDelta(t, 0.6, gotDists[0], 1e-6)
	require.InDelta(t, 1.1, gotDists[len(gotDists)-1], 1e-6)

	// 单页大 k（同 filter）对照。
	qAll := knnQuery("emb", []float64{0, 0, 0}, "L2", nil, 100, query.Eq("grp", "hit"))
	oneShot := knnPage(ctx, t, docDB, projectID, "knn", qAll, "")
	oneIDs := make([]string, 0, 6)
	for _, d := range oneShot.Documents {
		oneIDs = append(oneIDs, d.ID)
	}
	require.Equal(t, gotIDs, oneIDs)
}

// TestVectorSearch_Pagination_MaxDistance：max_distance 与续页组合——每页
// 独立后置过滤（语义同首页），拼接恰为距离 ≤ 阈值的前缀；阈值排除尾页时
// 以短页/空页收尾，不发游标。
func TestVectorSearch_Pagination_MaxDistance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNPageCollection(ctx, t)

	// L2 距离原点 0.0/0.2/0.4/0.6/0.8。
	for i := 0; i <= 4; i++ {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "kpage", databases.Document{
			ID:   fmt.Sprintf("m%d", i),
			Data: map[string]any{"emb_l2": []any{0.2 * float64(i), 0.0, 0.0}},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}

	md := 0.5
	q := knnQuery("emb_l2", []float64{0, 0, 0}, "L2", &md, 2, nil)
	gotIDs, gotDists := knnPaginateAll(ctx, t, docDB, projectID, "kpage", q, 2)
	require.Equal(t, []string{"m0", "m1", "m2"}, gotIDs, "stitched pages must stop at max_distance")
	require.Len(t, gotDists, 3)
	for _, d := range gotDists {
		require.LessOrEqual(t, d, md)
	}

	// 阈值覆盖全部行：首页空 + 无游标。
	mdAll := -1.0
	qAll := knnQuery("emb_l2", []float64{0, 0, 0}, "L2", &mdAll, 2, nil)
	list := knnPage(ctx, t, docDB, projectID, "kpage", qAll, "")
	require.Empty(t, list.Documents)
	require.Empty(t, list.NextPageToken)
}

// TestVectorSearch_Pagination_RejectedTokens：异族/垃圾游标显式拒绝——
// ka:/kb: keyset token 的续传键是排序键值，与距离序不兼容；垃圾 token、
// NaN 距离、非法 docID 一律 InvalidArgument（fail-closed）。
func TestVectorSearch_Pagination_RejectedTokens(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNPageCollection(ctx, t)
	_, err := docDB.CreateDocument(ctx, projectID, "app", "kpage", databases.Document{
		ID:   "p01",
		Data: map[string]any{"emb_l2": []any{1.0, 0.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	q := knnQuery("emb_l2", []float64{1, 0, 0}, "L2", nil, 2, nil)
	for _, tok := range []string{
		"ka:p01",                       // keyset token（异族）
		"kb:p01",                       // keyset token（异族）
		"garbage",                      // 无前缀
		"kvc:nothexnothexno:p01",       // hex 段非法
		"kvc:3ff0000000000000:bad id!", // docID 不合法
		"kvc:7ff8000000000000:p01",     // NaN 距离
	} {
		list, err := docDB.ListDocuments(ctx, projectID, "app", "kpage", knnTokenQuery(q, tok), databases.Principal{Roles: []string{"any"}})
		require.Error(t, err, "token %q must be rejected", tok)
		require.Equalf(t, codes.InvalidArgument, status.Code(err), "token %q", tok)
		require.Nil(t, list)
	}
}

// knnTokenQuery 复制查询并注入 page token。
func knnTokenQuery(q databases.Query, token string) databases.Query {
	cp := q
	cp.AST = &query.Query{
		VectorSearch: q.AST.VectorSearch,
		PageSize:     q.AST.PageSize,
		PageToken:    token,
	}
	return cp
}

// TestKNNCursorTokenRoundtrip：kvc: token 编解码单元测试（无 DB）——float8
// 比特定长 hex 精确往返（含负距离/零距离）、空 docID 起点形态、异常形态
// 全部拒绝。
func TestKNNCursorTokenRoundtrip(t *testing.T) {
	for _, d := range []float64{0, 1.5, -1.5, 0.1, math.MaxFloat64, math.SmallestNonzeroFloat64, -math.MaxFloat64} {
		tok := encodeKNNCursor(d, "doc:1")
		c, ok := decodeKNNCursor(tok)
		require.Truef(t, ok, "token %q", tok)
		require.Equalf(t, d, c.dist, "token %q", tok)
		require.Equalf(t, "doc:1", c.id, "token %q", tok)
	}
	// 空 docID = 距离起点形态（首页 tie-trim 全裁发放）。
	c, ok := decodeKNNCursor(encodeKNNCursor(2.5, ""))
	require.True(t, ok)
	require.Equal(t, 2.5, c.dist)
	require.Empty(t, c.id)

	for _, tok := range []string{
		"ka:x",                        // 异族前缀
		"kvc:",                        // 缺 hex 段
		"kvc:3ff:",                    // hex 段过短
		"kvc:3ff000000000000",         // 缺 id 分隔（15 位 hex ≠ 16）
		"kvc:zzzz000000000000:x",      // hex 非法
		"kvc:7ff8000000000000:x",      // NaN
		"kvc:3ff0000000000000:bad x!", // docID 非法
	} {
		_, ok := decodeKNNCursor(tok)
		require.Falsef(t, ok, "token %q must be rejected", tok)
	}
}
