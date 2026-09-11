package bunrepo

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

var _ analytics.QueryRepository = (*AnalyticsQueryRepository)(nil)

// AnalyticsQueryRepository 是 analytics.QueryRepository 的 bun 实现
// （PR3 查询面，docs/design/analytics.md §6）。纪律：
//   - 每方法一个读事务，事务首语句钉死语句超时与会话时区（analyticsQueryPreamble）；
//   - SQL 由 *SQL 构造器产出（形状护栏测试的锚点）：表名恒 schema 限定
//     （fmt 仅注入经 ident 校验+引号转义的 schema 名），值一律 `?` 绑定参数；
//   - prop_key / 事件名经用例层白名单正则后仍走绑定参数（props->>? / name = ?），
//     事件名集合经 pgTextArrayLiteral 编码后以 ANY(?::text[]) 绑定（沿
//     documentdb pgTextArray 先例）。
type AnalyticsQueryRepository struct {
	db *clients.Database
}

// NewAnalyticsQueryRepository 构造查询仓储。
func NewAnalyticsQueryRepository(db *clients.Database) *AnalyticsQueryRepository {
	return &AnalyticsQueryRepository{db: db}
}

// analyticsQueryPreamble 是查询读事务的首语句（simple protocol 单次往返）：
//   - SET LOCAL statement_timeout = 15s（D15：慢查询显式报错不挂死）；
//   - SET LOCAL TimeZone = 'UTC'：date_trunc('day', timestamptz) 的切日与
//     DATE 列比较（date ↔ timestamptz 隐式转换）钉死在 UTC（v1 UTC 切日语义，
//     不受服务器/连接时区影响）。SET LOCAL 事务结束自动失效，零连接残留。
const analyticsQueryPreamble = "SET LOCAL statement_timeout = '15s'; SET LOCAL TimeZone = 'UTC'"

// readTx 打开查询读事务：projectschema Apply（存量项目补齐 analytics 表，
// 就绪缓存命中直通）→ RunInTx → 前置语句 → fn（conn 即事务连接）。
func (r *AnalyticsQueryRepository) readTx(ctx context.Context, projectID string, fn func(ctx context.Context, conn bun.IDB, schema string) error) error {
	if _, _, _, err := Scoped(ctx, r.db, projectID, analyticsDailyTable, "ad"); err != nil {
		return err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn := r.db.Conn(txCtx)
		if _, err := conn.ExecContext(txCtx, analyticsQueryPreamble); err != nil {
			return fmt.Errorf("analytics query preamble: %w", err)
		}
		return fn(txCtx, conn, quoted)
	})
}

// DailyCoveredDays 覆盖检测（保守：零事件日无 daily 行 → 计为未覆盖 → 调用方
// 回退 raw，恒正确）。
func (r *AnalyticsQueryRepository) DailyCoveredDays(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) (int, error) {
	var n int
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		return conn.QueryRowContext(ctx, analyticsDailyCoveredDaysSQL(schema),
			startDay, endDayExclusive).Scan(&n)
	})
	return n, err
}

// DailySeries rollup 按日序列：name 空集 = 全事件总量（UniqueUsers 置零——
// 跨名并集 UV 不可从 daily 导出，调用方走 UserDaySeries）；单名 = 精确
// total/unique_users（(day,name) 主键行直读）。
func (r *AnalyticsQueryRepository) DailySeries(ctx context.Context, projectID, name string, startDay, endDayExclusive time.Time) ([]analytics.DailyPoint, error) {
	var out []analytics.DailyPoint
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		if name == "" {
			rows, err := conn.QueryContext(ctx, analyticsDailyTotalsSQL(schema), startDay, endDayExclusive)
			if err != nil {
				return err
			}
			defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
			for rows.Next() {
				var p analytics.DailyPoint
				if err := rows.Scan(&p.Day, &p.Total); err != nil {
					return err
				}
				out = append(out, p)
			}
			return rows.Err()
		}
		rows, err := conn.QueryContext(ctx, analyticsDailyNameSeriesSQL(schema), name, startDay, endDayExclusive)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var p analytics.DailyPoint
			if err := rows.Scan(&p.Day, &p.Total, &p.UniqueUsers); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// UserDaySeries user_days 按日活跃用户数（(user_id, day) 主键 → COUNT(*) 即
// 去重用户；全事件并集 UV 的精确基座，D6）。
func (r *AnalyticsQueryRepository) UserDaySeries(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) ([]analytics.DailyPoint, error) {
	var out []analytics.DailyPoint
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		rows, err := conn.QueryContext(ctx, analyticsUserDaySeriesSQL(schema), startDay, endDayExclusive)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var p analytics.DailyPoint
			if err := rows.Scan(&p.Day, &p.UniqueUsers); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// CountActiveUsers 窗口去重用户数（跨日不重复计数）。
func (r *AnalyticsQueryRepository) CountActiveUsers(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) (int64, error) {
	var n int64
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		return conn.QueryRowContext(ctx, analyticsCountActiveUsersSQL(schema),
			startDay, endDayExclusive).Scan(&n)
	})
	return n, err
}

