// 规模预警线 SLO 指标测试（转出 POC 门禁 B12，docs/developer/15-exit-poc.md）：
//  1. CollectScaleMetrics 三平面计数与 pg_class 独立复核查询一致（业务文档面
//     以真实 CreateDatabase+CreateCollection 造出物理表，静态面/控制面非零）；
//  2. db 为 nil 时 no-op；3. ObservePgDumpDuration 更新 pg_dump 指标骨架。
package documentdb

import (
	"context"
	"testing"
	"time"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// countPlaneTables 独立复核查询：与 scaleCountSQL 同口径逐 schema 计数
// （relkind r/p 物理表），作为指标值的对照物。
func countPlaneTables(t *testing.T, ctx context.Context, db *clients.Database, nspname string) float64 {
	t.Helper()
	var n int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname = ?`, nspname).Scan(&n))
	return float64(n)
}

func TestCollectScaleMetrics_CountsPlanes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	// 真实数据面：项目 schema（静态平面 17 表级）+ 业务库 + 用户 collection。
	p := NewPostgresDocumentDB(db, nil)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs", "Docs",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}},
		nil, []databases.Permission{{Type: "read", Role: "any"}}, true))

	res, err := CollectScaleMetrics(ctx, db)
	require.NoError(t, err)

	// 三平面均非零：控制面（public 目录表）、静态平面（系统静态表）、
	// 业务文档面（docs 物理表）。
	require.Greater(t, res.Catalog, int64(0))
	require.Greater(t, res.ProjectSchema, int64(0))
	require.GreaterOrEqual(t, res.Business, int64(1))

	// 指标值与独立 pg_class 复核查询一致（静态面按项目 schema 精确对账）。
	schema, err := ident.ProjectSchemaName(projectID)
	require.NoError(t, err)
	require.Equal(t, countPlaneTables(t, ctx, db, "public"), promtestutil.ToFloat64(ScaleTablesTotal.WithLabelValues("catalog")))
	require.Equal(t, countPlaneTables(t, ctx, db, schema), promtestutil.ToFloat64(ScaleTablesTotal.WithLabelValues("project_schema")))
	require.Equal(t, res.Catalog, int64(promtestutil.ToFloat64(ScaleTablesTotal.WithLabelValues("catalog"))))
	require.Equal(t, res.ProjectSchema, int64(promtestutil.ToFloat64(ScaleTablesTotal.WithLabelValues("project_schema"))))
	require.Equal(t, res.Business, int64(promtestutil.ToFloat64(ScaleTablesTotal.WithLabelValues("business"))))

	// 增量敏感性：再加一个 collection（DDL）后重采，business 计数 +1。
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs2", "Docs2",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}},
		nil, []databases.Permission{{Type: "read", Role: "any"}}, true))
	res2, err := CollectScaleMetrics(ctx, db)
	require.NoError(t, err)
	require.Equal(t, res.Business+1, res2.Business, "新 collection 物理表必须计入 business 平面")
}

func TestCollectScaleMetrics_NilDBNoOp(t *testing.T) {
	res, err := CollectScaleMetrics(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, ScaleCounts{}, res)
}

func TestObservePgDumpDuration_SetsGauge(t *testing.T) {
	ObservePgDumpDuration(90 * time.Second)
	require.Equal(t, 90.0, promtestutil.ToFloat64(pgDumpDurationSeconds))
}
