// B13c（物理名进程内缓存，阶段②挂账 + postgres_catalog.go 预决策 4）：
// resolvePhysicalTable 点查（catalog 主键索引）单次延迟取数（n=1000），对比
// 业务查询（ListDocuments 全链：resolve + COUNT + SELECT×RLS）总耗时占比。
// 判据：占比可忽略（<5%）→ 记录关闭；否则实现缓存 + 失效桥接测试。
// 结论回写 15-exit-poc B13c。
package documentdb

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// TestResolvePhysicalTable_PointQueryShare：物理名点查延迟 vs 业务查询总耗时
// 占比取数（SystemPrincipal 路径 = 最小分母的保守口径：resolve + COUNT +
// SELECT；非 System 主体另有 GetCollection 放大分母）。只取数不断言时间
// ——结论按判据人工回写门禁单。
func TestResolvePhysicalTable_PointQueryShare(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "views", Key: "views", Type: "integer"}}, nil, nil, true))
	ids := []string{"d0", "d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9"}
	for i, id := range ids {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
			ID: id, Data: map[string]any{"views": i + 1},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	p := docDB.(*postgresDocumentDB)

	const (
		n      = 1000
		warmup = 50
	)
	// 预热（连接池、catalog 页缓存、plan cache）。
	for i := 0; i < warmup; i++ {
		_, _, _, err := p.resolvePhysicalTable(ctx, projectID, "app", "posts")
		require.NoError(t, err)
	}

	start := time.Now()
	for i := 0; i < n; i++ {
		_, _, _, err := p.resolvePhysicalTable(ctx, projectID, "app", "posts")
		require.NoError(t, err)
	}
	resolveMean := time.Since(start) / n

	q := databases.Query{AST: &query.Query{PageSize: 50}}
	start = time.Now()
	for i := 0; i < n; i++ {
		list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", q, databases.SystemPrincipal)
		require.NoError(t, err)
		require.Len(t, list.Documents, len(ids))
	}
	listMean := time.Since(start) / n

	share := 100 * float64(resolveMean) / float64(listMean)
	t.Logf("resolvePhysicalTable: %v/op (n=%d) ; ListDocuments total: %v/op (n=%d) ; share = %.1f%%",
		resolveMean, n, listMean, n, share)
}