// CountNewUsers first_seen 表窗口行数（表空 = 0，不报错——PR5 前的预期态）。
func (r *AnalyticsQueryRepository) CountNewUsers(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) (int64, error) {
	var n int64
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		return conn.QueryRowContext(ctx, analyticsCountNewUsersSQL(schema),
			startDay, endDayExclusive).Scan(&n)
	})
	return n, err
}

// TopEventsFromDaily 窗口 Top 事件（daily 精确口径）。
func (r *AnalyticsQueryRepository) TopEventsFromDaily(ctx context.Context, projectID string, startDay, endDayExclusive time.Time, limit int) ([]analytics.TopEvent, error) {
	var out []analytics.TopEvent
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		rows, err := conn.QueryContext(ctx, analyticsTopEventsFromDailySQL(schema), startDay, endDayExclusive, limit)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var e analytics.TopEvent
			if err := rows.Scan(&e.Name, &e.Total); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ListEventDefinitions 字典分页（last_seen DESC, name ASC——total_30d 排序键
// 由 rollup 维护，PR5 起刷新；当前 last_seen 是最接近的活性序）+ 总数。
func (r *AnalyticsQueryRepository) ListEventDefinitions(ctx context.Context, projectID string, offset, limit int) ([]analytics.EventDefinitionInfo, int, error) {
	var (
		out   []analytics.EventDefinitionInfo
		total int
	)
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		if err := conn.QueryRowContext(ctx, analyticsCountDefinitionsSQL(schema)).Scan(&total); err != nil {
			return err
		}
		if total == 0 || offset >= total {
			return nil
		}
		rows, err := conn.QueryContext(ctx, analyticsListDefinitionsSQL(schema), limit, offset)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var d analytics.EventDefinitionInfo
			if err := rows.Scan(&d.Name, &d.FirstSeen, &d.LastSeen, &d.Total30d); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// RawTimeseries raw 分桶聚合（names 空 = 全事件；occurred_at 谓词触发月分区
// 裁剪；DAY/HOUR 的 date_trunc 单位经枚举白名单映射，非调用方字符串）。
func (r *AnalyticsQueryRepository) RawTimeseries(ctx context.Context, projectID string, names []string, start, end time.Time, granularity analytics.Granularity) ([]analytics.TimeseriesPoint, error) {
	unit, ok := analyticsTruncUnit(granularity)
	if !ok {
		return nil, fmt.Errorf("unsupported granularity %d", granularity)
	}
	var out []analytics.TimeseriesPoint
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		var (
			rows *sql.Rows
			err  error
		)
		if len(names) > 0 {
			rows, err = conn.QueryContext(ctx, analyticsRawTimeseriesSQL(schema, unit, true),
				start, end, pgTextArrayLiteral(names))
		} else {
			rows, err = conn.QueryContext(ctx, analyticsRawTimeseriesSQL(schema, unit, false), start, end)
		}
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var p analytics.TimeseriesPoint
			if err := rows.Scan(&p.Bucket, &p.Total, &p.UniqueUsers); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return out, err
}

// RawOverviewKPI raw 窗口总量 + UV（回退路径）。
func (r *AnalyticsQueryRepository) RawOverviewKPI(ctx context.Context, projectID string, start, end time.Time) (int64, int64, error) {
	var total, uv int64
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		return conn.QueryRowContext(ctx, analyticsRawOverviewKPISQL(schema), start, end).Scan(&total, &uv)
	})
	return total, uv, err
}

