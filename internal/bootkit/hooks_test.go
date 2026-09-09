// 启动钩子接线测试（转出 POC 门禁 A1 / B12）：CollectionGrantsReconcileHook 与
// ScaleMetricsHook 必须注册进 NewOnStarts（cmd/server 与 cmd/worker 共享装配），
// 且执行钩子等价于执行对应采集——A1 构造授权偏离终态的表跑完钩子后恢复终态，
// B12 执行后三平面 tables_total 指标刷新为当前库真实计数。
package bootkit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"testing"

	promtestutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

func TestCollectionGrantsReconcileHook_WiredInOnStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	// 构造授权故意偏离终态的业务集合表。
	p := documentdb.NewPostgresDocumentDB(db, nil)
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 0)
	t.Cleanup(cleanup)
	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs", "Docs",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}},
		nil, []databases.Permission{{Type: "read", Role: "any"}}, true))
	schema, err := ident.SchemaName(projectID, "app")
	require.NoError(t, err)
	var physical string
	require.NoError(t, db.NewSelect().TableExpr("public.catalog_collections").
		Column("physical_name").
		Where("project_id = ? AND database_id = ? AND collection_id = ?", projectID, "app", "docs").
		Scan(ctx, &physical))
	tbl := schema + "." + physical
	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`GRANT UPDATE (_acl) ON %s TO tw_app`, tbl))
	require.NoError(t, err, "偏离播种：R13a 旧形态多授")

	// 接线断言：grants reconcile 以闭包注入（R15 集中复审：bootkit 不 import
	// documentdb——reconcile 实现移至 cmd/server 组合根，经 NewOnStarts 可选
	// 参数注入；此处以直调 documentdb 的闭包复现 server 侧注入形态）。
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reconcile := func(ctx context.Context) error {
		_, err := documentdb.ReconcileCollectionColumnGrants(ctx, db)
		return err
	}
	hooks := NewOnStarts(nil, db, logger, reconcile, nil, nil)
	require.Len(t, hooks, 2, "NewOnStarts 必须包含注入的 reconcile 钩子（A1 接线锁定；基础钩子 = 项目 schema 确保一项——roles 密钥同步钩子已随 B15 退役）")
	for i, hook := range hooks {
		require.NoError(t, hook(ctx), "hook %d", i)
	}

	// 钩子执行后偏离被矫正：column_privileges 回到终态（UPDATE 排除 _acl）。
	rows, err := db.QueryContext(ctx,
		`SELECT column_name || ':' || privilege_type
		 FROM information_schema.column_privileges
		 WHERE table_schema = ? AND table_name = ? AND grantee = 'tw_app'
		 ORDER BY column_name, privilege_type`, schema, physical)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var grants []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		grants = append(grants, s)
	}
	require.NoError(t, rows.Err())
	sort.Strings(grants)
	require.NotContains(t, grants, "_acl:UPDATE", "钩子执行后 R13a 旧形态多授必须被收回")
	require.Contains(t, grants, "title:UPDATE", "钩子执行后数据列 UPDATE 授权必须在场")
}

// TestScaleMetricsHook_WiredInOnStarts 锁定门禁 B12 的钩子接线：规模预警线
// 表计数采集以闭包注入（与 A1 同形态——cmd/server 组合根直调 documentdb，
// 经 NewOnStarts 的 scaleMetrics 参数注入），执行钩子等价于执行采集——
// 三平面 tables_total 指标被刷新为当前库的真实计数。
func TestScaleMetricsHook_WiredInOnStarts(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	// 造业务文档面物理表，让 business 平面计数有非平凡对照物。
	p := documentdb.NewPostgresDocumentDB(db, nil)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "docs", "Docs",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}},
		nil, []databases.Permission{{Type: "read", Role: "any"}}, true))
	businessSchema, err := ident.SchemaName(projectID, "app")
	require.NoError(t, err)
	var wantBusiness int64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p') AND n.nspname = ?`, businessSchema).Scan(&wantBusiness))
	require.GreaterOrEqual(t, wantBusiness, int64(1))

	// 接线断言：未注入扩展钩子时仅 1 个基础钩子（nil 跳过语义；roles 密钥
	// 同步钩子已随 B15 退役）；注入 scaleMetrics 闭包后为 2 个，执行后指标
	// 被刷新。
	require.Len(t, NewOnStarts(nil, nil, nil, nil, nil, nil), 1, "nil 扩展钩子必须被跳过（基础钩子 = 项目 schema 确保一项）")
	scale := func(ctx context.Context) error {
		_, err := documentdb.CollectScaleMetrics(ctx, db)
		return err
	}
	hooks := NewOnStarts(nil, db, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, scale, nil)
	require.Len(t, hooks, 2, "NewOnStarts 必须包含注入的 scaleMetrics 钩子（B12 接线锁定）")
	for i, hook := range hooks {
		require.NoError(t, hook(ctx), "hook %d", i)
	}
	require.Equal(t, wantBusiness, int64(promtestutil.ToFloat64(documentdb.ScaleTablesTotal.WithLabelValues("business"))),
		"钩子执行后 business 平面计数必须与 pg_class 复核查询一致")
}
