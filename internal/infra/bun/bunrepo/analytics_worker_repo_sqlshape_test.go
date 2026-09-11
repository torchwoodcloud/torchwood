package bunrepo

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Analytics worker 面 SQL 形状护栏（沿 analytics_ingest_repo_sqlshape_test
// 家族技法）。护栏点：
//   - rollup upsert 冲突分支整体替换（重跑不翻倍，D7 红线：禁止出现
//     `total = analytics_daily.total +` 之类的累加语义）；
//   - user_days/first_seen 只消费归属事件（user_id <> ''）；
//   - 表名恒 schema 限定；值全部绑定参数；
//   - DDL 分区名/日期字面量经形状正则约束；
//   - tombstone 清洗语句有界（LIMIT）且重删幂等。

func TestAnalyticsRollupDaily_SQLShape(t *testing.T) {
	q := analyticsRollupDailySQL(`"tw_shapecheck"`)
	require.True(t, strings.HasPrefix(q, `INSERT INTO "tw_shapecheck".analytics_daily`),
		"表名必须 schema 限定且为 analytics_daily: %q", q)
	require.Contains(t, q, "SELECT ?::date AS day, name, COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users")
	require.Contains(t, q, "FROM \"tw_shapecheck\".analytics_events WHERE occurred_at >= ? AND occurred_at < ?")
	require.Contains(t, q, "GROUP BY name")
	require.Contains(t, q, "ON CONFLICT (day, name) DO UPDATE")
	// 覆盖语义：SET 全部取 EXCLUDED（重跑不翻倍），禁止累加。
	require.Contains(t, q, "SET total = EXCLUDED.total, unique_users = EXCLUDED.unique_users, updated_at = EXCLUDED.updated_at")
	require.NotContains(t, q, "total + ", "daily 覆盖写禁止累加语义（D7）")
	require.NotContains(t, q, "total = analytics_daily.total")
	// updated_at 必须随覆盖写推进。
	require.Contains(t, q, "updated_at = EXCLUDED.updated_at")
}

func TestAnalyticsRollupUserDays_SQLShape(t *testing.T) {
	q := analyticsRollupUserDaysSQL(`"tw_shapecheck"`)
	require.True(t, strings.HasPrefix(q, `INSERT INTO "tw_shapecheck".analytics_user_days`), q)
	require.Contains(t, q, "user_id <> ''", "空归属事件不得进用户粒度表")
	require.Contains(t, q, "ON CONFLICT (user_id, day) DO UPDATE")
	require.Contains(t, q, "SET events = EXCLUDED.events")
	require.NotContains(t, q, "events + ", "user_days 覆盖写禁止累加语义（D7）")
	require.NotContains(t, q, "user_id, ?::date, ''", "禁止空归属行")
}

func TestAnalyticsRollupFirstSeen_SQLShape(t *testing.T) {
	q := analyticsRollupFirstSeenSQL(`"tw_shapecheck"`)
	require.True(t, strings.HasPrefix(q, `INSERT INTO "tw_shapecheck".analytics_user_first_seen`), q)
	require.Contains(t, q, "ON CONFLICT (user_id) DO UPDATE")
	require.Contains(t, q, "first_day = LEAST(analytics_user_first_seen.first_day, EXCLUDED.first_day)")
	require.Contains(t, q, "last_day = GREATEST(analytics_user_first_seen.last_day, EXCLUDED.last_day)")
	require.Contains(t, q, "user_id <> ''", "空归属事件不得进用户粒度表")
}

func TestAnalyticsRefreshDefTotals_SQLShape(t *testing.T) {
	q := analyticsRefreshDefTotalsSQL(`"tw_shapecheck"`)
	require.Contains(t, q, `UPDATE "tw_shapecheck".analytics_event_definitions AS aed`)
	require.Contains(t, q, "SET total_30d = COALESCE(c.cnt, 0)", "窗口无事件的名字必须归 0（覆盖式）")
	// 值绑定参数（窗口起点），无字面量时间。
	require.Equal(t, 1, strings.Count(q, "?"), "仅 since 一个绑定参数: %q", q)
}

func TestAnalyticsCreatePartition_SQLShape(t *testing.T) {
	name := analyticsMonthlyPartitionName(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	require.Equal(t, "analytics_events_2026_12", name)
	q := analyticsCreatePartitionSQL(`"tw_shapecheck"`, name, "2026-12-01", "2027-01-01")
	require.Equal(t,
		`CREATE TABLE IF NOT EXISTS "tw_shapecheck".analytics_events_2026_12 PARTITION OF "tw_shapecheck".analytics_events FOR VALUES FROM ('2026-12-01') TO ('2027-01-01')`, q)
	// 边界字面量必须是纯日期形状（非用户输入防线：正则锚定）。
	require.Regexp(t, regexp.MustCompile(`'(\d{4}-\d{2}-\d{2})'`), q)
}

func TestAnalyticsPartitionNameGuards(t *testing.T) {
	// 合法形状 → 月份上界解析正确（整月粒度裁剪的判定基数）。
	end, ok := analyticsPartitionMonthEnd("analytics_events_2026_09")
	require.True(t, ok)
	require.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), end)
	// DEFAULT 与未知形状 → 拒绝（不进 DROP）。
	_, ok = analyticsPartitionMonthEnd("analytics_events_default")
	require.False(t, ok)
	_, ok = analyticsPartitionMonthEnd("analytics_events; DROP TABLE x")
	require.False(t, ok)
	_, ok = analyticsPartitionMonthEnd("users")
	require.False(t, ok)
	// 月份越界（13 月）→ 拒绝。
	_, ok = analyticsPartitionMonthEnd("analytics_events_2026_13")
	require.False(t, ok)
}

func TestAnalyticsPruneAndCleanup_SQLShape(t *testing.T) {
	prune := analyticsPruneUserDaysSQL(`"tw_shapecheck"`)
	require.Contains(t, prune, `DELETE FROM "tw_shapecheck".analytics_user_days WHERE ctid IN`)
	require.Contains(t, prune, "WHERE day < ?::date LIMIT ?", "user_days 修剪必须分批有界")

	pending := analyticsPendingDeletionsSQL(`"tw_shapecheck"`)
	require.Contains(t, pending, `FROM "tw_shapecheck".analytics_user_deletions WHERE done_at IS NULL`)
	require.Contains(t, pending, "ORDER BY enqueued_at ASC LIMIT ?", "清洗队列必须先进先出且有界")

	del := analyticsDeleteEventsByUserSQL(`"tw_shapecheck"`)
	require.Contains(t, del, `DELETE FROM "tw_shapecheck".analytics_events e`)
	require.Contains(t, del, "WHERE user_id = ? LIMIT ?", "raw 点删必须分批有界")
	require.Contains(t, del, "e.id = v.id AND e.occurred_at = v.occurred_at",
		"必须按 PK (id, occurred_at) 精确行定位（分区表跨分区无错删面）")

	enq := analyticsEnqueueDeletionSQL(`"tw_shapecheck"`)
	require.Contains(t, enq, `INSERT INTO "tw_shapecheck".analytics_user_deletions (user_id, enqueued_at) VALUES (?, ?)`)
	require.Contains(t, enq, "ON CONFLICT (user_id) DO NOTHING", "重删必须幂等（D10）")
}