// RawTopEvents raw 窗口 Top 事件（回退路径）。
func (r *AnalyticsQueryRepository) RawTopEvents(ctx context.Context, projectID string, start, end time.Time, limit int) ([]analytics.TopEvent, error) {
	var out []analytics.TopEvent
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		rows, err := conn.QueryContext(ctx, analyticsRawTopEventsSQL(schema), start, end, limit)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var e analytics.TopEvent
			if err := rows.Scan(&e.Name, &e.Total); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// RawTodayStats 今日实时块（恒 raw 直读）。
func (r *AnalyticsQueryRepository) RawTodayStats(ctx context.Context, projectID string, dayStart time.Time) (analytics.TodayStats, error) {
	var s analytics.TodayStats
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		return conn.QueryRowContext(ctx, analyticsRawTodayStatsSQL(schema), dayStart).Scan(&s.TotalEvents, &s.UniqueUsers)
	})
	return s, err
}

// RawBreakdown raw 维度拆解：Top-N 桶（total DESC, val ASC 决胜稳定）+
// 其余事件整体聚合（COUNT DISTINCT 精确 UV——非 Top-N UV 差值近似）。
func (r *AnalyticsQueryRepository) RawBreakdown(ctx context.Context, projectID, name, propKey string, start, end time.Time, topN int) (top []analytics.BreakdownBucket, other *analytics.BreakdownBucket, err error) {
	err = r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		rows, qErr := conn.QueryContext(ctx, analyticsBreakdownTopSQL(schema),
			propKey, name, start, end, topN)
		if qErr != nil {
			return qErr
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var b analytics.BreakdownBucket
			if sErr := rows.Scan(&b.Value, &b.Total, &b.UniqueUsers); sErr != nil {
				return sErr
			}
			top = append(top, b)
		}
		if rErr := rows.Err(); rErr != nil {
			return rErr
		}
		// __other__：Top-N 之外维度值的精确聚合（Top-N 子查询重算，同一事务
		// 内一致）。
		var o analytics.BreakdownBucket
		if sErr := conn.QueryRowContext(ctx, analyticsBreakdownRestSQL(schema),
			name, start, end, propKey, propKey, name, start, end, topN).Scan(&o.Total, &o.UniqueUsers); sErr != nil {
			return sErr
		}
		if o.Total > 0 || o.UniqueUsers > 0 {
			other = &o
		}
		return nil
	})
	return top, other, err
}

