package bunrepo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/uptrace/bun"
)

// Analytics 摄入 SQL 形状护栏（沿 project_repo_sqlshape_test 家族技法：
// render-only DB，Exec 建连失败但 BeforeQuery 已携带渲染语句）。护栏点：
//   - 多行单语句 INSERT（D2 热路径：一次往返）；
//   - INSERT 列白名单精确等于登记集（排除 BIGSERIAL id——bun 无 serial
//     特判，混入即烧序列号语义漂移）；
//   - 字典 upsert 的 conflict 目标与 SET 形状（first_seen 不回退）；
//   - 表名恒 schema 限定（禁止扫 public）。

func analyticsTestTableExpr(table, alias string) (string, bun.Ident) {
	return "?." + table + " AS " + alias, bun.Ident("tw_shapecheck")
}

// insertColumnsOf 提取 INSERT 语句目标列清单（首个括号段，逗号切分，
// 去除 bun 渲染的标识符引号）。
func insertColumnsOf(t *testing.T, query string) []string {
	t.Helper()
	open := strings.Index(query, "(")
	require.NotEqual(t, -1, open, "语句缺少列清单: %s", query)
	close := strings.Index(query, ")")
	require.NotEqual(t, -1, close, "语句列清单未闭合: %s", query)
	require.Greater(t, close, open, "括号顺序异常: %s", query)
	section := query[open+1 : close]
	cols := make([]string, 0, 12)
	for _, c := range strings.Split(section, ",") {
		cols = append(cols, strings.Trim(strings.TrimSpace(c), `"`))
	}
	return cols
}

// TestAnalyticsInsertEvents_SQLShape：事件批量 INSERT 的形状锁。
func TestAnalyticsInsertEvents_SQLShape(t *testing.T) {
	bunDB, hook := newRenderOnlyDB(t)
	expr, sch := analyticsTestTableExpr(analyticsEventsTable, "ae")

	rows := []model.AnalyticsEvent{
		{Name: "level_complete", UserID: "u1", SessionID: "s1", Source: analytics.SourceClient,
			OccurredAt: time.Date(2026, 9, 11, 1, 0, 0, 0, time.UTC),
			IngestedAt: time.Date(2026, 9, 11, 1, 0, 1, 0, time.UTC),
			Props:      json.RawMessage(`{"level":3}`)},
		{Name: "level_complete", UserID: "u2", Source: analytics.SourceServer,
			OccurredAt: time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC),
			IngestedAt: time.Date(2026, 9, 11, 2, 0, 1, 0, time.UTC),
			Props:      json.RawMessage(`{}`)},
		{Name: "ad_watch", UserID: "u3", Source: analytics.SourceClient,
			OccurredAt: time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC),
			IngestedAt: time.Date(2026, 9, 11, 3, 0, 1, 0, time.UTC),
			Props:      json.RawMessage(`{}`)},
	}
	_ = insertAnalyticsEvents(context.Background(), bunDB, expr, sch, rows)

	q := hook.capturedSQL()
	require.NotEmpty(t, q)
	// schema 限定的单条 INSERT 语句。
	require.True(t, strings.HasPrefix(q, `INSERT INTO "tw_shapecheck".analytics_events`),
		"表名必须 schema 限定且为 analytics_events: %s", q)
	// 列白名单精确等于登记集：无 id（BIGSERIAL 库分配）、无白名单外列。
	require.Equal(t, analyticsEventInsertColumns, insertColumnsOf(t, q),
		"INSERT 列集合必须精确等于白名单（含 id 即烧序列号语义漂移）")
	require.NotContains(t, insertColumnsOf(t, q), "id")
	// 多行单语句：3 行 = 3 个 VALUES 组（bun 以 "), (" 分隔多行）。
	require.Equal(t, 3, strings.Count(q, "), (")+1,
		"3 行必须渲染为单条多 VALUES 语句: %s", q)
	// 值全部来自行数据（无 DEFAULT 占位——bun 对 default 标记零值列会渲染
	// DEFAULT 并附加 RETURNING，本模型已去 default 标签锁定该形态）。
	require.NotContains(t, q, "DEFAULT")
	require.NotContains(t, q, "RETURNING")
}

// TestAnalyticsUpsertDefinitions_SQLShape：字典 upsert 的形状锁
// （conflict 目标 name；SET 仅推进 last_seen，first_seen 不回退）。
func TestAnalyticsUpsertDefinitions_SQLShape(t *testing.T) {
	bunDB, hook := newRenderOnlyDB(t)
	expr, sch := analyticsTestTableExpr(analyticsEventDefinitionsTable, "aed")

	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	rows := []model.AnalyticsEventDefinition{
		{Name: "level_complete", FirstSeen: now, LastSeen: now},
		{Name: "ad_watch", FirstSeen: now, LastSeen: now},
	}
	_ = upsertAnalyticsEventDefinitions(context.Background(), bunDB, expr, sch, rows)

	q := hook.capturedSQL()
	require.NotEmpty(t, q)
	require.True(t, strings.HasPrefix(q, `INSERT INTO "tw_shapecheck".analytics_event_definitions`),
		"表名必须 schema 限定且为 analytics_event_definitions: %s", q)
	require.Equal(t, []string{"name", "first_seen", "last_seen"}, insertColumnsOf(t, q),
		"字典 INSERT 列集合必须精确等于白名单（total_30d 由 rollup 维护）")
	require.Contains(t, q, "ON CONFLICT (name) DO UPDATE")
	require.Contains(t, q, "SET last_seen = GREATEST(aed.last_seen, EXCLUDED.last_seen)")
	require.NotContains(t, q, "first_seen = ",
		"first_seen 必须保持首见不回退（不得进 SET）")
	require.NotContains(t, q, "total_30d",
		"total_30d 由 rollup worker 维护（PR5），摄入路径不得触碰")
}

// TestAnalyticsCountDefinitions_SQLShape：软上限判定量读全表计数，表名
// schema 限定。
func TestAnalyticsCountDefinitions_SQLShape(t *testing.T) {
	bunDB, hook := newRenderOnlyDB(t)
	expr, sch := analyticsTestTableExpr(analyticsEventDefinitionsTable, "aed")

	_, _ = countAnalyticsEventDefinitions(context.Background(), bunDB, expr, sch)
	q := hook.capturedSQL()
	require.Contains(t, q, "count(*)")
	require.Contains(t, q, `"tw_shapecheck".analytics_event_definitions`)
}

// TestAnalyticsMergeEventDefinitions：批内同名防御性合并（同语句两行命中
// 同一冲突行会报 "cannot affect row a second time"）。
func TestAnalyticsMergeEventDefinitions(t *testing.T) {
	t0 := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	t2 := t0.Add(2 * time.Hour)
	got := mergeEventDefinitions([]analytics.EventDefinition{
		{Name: "b", FirstSeen: t1, LastSeen: t1},
		{Name: "a", FirstSeen: t0, LastSeen: t1},
		{Name: "a", FirstSeen: t1, LastSeen: t2}, // 同名：first 更早者胜、last 更晚者胜
	})
	require.Len(t, got, 2)
	require.Equal(t, "b", got[0].Name) // 首次出现顺序
	require.Equal(t, "a", got[1].Name)
	require.Equal(t, t0, got[1].FirstSeen)
	require.Equal(t, t2, got[1].LastSeen)
}
