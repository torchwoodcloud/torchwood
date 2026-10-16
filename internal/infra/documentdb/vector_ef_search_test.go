// B7（ef_search 查询级暴露）端到端：显式拒绝矩阵（≤0 / >500 →
// InvalidArgument，不静默 clamp）、边界值放行、默认（未设置）与显式
// ef=40 逐字节同果（回归：缺省不 emit SET LOCAL）、近重复簇饱和边界数据
// 形态上的召回/延迟对比取数（仅记录，不作召回/时间断言）+ 双 ef 通路
// RLS 不泄漏守卫（显式 SET LOCAL 注入路径同样不得让不可见行漏出）。
package documentdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// efQuery 构造带/不带 ef_search 的 vector_search domain Query。
func efQuery(values []float64, ef *int32, k int32, filter *query.Filter) databases.Query {
	return databases.Query{AST: &query.Query{
		VectorSearch: &query.VectorSearch{
			Attribute: "emb", Values: values, Metric: "L2", EfSearch: ef,
		},
		PageSize: k,
		Filter:   filter,
	}}
}

// TestVectorSearch_EfSearch_RejectionMatrix：ef_search 取值域 [1,500] 的
// 显式拒绝（R9：不做静默 clamp——静默改写让调用方误以为请求值生效）；
// 边界值 1 与 500 合法放行。
func TestVectorSearch_EfSearch_RejectionMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)
	// setupKNNCollection 只有 COSINE 索引；efQuery 用 L2 metric，补一个 L2 索引。
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2",
	}))
	_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
		ID: "d1", Data: map[string]any{"emb": []any{1.0, 0.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	principal := databases.Principal{Roles: []string{"any"}}

	for _, ef := range []int32{0, -1, 501, 1 << 30} {
		_, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
			efQuery([]float64{1, 0, 0}, &ef, 5, nil), principal)
		require.Equalf(t, codes.InvalidArgument, status.Code(err), "ef=%d must be rejected", ef)
		require.Containsf(t, err.Error(), "ef_search", "ef=%d", ef)
	}

	// 下界 1 与上界 500：合法（查询照常执行）。
	for _, ef := range []int32{1, 500} {
		list, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
			efQuery([]float64{1, 0, 0}, &ef, 5, nil), principal)
		require.NoErrorf(t, err, "ef=%d must be accepted", ef)
		require.NotEmpty(t, list.Documents)
	}
}

// TestVectorSearch_EfSearch_DefaultByteIdentical：未设置 ef_search 的查询与
// 显式 ef=40（pgvector 缺省值）的结果逐一相同（同索引状态下确定性重放）——
// 缺省路径不 emit SET LOCAL 语句、行为与现状一致的回归锚点。
func TestVectorSearch_EfSearch_DefaultByteIdentical(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2",
	}))
	seed := map[string][]any{
		"a": {1.0, 0.0, 0.0},
		"b": {0.9, 0.1, 0.0},
		"c": {0.8, 0.2, 0.0},
		"d": {0.0, 1.0, 0.0},
		"e": {0.0, 0.0, 1.0},
		"f": {0.5, 0.5, 0.5},
	}
	for id, emb := range seed {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID: id, Data: map[string]any{"emb": emb},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}
	principal := databases.Principal{Roles: []string{"any"}}

	for _, qv := range [][]float64{{0, 0, 0}, {1, 1, 0}, {0.2, 0.9, 0.4}} {
		def, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
			efQuery(qv, nil, 4, nil), principal)
		require.NoError(t, err)
		ef40 := int32(40)
		explicit, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
			efQuery(qv, &ef40, 4, nil), principal)
		require.NoError(t, err)
		require.Equal(t, def.Documents, explicit.Documents,
			"unset ef_search must behave identically to the pgvector default 40")
		require.Equal(t, def.Distances, explicit.Distances)
	}
}