// RetentionMatrix cohort × D0–D14 留存矩阵（D11：first_seen ⋈ user_days 自
// 连接；LEFT JOIN 行乘以 COUNT(DISTINCT) 去重）。表空 → 空切片。
func (r *AnalyticsQueryRepository) RetentionMatrix(ctx context.Context, projectID string, cohortStart, cohortEndExclusive time.Time) ([]analytics.RetentionCohort, error) {
	var out []analytics.RetentionCohort
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		rows, err := conn.QueryContext(ctx, analyticsRetentionMatrixSQL(schema),
			cohortStart, cohortEndExclusive)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		cols := make([]int64, analytics.RetentionSlots)
		for rows.Next() {
			var c analytics.RetentionCohort
			dest := make([]any, 0, 2+analytics.RetentionSlots)
			dest = append(dest, &c.Cohort, &c.Size)
			for i := range cols {
				dest = append(dest, &cols[i])
			}
			if err := rows.Scan(dest...); err != nil {
				return err
			}
			c.Retained = append([]int64(nil), cols...)
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// ListUserEvents raw keyset 分页：探测行策略（Limit+1 取回，截断到 Limit，
// hasMore = 是否截断）。
func (r *AnalyticsQueryRepository) ListUserEvents(ctx context.Context, projectID string, q analytics.UserEventsQuery) ([]analytics.UserEvent, bool, error) {
	var (
		out     []analytics.UserEvent
		hasMore bool
	)
	err := r.readTx(ctx, projectID, func(ctx context.Context, conn bun.IDB, schema string) error {
		sqlText, args := analyticsUserEventsSQL(schema, q)
		rows, err := conn.QueryContext(ctx, sqlText, args...)
		if err != nil {
			return err
		}
		defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
		for rows.Next() {
			var e analytics.UserEvent
			if err := rows.Scan(&e.ID, &e.Name, &e.OccurredAt, &e.IngestedAt, &e.Source,
				&e.Platform, &e.AppVersion, &e.SessionID, &e.Props); err != nil {
				return err
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(out) > q.Limit {
			out = out[:q.Limit]
			hasMore = true
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, hasMore, nil
}

// ---------------------------------------------------------------------------
// SQL 构造器（SQL 形状护栏测试的锚点：schema 名为唯一 fmt 注入物——经
// ident 校验 + 引号转义；值全部 `?` 绑定参数；聚合字面量（'(unset)'、
// date_trunc 单位、D+k 整数）为域常量，非用户输入）。
// ---------------------------------------------------------------------------

// DATE 列读取口径（pgdriver 把 DATE 以字符串返回，database/sql 无法直接
// Scan 进 time.Time）：SELECT 列表显式 `::timestamptz` 投影——前置语句已
// SET LOCAL TimeZone='UTC'，date→timestamptz 恒为当日 UTC 零点，扫描为
// time.Time（UTC）。WHERE 侧保持 DATE 原列（参数为 timestamptz，比较走
// 会话时区 = UTC 的隐式转换，语义不变）。

func analyticsDailyCoveredDaysSQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(DISTINCT day) FROM %s.analytics_daily WHERE day >= ? AND day < ?`, schema)
}

func analyticsDailyTotalsSQL(schema string) string {
	return fmt.Sprintf(`SELECT day::timestamptz AS day, SUM(total) AS total FROM %s.analytics_daily WHERE day >= ? AND day < ? GROUP BY day ORDER BY day ASC`, schema)
}

func analyticsDailyNameSeriesSQL(schema string) string {
	return fmt.Sprintf(`SELECT day::timestamptz AS day, total, unique_users FROM %s.analytics_daily WHERE name = ? AND day >= ? AND day < ? ORDER BY day ASC`, schema)
}

func analyticsUserDaySeriesSQL(schema string) string {
	return fmt.Sprintf(`SELECT day::timestamptz AS day, COUNT(*) AS unique_users FROM %s.analytics_user_days WHERE day >= ? AND day < ? GROUP BY day ORDER BY day ASC`, schema)
}

func analyticsCountActiveUsersSQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(DISTINCT user_id) FROM %s.analytics_user_days WHERE day >= ? AND day < ?`, schema)
}

func analyticsCountNewUsersSQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(*) FROM %s.analytics_user_first_seen WHERE first_day >= ? AND first_day < ?`, schema)
}

func analyticsTopEventsFromDailySQL(schema string) string {
	return fmt.Sprintf(`SELECT name, SUM(total) AS total FROM %s.analytics_daily WHERE day >= ? AND day < ? GROUP BY name ORDER BY total DESC, name ASC LIMIT ?`, schema)
}

func analyticsCountDefinitionsSQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(*) FROM %s.analytics_event_definitions`, schema)
}

func analyticsListDefinitionsSQL(schema string) string {
	return fmt.Sprintf(`SELECT name, first_seen, last_seen, total_30d FROM %s.analytics_event_definitions ORDER BY last_seen DESC, name ASC LIMIT ? OFFSET ?`, schema)
}

// analyticsRawTimeseriesSQL：withNames=false 无名谓词（全事件）。unit 经
// analyticsTruncUnit 白名单产出（'day'/'hour'）。
func analyticsRawTimeseriesSQL(schema, unit string, withNames bool) string {
	namePred := ""
	if withNames {
		namePred = " AND name = ANY(?::text[])"
	}
	return fmt.Sprintf(`SELECT date_trunc('%s', occurred_at) AS bucket, COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users FROM %s.analytics_events WHERE occurred_at >= ? AND occurred_at < ?%s GROUP BY 1 ORDER BY 1 ASC`, unit, schema, namePred)
}

func analyticsRawOverviewKPISQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users FROM %s.analytics_events WHERE occurred_at >= ? AND occurred_at < ?`, schema)
}

func analyticsRawTopEventsSQL(schema string) string {
	return fmt.Sprintf(`SELECT name, COUNT(*) AS total FROM %s.analytics_events WHERE occurred_at >= ? AND occurred_at < ? GROUP BY name ORDER BY total DESC, name ASC LIMIT ?`, schema)
}

func analyticsRawTodayStatsSQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users FROM %s.analytics_events WHERE occurred_at >= ?`, schema)
}

func analyticsBreakdownTopSQL(schema string) string {
	return fmt.Sprintf(`SELECT COALESCE(props->>?, '(unset)') AS val, COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users FROM %s.analytics_events WHERE name = ? AND occurred_at >= ? AND occurred_at < ? GROUP BY 1 ORDER BY total DESC, val ASC LIMIT ?`, schema)
}

func analyticsBreakdownRestSQL(schema string) string {
	return fmt.Sprintf(`SELECT COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users FROM %s.analytics_events WHERE name = ? AND occurred_at >= ? AND occurred_at < ? AND COALESCE(props->>?, '(unset)') NOT IN (SELECT val FROM (SELECT COALESCE(props->>?, '(unset)') AS val, COUNT(*) AS total FROM %s.analytics_events WHERE name = ? AND occurred_at >= ? AND occurred_at < ? GROUP BY 1 ORDER BY total DESC, val ASC LIMIT ?) top_buckets)`, schema, schema)
}

// analyticsRetentionMatrixSQL：D0–D14 列由常量循环展开（整数与别名为编译期
// 域常量，非用户输入）；LEFT JOIN 乘积行经 COUNT(DISTINCT du.user_id)
// FILTER 精确去重。
func analyticsRetentionMatrixSQL(schema string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `SELECT fs.first_day::timestamptz AS cohort, COUNT(DISTINCT fs.user_id) AS size`)
	for k := 0; k < analytics.RetentionSlots; k++ {
		fmt.Fprintf(&b, `, COUNT(DISTINCT du.user_id) FILTER (WHERE du.day = fs.first_day + %d) AS d%d`, k, k)
	}
	fmt.Fprintf(&b, ` FROM %s.analytics_user_first_seen fs LEFT JOIN %s.analytics_user_days du ON du.user_id = fs.user_id AND du.day BETWEEN fs.first_day AND fs.first_day + %d WHERE fs.first_day >= ? AND fs.first_day < ? GROUP BY fs.first_day ORDER BY fs.first_day ASC`,
		schema, schema, analytics.RetentionSlots-1)
	return b.String()
}

