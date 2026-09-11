package bunrepo

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/analytics"
)

// Analytics 查询 SQL 形状护栏（沿 analytics_ingest_repo_sqlshape_test 家族；
// 查询为原生 SQL 构造器，直接对构造器产物断言——比 hook 捕获更强：SQL 字符
// 串即交付物）。护栏点：
//   - 表名恒 schema 限定（禁止裸表名/扫 public）；
//   - 值一律 `?` 绑定参数：注入面（prop_key→props->>?、事件名→name=?、
//     事件名集合→ANY(?::text[])）无字符串拼接；fmt 注入物仅 schema 名
//     （ident 校验 + 引号转义产物）与域常量字面量；
//   - date_trunc 单位来自枚举白名单（analyticsTruncUnit）；
//   - 读事务前置语句锁形态（statement_timeout + UTC 时区）。

const shapeSchema = `"tw_shapecheck"`

func TestAnalyticsQueryPreamble_Shape(t *testing.T) {
	require.Equal(t,
		"SET LOCAL statement_timeout = '15s'; SET LOCAL TimeZone = 'UTC'",
		analyticsQueryPreamble,
		"前置语句是超时+时区钉死的固定字面量（D15；UTC 切日语义不受会话时区影响）")
}

// TestAnalyticsTimeseriesSQL_Shape：覆盖检测/rollup 序列/raw 分桶的形状锁。
func TestAnalyticsTimeseriesSQL_Shape(t *testing.T) {
	covered := analyticsDailyCoveredDaysSQL(shapeSchema)
	require.Contains(t, covered, `FROM "tw_shapecheck".analytics_daily`)
	require.Equal(t, 2, strings.Count(covered, "?"), "窗口双边界全绑定: %s", covered)

	totals := analyticsDailyTotalsSQL(shapeSchema)
	require.Contains(t, totals, `day::timestamptz AS day`, "DATE 列扫描进 time.Time 必须经 timestamptz 投影（pgdriver DATE=字符串；UTC 零点由前置 TimeZone 钉死）")
	require.Contains(t, totals, `SUM(total)`)
	require.NotContains(t, totals, "unique_users", "全事件序列不得产出跨名求和 UV（过计数），UV 由 user_days 承担: %s", totals)

	nameSeries := analyticsDailyNameSeriesSQL(shapeSchema)
	require.Contains(t, nameSeries, `day::timestamptz AS day`)
	require.Contains(t, nameSeries, `WHERE name = ? AND day >= ? AND day < ?`)
	require.Contains(t, nameSeries, "unique_users")

	userDays := analyticsUserDaySeriesSQL(shapeSchema)
	require.Contains(t, userDays, `"tw_shapecheck".analytics_user_days`)
	require.Contains(t, userDays, `day::timestamptz AS day`)
	require.Contains(t, userDays, "COUNT(*) AS unique_users", "(user_id,day) 主键 → COUNT(*) 即去重用户")

	rawNoNames := analyticsRawTimeseriesSQL(shapeSchema, "day", false)
	require.Contains(t, rawNoNames, `date_trunc('day', occurred_at)`)
	require.Contains(t, rawNoNames, `occurred_at >= ? AND occurred_at < ?`)
	require.NotContains(t, rawNoNames, "ANY(", "全事件无名字谓词: %s", rawNoNames)

	rawNames := analyticsRawTimeseriesSQL(shapeSchema, "hour", true)
	require.Contains(t, rawNames, `date_trunc('hour', occurred_at)`)
	require.Contains(t, rawNames, `AND name = ANY(?::text[])`, "事件名集合经数组字面量绑定，不拼接: %s", rawNames)
	require.Contains(t, rawNames, "COUNT(DISTINCT user_id)")
}

// TestAnalyticsTruncUnitWhitelist：date_trunc 单位只出自枚举白名单。
func TestAnalyticsTruncUnitWhitelist(t *testing.T) {
	unit, ok := analyticsTruncUnit(analytics.GranularityDay)
	require.True(t, ok)
	require.Equal(t, "day", unit)
	unit, ok = analyticsTruncUnit(analytics.GranularityHour)
	require.True(t, ok)
	require.Equal(t, "hour", unit)
	_, ok = analyticsTruncUnit(analytics.GranularityUnspecified)
	require.False(t, ok, "未指定粒度必须拒绝（不可拼入 date_trunc）")
	_, ok = analyticsTruncUnit(analytics.Granularity(99))
	require.False(t, ok, "未知枚举值必须拒绝")
}

// TestAnalyticsBreakdownSQL_Shape：拆解两条语句的注入面形状锁——prop_key
// 全部经 `props->>?` 绑定，'(unset)' 为域常量字面量。
func TestAnalyticsBreakdownSQL_Shape(t *testing.T) {
	top := analyticsBreakdownTopSQL(shapeSchema)
	require.Contains(t, top, `COALESCE(props->>?, '(unset)')`)
	require.Contains(t, top, `WHERE name = ? AND occurred_at >= ? AND occurred_at < ?`)
	require.Contains(t, top, "ORDER BY total DESC, val ASC LIMIT ?")
	require.Equal(t, 5, strings.Count(top, "?"), "prop_key/name/双边界/limit 全绑定: %s", top)
	for _, banned := range []string{"props->>'", "name = '", "LIMIT "} {
		if banned == "LIMIT " {
			require.Contains(t, top, "LIMIT ?")
			continue
		}
		require.NotContains(t, top, banned, "禁止字面量拼接值: %s", top)
	}

	rest := analyticsBreakdownRestSQL(shapeSchema)
	// 主查询 + Top-N 子查询各持一套绑定参数（name/边界/prop_key ×2 + limit）。
	require.Equal(t, 9, strings.Count(rest, "?"), "rest 语句绑定参数计数: %s", rest)
	require.Contains(t, rest, "NOT IN (SELECT val FROM")
	require.Contains(t, rest, "COUNT(DISTINCT user_id)", "__other__ UV 必须整体去重（非差值近似）")
}

