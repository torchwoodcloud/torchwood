package projectschema_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

// TestApply_AnalyticsPartitioning 验证 000019 analytics_events 的分区形态
// （设计 §5 / PR1 验收）：月 RANGE 父表、静态预建 2026-09/2026-10 + DEFAULT
// 兜底分区、父表四索引级联子分区、写入按 occurred_at 路由到正确分区、
// 重复 Apply 幂等。
func TestApply_AnalyticsPartitioning(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	// 重复 Apply 幂等（版本表断点续跑，二次 Apply 不产生差异）。
	require.NoError(t, projectschema.Apply(ctx, db, projectID))

	schema, err := ident.ProjectSchemaName(projectID)
	require.NoError(t, err)
	quoted := testutil.CatalogQuoted(projectID)

	// ① 父表为 RANGE(occurred_at) 分区表（relkind 'p'）。
	var relkind, partkey string
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT c.relkind, pg_get_partkeydef(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ? AND c.relname = 'analytics_events'`, schema).
		Scan(&relkind, &partkey))
	require.Equal(t, "p", relkind, "analytics_events 应为分区父表")
	require.Contains(t, partkey, "RANGE", partkey)
	require.Contains(t, partkey, "occurred_at", partkey)

	// ② 三个子分区：2026-09（当月）、2026-10（次月）与 DEFAULT 兜底。
	// 注意 relkind='r'：分区索引同样 relispartition=true，需排除。
	type partRow struct {
		name  string
		bound string
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname, pg_get_expr(c.relpartbound, c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = ? AND c.relkind = 'r' AND c.relispartition AND c.relname LIKE 'analytics_events_%'
		ORDER BY c.relname`, schema)
	require.NoError(t, err)
	parts := map[string]string{}
	for rows.Next() {
		var pr partRow
		require.NoError(t, rows.Scan(&pr.name, &pr.bound))
		parts[pr.name] = pr.bound
	}
	require.NoError(t, rows.Err())
	require.Len(t, parts, 3, "静态预建 2026-09/2026-10 + DEFAULT 共三分区: %v", parts)
	require.Contains(t, parts["analytics_events_2026_09"], "FROM ('2026-09-01 00:00:00", parts["analytics_events_2026_09"])
	require.Contains(t, parts["analytics_events_2026_09"], "TO ('2026-10-01 00:00:00", parts["analytics_events_2026_09"])
	require.Contains(t, parts["analytics_events_2026_10"], "FROM ('2026-10-01 00:00:00", parts["analytics_events_2026_10"])
	require.Contains(t, parts["analytics_events_default"], "DEFAULT", parts["analytics_events_default"])

	// ③ 父表四索引 + PK 级联到每个子分区（PG11+ partitioned index：父建
	// 即级联，子分区各持 5 个有效索引，定义与父一致）。
	for _, part := range []string{"analytics_events_2026_09", "analytics_events_2026_10", "analytics_events_default"} {
		idxRows, err := db.QueryContext(ctx, `
			SELECT indexdef FROM pg_indexes
			WHERE schemaname = ? AND tablename = ?`, schema, part)
		require.NoError(t, err)
		var defs []string
		for idxRows.Next() {
			var d string
			require.NoError(t, idxRows.Scan(&d))
			defs = append(defs, d)
		}
		require.NoError(t, idxRows.Err())
		require.Len(t, defs, 5, "%s 应级联 PK + 四索引", part)
		requireJoined(t, defs, "USING brin (occurred_at)", part+" BRIN")
		requireJoined(t, defs, "USING gin (props jsonb_path_ops)", part+" GIN jsonb_path_ops")
		requireJoined(t, defs, "(name, occurred_at DESC)", part+" name+time")
		requireJoined(t, defs, "(user_id, occurred_at DESC)", part+" user+time")
	}

	// ④ 写入路由：occurred_at 落 2026-09 → 当月分区；窗外的远期时间 →
	// DEFAULT 兜底（钳制窗保证 DEFAULT 仅竞态，D4/D5）。id 由 BIGSERIAL
	// 默认分配（列白名单排除 id 的运行时对照）。
	_, err = db.ExecContext(ctx, `INSERT INTO `+quoted+`.analytics_events
		(name, user_id, occurred_at, props) VALUES
		('level_complete', 'u1', '2026-09-11T01:02:03Z', '{"level":3}'),
		('level_complete', 'u1', '2026-12-25T00:00:00Z', '{}')`)
	require.NoError(t, err)

	routed := map[string]int{}
	for part, where := range map[string]string{
		"analytics_events_2026_09": "occurred_at = '2026-09-11T01:02:03Z'",
		"analytics_events_default": "occurred_at = '2026-12-25T00:00:00Z'",
	} {
		var n int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM `+quoted+`.`+part+` WHERE `+where).Scan(&n), part)
		routed[part] = n
	}
	require.Equal(t, 1, routed["analytics_events_2026_09"], "当月事件应路由到 2026-09 分区")
	require.Equal(t, 1, routed["analytics_events_default"], "窗外事件应落入 DEFAULT 兜底分区")

	// BIGSERIAL 调试序生效（库分配，非零递增）。
	var ids []int64
	idRows, err := db.QueryContext(ctx,
		`SELECT id FROM `+quoted+`.analytics_events ORDER BY occurred_at`)
	require.NoError(t, err)
	for idRows.Next() {
		var id int64
		require.NoError(t, idRows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, idRows.Err())
	require.Len(t, ids, 2)
	require.NotZero(t, ids[0])
	require.NotZero(t, ids[1])
	require.NotEqual(t, ids[0], ids[1], "BIGSERIAL 应递增分配")
}

// requireJoined 断言 defs 中存在包含 sub 的索引定义。
func requireJoined(t *testing.T, defs []string, sub, what string) {
	t.Helper()
	for _, d := range defs {
		if strings.Contains(d, sub) {
			return
		}
	}
	t.Errorf("%s 索引缺失（期望包含 %q，实际 %v）", what, sub, defs)
}