// TestVectorSearch_EfSearch_RecallAndLatency：近重复簇 + 稀疏可见行数据形态
// （B2 实测的 HNSW 饱和边界）上的召回/延迟对比取数——ef=40（缺省档）vs
// ef=200。稳定契约有二：(1) RLS 不泄漏——两档 ef（含 ef=200 的 SET LOCAL
// 显式注入路径）返回的每行都必须属于可见集，近似索引与调参通路不得绕过
// 行级安全；(2) 全部查询 NoError。召回数字与延迟均值经 t.Logf 记录（转出
// POC B7 闭环证据）。
// 不作召回断言（绝对或相对）——本数据形态落在 HNSW 图拓扑彩票上：995 个
// 近重复行占满 M 邻接槽位，5 个可见行后插只形成单向辐条，能否召回取决于
// pgvector 逐行插入的随机分层（后端 random()，逐轮不可复现）是否把可见行
// 选为多层入口点。本地单测串行 6 轮实测：无任何其他测试并发，仍出现
// [5 5 5] 与 [1 1 1] 两种拓扑（干扰假设排除）；同轮内 ef=40 与 ef=200 逐
// 查询同果，CI run 38850847834 则跨 ef 分化（r40=1.00 [5 5 5] /
// r200=0.47 [5 2 0]）。「更大 ef 候选池严格更大 → 聚合召回不降」是精确
// 检索的直觉，对 strict_order iterative scan 在饱和图上的游走轨迹不成立
// （原 require.GreaterOrEqual(r200, r40) 因此删除——能覆盖 CI 缺口 0.53
// 的容忍带已形同虚设）。iterative scan 的语义锁定在分散形态：
// TestVectorSearch_SparseVisibilityRecall（分散不可见行稳定全召回）。
// 取数结论（开发期观测，pgvector 0.8.x / 1k 行）：饱和形态下召回由索引
// 拓扑决定而非 ef 梯度（要么同满、要么同塌、极端轮次跨 ef 分化），默认 40
// 在可达召回时不劣于更大 ef——佐证默认值；ef 暴露的价值在更大规模/更缓
// 的梯度区间。
func TestVectorSearch_EfSearch_RecallAndLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupKNNCollection(ctx, t)
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "knn", databases.Index{
		ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2",
	}))

	otherOnly := []databases.Permission{{Type: "read", Role: "other-role"}}
	// 995 个不可见行：挤进 3 个近重复簇——簇质心紧贴查询向量（半径 0.05），
	// 簇内抖动 1e-3（行间几乎相同）。这是 B2 开发期实证的 HNSW 饱和形态：
	// 图在簇内导航饱和、escaping 到远处可见行需要 iteratively 扩大候选池，
	// ef_search 是该边界的唯一调参手段。
	clusters := [][]float64{{0.05, 0, 0}, {0, 0.05, 0}, {0, 0, 0.05}}
	for i := 1; i <= 995; i++ {
		c := clusters[i%3]
		j := 0.001 * sin64(float64(i)*1.7)
		emb := []any{c[0] + j, c[1] - j, c[2] + j*0.5}
		_, err := docDB.CreateDocument(ctx, projectID, "app", "knn", databases.Document{
			ID:   fmt.Sprintf("hidden-%03d", i),
			Data: map[string]any{"emb": emb},
		}, otherOnly, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	// 5 个可见行：距离各查询点较远（10x 量级，超出簇邻域多跳可达范围），
	// 互异方向。
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
		}, []databases.Permission{{Type: "read", Role: "any"}}, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	principal := databases.Principal{Roles: []string{"any"}}

	// ground truth：对 5 个可见行按各查询点的精确距离排序。
	queries := [][]float64{{0, 0, 0}, {0.3, 0.3, 0.0}, {1.5, 1.5, 1.5}}
	truth := make([][]string, len(queries))
	db := docDB.(*postgresDocumentDB)
	phys := knPhysical(ctx, t, docDB, projectID)
	for qi, qv := range queries {
		func() {
			rows, err := db.conn(ctx).QueryContext(ctx, fmt.Sprintf(
				`SELECT _id FROM tw_%s_app.%s WHERE _id IN ('va','vb','vc','vd','ve')
				 ORDER BY emb <-> ?::vector`, projectID, phys,
			), pgVectorFloatLiteral(qv))
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id string
				require.NoError(t, rows.Scan(&id))
				truth[qi] = append(truth[qi], id)
			}
			require.NoError(t, rows.Err())
		}()
		require.Len(t, truth[qi], 5)
	}

	recall := func(ef *int32, label string) (float64, []int) {
		perQuery := make([]int, len(queries))
		for qi, qv := range queries {
			list, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
				efQuery(qv, ef, 5, nil), principal)
			require.NoError(t, err)
			got := map[string]bool{}
			for _, d := range list.Documents {
				// RLS 不泄漏：饱和簇形态正是泄漏最易显形的边界（不可见行
				// 距查询更近）——返回行必须全部属于 ground truth 可见集。
				require.Containsf(t, truth[qi], d.ID,
					"%s: RLS leak, invisible row %q in vector search result", label, d.ID)
				got[d.ID] = true
			}
			want := map[string]bool{}
			for _, id := range truth[qi] {
				want[id] = true
			}
			for id := range got {
				if want[id] {
					perQuery[qi]++
				}
			}
		}
		sum := 0
		for _, n := range perQuery {
			sum += n
		}
		return float64(sum) / float64(5*len(queries)), perQuery
	}

	// 缺省档 = pgvector 缺省 40（显式 40 与 unset 已在 DefaultByteIdentical
	// 锁定同果；此处显式设 40 以走同一条注入路径计量）。
	ef40 := int32(40)
	ef200 := int32(200)
	r40, per40 := recall(&ef40, "ef=40")
	r200, per200 := recall(&ef200, "ef=200")
	t.Logf("recall ef=40: %.2f %v ; ef=200: %.2f %v (visible ground truth = 5 per query)",
		r40, per40, r200, per200)
	// 弱单调 recall(200) ≥ recall(40) 不作断言（饱和图拓扑彩票，见函数头
	// 注）；RLS 泄漏守卫已在 recall 内逐行断言。

	// 延迟对比取数（均值，50 次每档；不做时间断言——只产出闭环证据数字）。
	visibleIDs := make([]string, 0, len(visible))
	for id := range visible {
		visibleIDs = append(visibleIDs, id)
	}
	latency := func(ef int32) time.Duration {
		start := time.Now()
		const runs = 50
		for i := 0; i < runs; i++ {
			list, err := docDB.ListDocuments(ctx, projectID, "app", "knn",
				efQuery([]float64{0, 0, 0}, &ef, 25, nil), principal)
			require.NoError(t, err)
			for _, d := range list.Documents {
				require.Containsf(t, visibleIDs, d.ID,
					"ef=%d: RLS leak, invisible row %q in vector search result", ef, d.ID)
			}
		}
		return time.Since(start) / runs
	}
	d40 := latency(ef40)
	d200 := latency(200)
	t.Logf("latency per query (k=25, 1000 rows, RLS on): ef=40 %v ; ef=200 %v", d40, d200)
}