// TestAnalyticsRetentionSQL_Shape：矩阵语句展开 D0–D14（整数/别名来自域常量）
// 且 only 绑定参数是 cohort 双边界。
func TestAnalyticsRetentionSQL_Shape(t *testing.T) {
	sqlText := analyticsRetentionMatrixSQL(shapeSchema)
	require.Contains(t, sqlText, `fs.first_day::timestamptz AS cohort`, "cohort 列经 timestamptz 投影（DATE→time.Time 扫描口径）")
	require.Contains(t, sqlText, `"tw_shapecheck".analytics_user_first_seen fs`)
	require.Contains(t, sqlText, `JOIN "tw_shapecheck".analytics_user_days du`)
	require.Contains(t, sqlText, "du.day BETWEEN fs.first_day AND fs.first_day + 14")
	require.Equal(t, 2, strings.Count(sqlText, "?"), "仅 cohort 双边界绑定，D+k 为域常量整数: %s", sqlText)
	for k := 0; k < 15; k++ {
		require.Contains(t, sqlText, "fs.first_day + "+strconv.Itoa(k), "D%d 列缺失", k)
	}
	require.Contains(t, sqlText, "COUNT(DISTINCT fs.user_id) AS size")
}

// TestAnalyticsUserEventsSQL_Shape：keyset 分页语句各段组合（值全绑定）。
func TestAnalyticsUserEventsSQL_Shape(t *testing.T) {
	// 首页无窗口。
	sqlText, args := analyticsUserEventsSQL(shapeSchema, analytics.UserEventsQuery{UserID: "u1", Limit: 20})
	require.Contains(t, sqlText, `FROM "tw_shapecheck".analytics_events`)
	require.Contains(t, sqlText, "WHERE user_id = ?")
	require.Contains(t, sqlText, "ORDER BY occurred_at DESC, id DESC LIMIT ?")
	require.NotContains(t, sqlText, "occurred_at <")
	require.Len(t, args, 2)

	// 全段（窗口 + 游标）。
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	cursorAt := start.Add(12 * time.Hour)
	sqlText, args = analyticsUserEventsSQL(shapeSchema, analytics.UserEventsQuery{
		UserID:      "u1",
		PeriodStart: &start,
		PeriodEnd:   &end,
		Cursor:      &analytics.UserEventCursor{OccurredAt: cursorAt, ID: 42},
		Limit:       20,
	})
	require.Contains(t, sqlText, "occurred_at >= ?")
	require.Contains(t, sqlText, "occurred_at < ?")
	require.Contains(t, sqlText, "(occurred_at < ? OR (occurred_at = ? AND id < ?))", "keyset 去重续页谓词")
	require.Len(t, args, 7, "user_id + 双边界 + 游标三元组 + limit")
	require.Equal(t, 21, args[len(args)-1], "LIMIT 绑定值为 Limit+1 探测行")
}

// TestAnalyticsDefinitionsAndKpiSQL_Shape：字典/概览/Top/覆盖类语句。
func TestAnalyticsDefinitionsAndKpiSQL_Shape(t *testing.T) {
	defs := analyticsListDefinitionsSQL(shapeSchema)
	require.Contains(t, defs, "ORDER BY last_seen DESC, name ASC")
	require.Contains(t, defs, "total_30d")
	require.Contains(t, defs, "LIMIT ? OFFSET ?")

	count := analyticsCountDefinitionsSQL(shapeSchema)
	require.Equal(t, `SELECT COUNT(*) FROM "tw_shapecheck".analytics_event_definitions`, count)

	newUsers := analyticsCountNewUsersSQL(shapeSchema)
	require.Contains(t, newUsers, "analytics_user_first_seen")
	require.Equal(t, 2, strings.Count(newUsers, "?"))

	active := analyticsCountActiveUsersSQL(shapeSchema)
	require.Contains(t, active, "COUNT(DISTINCT user_id)", "窗口 UV 必须整体去重（不可按日求和）")

	today := analyticsRawTodayStatsSQL(shapeSchema)
	require.Contains(t, today, `occurred_at >= ?`)
	require.Equal(t, 1, strings.Count(today, "?"))

	kpi := analyticsRawOverviewKPISQL(shapeSchema)
	require.Contains(t, kpi, "COUNT(DISTINCT user_id)")
	require.Equal(t, 2, strings.Count(kpi, "?"))

	topDaily := analyticsTopEventsFromDailySQL(shapeSchema)
	require.Contains(t, topDaily, `FROM "tw_shapecheck".analytics_daily`)
	require.Contains(t, topDaily, "LIMIT ?")

	topRaw := analyticsRawTopEventsSQL(shapeSchema)
	require.Contains(t, topRaw, `FROM "tw_shapecheck".analytics_events`)
	require.Equal(t, 3, strings.Count(topRaw, "?"))
}

// TestAnalyticsPgTextArrayLiteral：数组字面量编码（转义 + 大括号形态）。
func TestAnalyticsPgTextArrayLiteral(t *testing.T) {
	require.Equal(t, `{}`, pgTextArrayLiteral(nil))
	require.Equal(t, `{"a","b"}`, pgTextArrayLiteral([]string{"a", "b"}))
	require.Equal(t, `{"he said \"hi\""}`, pgTextArrayLiteral([]string{`he said "hi"`}))
	require.Equal(t, `{"a\\b"}`, pgTextArrayLiteral([]string{`a\b`}))
}