// analyticsUserEventsSQL 组装 keyset 分页语句（谓词按 q 的非空段拼接——
// 谓词片段为固定字面量，值全绑定）。
func analyticsUserEventsSQL(schema string, q analytics.UserEventsQuery) (string, []any) {
	var (
		sb      strings.Builder
		args    = make([]any, 0, 8)
		firstEd = true
	)
	sb.WriteString(`SELECT id, name, occurred_at, ingested_at, source, platform, app_version, session_id, props FROM `)
	sb.WriteString(schema)
	sb.WriteString(`.analytics_events`)
	addPred := func(pred string, vals ...any) {
		if firstEd {
			sb.WriteString(` WHERE `)
			firstEd = false
		} else {
			sb.WriteString(` AND `)
		}
		sb.WriteString(pred)
		args = append(args, vals...)
	}
	addPred(`user_id = ?`, q.UserID)
	if q.PeriodStart != nil {
		addPred(`occurred_at >= ?`, *q.PeriodStart)
	}
	if q.PeriodEnd != nil {
		addPred(`occurred_at < ?`, *q.PeriodEnd)
	}
	if q.Cursor != nil {
		// keyset 续页：(occurred_at, id) 严格小于游标（倒序扫描的续读点）。
		addPred(`(occurred_at < ? OR (occurred_at = ? AND id < ?))`, q.Cursor.OccurredAt, q.Cursor.OccurredAt, q.Cursor.ID)
	}
	sb.WriteString(` ORDER BY occurred_at DESC, id DESC LIMIT ?`)
	args = append(args, q.Limit+1)
	return sb.String(), args
}

// analyticsTruncUnit 粒度枚举 → date_trunc 单位白名单（非调用方字符串）。
func analyticsTruncUnit(g analytics.Granularity) (string, bool) {
	switch g {
	case analytics.GranularityHour:
		return "hour", true
	case analytics.GranularityDay:
		return "day", true
	default:
		return "", false
	}
}

// pgTextArrayLiteral 把字符串集合编码为 PG 数组字面量（作为绑定参数传入
// ANY(?::text[])；元素引号/反斜杠转义——事件名经白名单正则后不含转义字符，
// 转义为防御层）。沿 documentdb pgTextArray 先例。
func pgTextArrayLiteral(items []string) string {
	var parts []string
	for _, item := range items {
		item = strings.ReplaceAll(item, `\`, `\\`)
		item = strings.ReplaceAll(item, `"`, `\"`)
		parts = append(parts, `"`+item+`"`)
	}
	return `{` + strings.Join(parts, ",") + `}`
}
