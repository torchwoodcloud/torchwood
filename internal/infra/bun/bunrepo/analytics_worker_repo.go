package bunrepo

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

var (
	_ analytics.RollupRepository        = (*AnalyticsWorkerRepository)(nil)
	_ analytics.MaintenanceRepository   = (*AnalyticsWorkerRepository)(nil)
	_ analytics.TombstoneCleaner        = (*AnalyticsWorkerRepository)(nil)
	_ analytics.DeletionQueueRepository = (*AnalyticsWorkerRepository)(nil)
)

// analyticsEventsDefaultTable 是 DEFAULT 兜底分区表名（迁移 000019 静态建）。
const analyticsEventsDefaultTable = "analytics_events_default"

// AnalyticsWorkerRepository 是 PR5 worker 面仓储（bunrepo Scoped 模式）：
// rollup 幂等重算（D7）+ 分区治理/保留期裁剪（D5）+ tombstone 清洗与注销
// 钩子写入（D10）。纪律与 analytics_query_repo 同源：
//   - 表名恒 schema 限定（fmt 仅注入经 ident 校验 + 引号转义的 schema 名），
//     值一律 `?` 绑定参数；DDL 中的分区边界/分区名为 worker 派生的日期
//     字面量并经形状正则约束（非用户输入）；
//   - 幂等覆盖语义：upsert 冲突分支整体替换聚合值（重跑不翻倍，D7），
//     first_seen 取极值（LEAST/GREATEST 天然幂等），tombstone DO NOTHING；
//   - 日期一律以 'YYYY-MM-DD' 字面量参数传给 ?::date（字符串→date 铸造与
//     会话时区无关，避免服务器 TZ 漂移改变切日）。
type AnalyticsWorkerRepository struct {
	db *clients.Database
}

// NewAnalyticsWorkerRepository 构造 worker 面仓储。
func NewAnalyticsWorkerRepository(db *clients.Database) *AnalyticsWorkerRepository {
	return &AnalyticsWorkerRepository{db: db}
}

// ensureSchema 确保 analytics 表就绪（Apply 就绪缓存命中直通；ctx 携带调用
// 方事务时并入该事务——与全部 repo 的 Scoped 前置一致）。
func (r *AnalyticsWorkerRepository) ensureSchema(ctx context.Context, projectID string) error {
	_, _, _, err := Scoped(ctx, r.db, projectID, analyticsDailyTable, "ad")
	return err
}

// schemaQuoted 确保就绪后返回 quoteIdent 的 schema 名（Raw SQL 用）。
func (r *AnalyticsWorkerRepository) schemaQuoted(ctx context.Context, projectID, table, alias string) (string, error) {
	if _, _, _, err := Scoped(ctx, r.db, projectID, table, alias); err != nil {
		return "", err
	}
	return ProjectQuoted(projectID)
}

// exec 项目 schema 上执行单条 worker SQL（简单协议多语句为隐式事务；批量
// DELETE 各自独立提交、重试幂等）。
func (r *AnalyticsWorkerRepository) exec(ctx context.Context, projectID, query string, args ...any) error {
	if err := r.ensureSchema(ctx, projectID); err != nil {
		return err
	}
	if _, err := r.db.Conn(ctx).ExecContext(ctx, query, args...); err != nil {
		return err
	}
	return nil
}

// execResult 同 exec 并返回受影响行数。
func (r *AnalyticsWorkerRepository) execResult(ctx context.Context, projectID, query string, args ...any) (int64, error) {
	if err := r.ensureSchema(ctx, projectID); err != nil {
		return 0, err
	}
	res, err := r.db.Conn(ctx).ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// queryList 项目 schema 上执行单列查询。
func (r *AnalyticsWorkerRepository) queryList(ctx context.Context, projectID, query string, args ...any) ([]string, error) {
	if err := r.ensureSchema(ctx, projectID); err != nil {
		return nil, err
	}
	rows, err := r.db.Conn(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() // #nosec G104 -- 只读游标，关闭失败不改变查询结果
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RollupDay 幂等重算单日（同事务三表覆盖，D7；day = UTC 零点）。
func (r *AnalyticsWorkerRepository) RollupDay(ctx context.Context, projectID string, day time.Time) error {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsDailyTable, "ad")
	if err != nil {
		return err
	}
	dayStart := dayUTC(day)
	dayEnd := dayStart.Add(24 * time.Hour)
	dayLiteral := dayStart.Format("2006-01-02")
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn := r.db.Conn(txCtx)
		if _, err := conn.ExecContext(txCtx, analyticsRollupDailySQL(quoted), dayLiteral, dayStart, dayEnd); err != nil {
			return fmt.Errorf("rollup daily: %w", err)
		}
		if _, err := conn.ExecContext(txCtx, analyticsRollupUserDaysSQL(quoted), dayLiteral, dayStart, dayEnd); err != nil {
			return fmt.Errorf("rollup user_days: %w", err)
		}
		if _, err := conn.ExecContext(txCtx, analyticsRollupFirstSeenSQL(quoted), dayLiteral, dayLiteral, dayStart, dayEnd); err != nil {
			return fmt.Errorf("rollup first_seen: %w", err)
		}
		return nil
	})
}

// RefreshDefinitionTotals30d 覆盖式刷新字典 total_30d（窗口外名字归 0）。
func (r *AnalyticsWorkerRepository) RefreshDefinitionTotals30d(ctx context.Context, projectID string, since time.Time) error {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsEventDefinitionsTable, "aed")
	if err != nil {
		return err
	}
	return r.exec(ctx, projectID, analyticsRefreshDefTotalsSQL(quoted), since.UTC())
}

// EnsureMonthlyPartitions 预建自 now 所在月起 months 个月的月分区；DEFAULT
// 与目标月重叠时事务内搬运（父表 ACCESS EXCLUSIVE 锁内：count → 搬 → 建分区
// → 回填，杜绝并发摄取竞态窗口）。
func (r *AnalyticsWorkerRepository) EnsureMonthlyPartitions(ctx context.Context, projectID string, now time.Time, months int) error {
	if months <= 0 {
		return nil
	}
	now = now.UTC()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < months; i++ {
		monthStart := first.AddDate(0, i, 0)
		if err := r.ensureOnePartition(ctx, projectID, monthStart); err != nil {
			return err
		}
	}
	return nil
}

func (r *AnalyticsWorkerRepository) ensureOnePartition(ctx context.Context, projectID string, monthStart time.Time) error {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsEventsTable, "ae")
	if err != nil {
		return err
	}
	name := analyticsMonthlyPartitionName(monthStart)
	if !analyticsPartitionNamePattern.MatchString(name) {
		return fmt.Errorf("invalid partition name %q", name)
	}
	monthEnd := monthStart.AddDate(0, 1, 0)
	lo, hi := monthStart.Format("2006-01-02"), monthEnd.Format("2006-01-02")
	// 单事务：锁父表（阻塞并发摄取进 DEFAULT）→ 数 DEFAULT 内该月行 →
	// 有则搬运（TEMP 表中转，ON COMMIT DROP 自清理）→ 建分区（幂等）→ 回填。
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn := r.db.Conn(txCtx)
		if _, err := conn.ExecContext(txCtx, fmt.Sprintf(`LOCK TABLE %s.analytics_events IN ACCESS EXCLUSIVE MODE`, quoted)); err != nil {
			return fmt.Errorf("lock analytics_events: %w", err)
		}
		var n int
		if err := conn.QueryRowContext(txCtx,
			fmt.Sprintf(`SELECT count(*) FROM %s.%s WHERE occurred_at >= ? AND occurred_at < ?`, quoted, analyticsEventsDefaultTable),
			monthStart, monthEnd).Scan(&n); err != nil {
			return fmt.Errorf("count default partition rows: %w", err)
		}
		if n > 0 {
			if _, err := conn.ExecContext(txCtx, fmt.Sprintf(
				`CREATE TEMP TABLE analytics_events_stage (LIKE %s.analytics_events INCLUDING DEFAULTS) ON COMMIT DROP`, quoted)); err != nil {
				return fmt.Errorf("create stage table: %w", err)
			}
			if _, err := conn.ExecContext(txCtx, fmt.Sprintf(
				`INSERT INTO analytics_events_stage SELECT * FROM %s.%s WHERE occurred_at >= ? AND occurred_at < ?`, quoted, analyticsEventsDefaultTable),
				monthStart, monthEnd); err != nil {
				return fmt.Errorf("stage default rows: %w", err)
			}
			if _, err := conn.ExecContext(txCtx, fmt.Sprintf(
				`DELETE FROM %s.%s WHERE occurred_at >= ? AND occurred_at < ?`, quoted, analyticsEventsDefaultTable),
				monthStart, monthEnd); err != nil {
				return fmt.Errorf("delete default rows: %w", err)
			}
		}
		if _, err := conn.ExecContext(txCtx, analyticsCreatePartitionSQL(quoted, name, lo, hi)); err != nil {
			return fmt.Errorf("create partition %s: %w", name, err)
		}
		if n > 0 {
			if _, err := conn.ExecContext(txCtx, fmt.Sprintf(
				`INSERT INTO %s.%s SELECT * FROM analytics_events_stage`, quoted, name)); err != nil {
				return fmt.Errorf("backfill partition %s: %w", name, err)
			}
		}
		return nil
	})
}

// ListExpiredMonthlyPartitions 返回整月早于 cutoff 的分区名（经 pg_inherits
// 枚举真实分区，再按名称形状解析月份边界判定）。
func (r *AnalyticsWorkerRepository) ListExpiredMonthlyPartitions(ctx context.Context, projectID string, cutoff time.Time) ([]string, error) {
	if _, _, _, err := Scoped(ctx, r.db, projectID, analyticsEventsTable, "ae"); err != nil {
		return nil, err
	}
	schema, err := ident.ProjectSchemaName(projectID)
	if err != nil {
		return nil, err
	}
	names, err := r.queryList(ctx, projectID, analyticsListPartitionsSQL(), schema)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		if name == analyticsEventsDefaultTable {
			continue
		}
		monthEnd, ok := analyticsPartitionMonthEnd(name)
		if !ok {
			continue // 非月分区命名形状（防御：DEFAULT 之外的未知子表不动）
		}
		if !monthEnd.After(cutoff.UTC()) {
			out = append(out, name)
		}
	}
	return out, nil
}

// DropPartition DROP 指定月分区（名称必须匹配 analytics_events_YYYY_MM）。
func (r *AnalyticsWorkerRepository) DropPartition(ctx context.Context, projectID, partition string) error {
	if !analyticsPartitionNamePattern.MatchString(partition) {
		return fmt.Errorf("invalid partition name %q", partition)
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	return r.exec(ctx, projectID, fmt.Sprintf(`DROP TABLE IF EXISTS %s.%s`, quoted, partition))
}

// PruneUserDaysBefore 批删早于 cutoff 的 user_days 行（ctid 分批，单批 <= limit）。
func (r *AnalyticsWorkerRepository) PruneUserDaysBefore(ctx context.Context, projectID string, cutoff time.Time, limit int64) (int64, error) {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsUserDaysTable, "aud")
	if err != nil {
		return 0, err
	}
	return r.execResult(ctx, projectID, analyticsPruneUserDaysSQL(quoted), dayUTC(cutoff).Format("2006-01-02"), limit)
}

// ListPendingDeletionUsers 待清洗 tombstone 用户（先进先出）。
func (r *AnalyticsWorkerRepository) ListPendingDeletionUsers(ctx context.Context, projectID string, limit int) ([]string, error) {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsUserDeletionsTable, "audl")
	if err != nil {
		return nil, err
	}
	return r.queryList(ctx, projectID, analyticsPendingDeletionsSQL(quoted), limit)
}

// DeleteEventsByUserBatch 按身份点删一批 raw 事件（PK (id, occurred_at) 精确
// 行定位——USING 子查询与目标同表同 user_id 谓词，跨分区无错删面）。
func (r *AnalyticsWorkerRepository) DeleteEventsByUserBatch(ctx context.Context, projectID, userID string, limit int64) (int64, error) {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsEventsTable, "ae")
	if err != nil {
		return 0, err
	}
	return r.execResult(ctx, projectID, analyticsDeleteEventsByUserSQL(quoted), userID, limit)
}

// DeleteUserDaysByUser 删除该用户全部 user_days 行。
func (r *AnalyticsWorkerRepository) DeleteUserDaysByUser(ctx context.Context, projectID, userID string) (int64, error) {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsUserDaysTable, "aud")
	if err != nil {
		return 0, err
	}
	return r.execResult(ctx, projectID, fmt.Sprintf(`DELETE FROM %s.%s WHERE user_id = ?`, quoted, analyticsUserDaysTable), userID)
}

// DeleteFirstSeenByUser 删除该用户 first_seen 行。
func (r *AnalyticsWorkerRepository) DeleteFirstSeenByUser(ctx context.Context, projectID, userID string) (int64, error) {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsUserFirstSeenTable, "ufs")
	if err != nil {
		return 0, err
	}
	return r.execResult(ctx, projectID, fmt.Sprintf(`DELETE FROM %s.%s WHERE user_id = ?`, quoted, analyticsUserFirstSeenTable), userID)
}

// MarkDeletionDone 标记 tombstone 已清洗（幂等）。
func (r *AnalyticsWorkerRepository) MarkDeletionDone(ctx context.Context, projectID, userID string, doneAt time.Time) error {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsUserDeletionsTable, "audl")
	if err != nil {
		return err
	}
	return r.exec(ctx, projectID,
		fmt.Sprintf(`UPDATE %s.%s SET done_at = ? WHERE user_id = ?`, quoted, analyticsUserDeletionsTable),
		doneAt.UTC(), userID)
}

// EnqueueUserDeletion 注销钩子写 tombstone（重删幂等 DO NOTHING；ctx 带调用
// 方事务时并入——Scoped/Conn 的 WithTx 通道）。
func (r *AnalyticsWorkerRepository) EnqueueUserDeletion(ctx context.Context, projectID, userID string, enqueuedAt time.Time) error {
	quoted, err := r.schemaQuoted(ctx, projectID, analyticsUserDeletionsTable, "audl")
	if err != nil {
		return err
	}
	_, err = r.db.Conn(ctx).ExecContext(ctx, analyticsEnqueueDeletionSQL(quoted), userID, enqueuedAt.UTC())
	return err
}

// ---------------------------------------------------------------------------
// SQL 构造器（形状护栏测试的锚点：schema 为唯一 fmt 注入物——经 ident 校验
// + 引号转义；值全部 `?` 绑定参数；分区名/日期字面量为 worker 派生并被形状
// 正则约束；聚合语义覆盖式——重跑不翻倍，D7）。
// ---------------------------------------------------------------------------

// analyticsRollupDailySQL 事件×日覆盖 upsert：GROUP BY 全量重算 + ON CONFLICT
// 整值替换（不累加）。unique_users 口径与 raw 路径一致（COUNT(DISTINCT
// user_id)，含空归属）——source 切换时数字无缝衔接。
func analyticsRollupDailySQL(schema string) string {
	return fmt.Sprintf(`INSERT INTO %s.analytics_daily (day, name, total, unique_users, updated_at)
SELECT ?::date AS day, name, COUNT(*) AS total, COUNT(DISTINCT user_id) AS unique_users, NOW()
FROM %s.analytics_events WHERE occurred_at >= ? AND occurred_at < ?
GROUP BY name
ON CONFLICT (day, name) DO UPDATE
SET total = EXCLUDED.total, unique_users = EXCLUDED.unique_users, updated_at = EXCLUDED.updated_at`, schema, schema)
}

// analyticsRollupUserDaysSQL 用户×日覆盖 upsert（仅归属事件 user_id <> ”；
// events 整值替换）。
func analyticsRollupUserDaysSQL(schema string) string {
	return fmt.Sprintf(`INSERT INTO %s.analytics_user_days (user_id, day, events)
SELECT user_id, ?::date AS day, COUNT(*) AS events
FROM %s.analytics_events WHERE occurred_at >= ? AND occurred_at < ? AND user_id <> ''
GROUP BY user_id
ON CONFLICT (user_id, day) DO UPDATE
SET events = EXCLUDED.events`, schema, schema)
}

// analyticsRollupFirstSeenSQL 首见/末见极值 upsert（LEAST/GREATEST：重算与
// 补算均幂等；updated_at 推进）。
func analyticsRollupFirstSeenSQL(schema string) string {
	return fmt.Sprintf(`INSERT INTO %s.analytics_user_first_seen (user_id, first_day, last_day, updated_at)
SELECT user_id, ?::date AS first_day, ?::date AS last_day, NOW()
FROM %s.analytics_events WHERE occurred_at >= ? AND occurred_at < ? AND user_id <> ''
GROUP BY user_id
ON CONFLICT (user_id) DO UPDATE
SET first_day = LEAST(analytics_user_first_seen.first_day, EXCLUDED.first_day),
    last_day = GREATEST(analytics_user_first_seen.last_day, EXCLUDED.last_day),
    updated_at = NOW()`, schema, schema)
}

// analyticsRefreshDefTotalsSQL 字典 total_30d 覆盖刷新：LEFT JOIN 全字典
// （窗口无事件的名字 COALESCE 归 0——total_30d 是排序键而非累计值）。
func analyticsRefreshDefTotalsSQL(schema string) string {
	return fmt.Sprintf(`UPDATE %s.analytics_event_definitions AS aed
SET total_30d = COALESCE(c.cnt, 0)
FROM (
    SELECT d.name AS name, e.cnt AS cnt
    FROM %s.analytics_event_definitions d
    LEFT JOIN (
        SELECT name, COUNT(*) AS cnt FROM %s.analytics_events
        WHERE occurred_at >= ?
        GROUP BY name
    ) e ON e.name = d.name
) c
WHERE aed.name = c.name`, schema, schema, schema)
}

// analyticsListPartitionsSQL 经 pg_inherits 枚举 analytics_events 的分区名
// （schema 名走绑定参数，无 fmt 注入物）。
func analyticsListPartitionsSQL() string {
	return `SELECT c.relname FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
JOIN pg_class p ON p.oid = i.inhparent
JOIN pg_namespace n ON n.oid = p.relnamespace
WHERE n.nspname = ? AND p.relname = 'analytics_events'`
}

// analyticsCreatePartitionSQL 建月分区（幂等；边界为 worker 派生日期字面量）。
func analyticsCreatePartitionSQL(schema, partition, lo, hi string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s PARTITION OF %s.analytics_events FOR VALUES FROM ('%s') TO ('%s')`,
		schema, partition, schema, lo, hi)
}

// analyticsPruneUserDaysSQL user_days 保留期修剪（ctid 分批，行数有界）。
func analyticsPruneUserDaysSQL(schema string) string {
	return fmt.Sprintf(`DELETE FROM %s.analytics_user_days WHERE ctid IN (
SELECT ctid FROM %s.analytics_user_days WHERE day < ?::date LIMIT ?)`, schema, schema)
}

// analyticsPendingDeletionsSQL 待清洗 tombstone 队列（先进先出，有界）。
func analyticsPendingDeletionsSQL(schema string) string {
	return fmt.Sprintf(`SELECT user_id FROM %s.analytics_user_deletions WHERE done_at IS NULL ORDER BY enqueued_at ASC LIMIT ?`, schema)
}

// analyticsDeleteEventsByUserSQL 用户 raw 事件身份点删（单批有界；PK 精确
// 行定位，重删幂等）。
func analyticsDeleteEventsByUserSQL(schema string) string {
	return fmt.Sprintf(`DELETE FROM %s.analytics_events e
USING (SELECT id, occurred_at FROM %s.analytics_events WHERE user_id = ? LIMIT ?) v
WHERE e.id = v.id AND e.occurred_at = v.occurred_at`, schema, schema)
}

// analyticsEnqueueDeletionSQL 注销 tombstone 写入（重删幂等 DO NOTHING）。
func analyticsEnqueueDeletionSQL(schema string) string {
	return fmt.Sprintf(`INSERT INTO %s.analytics_user_deletions (user_id, enqueued_at) VALUES (?, ?) ON CONFLICT (user_id) DO NOTHING`, schema)
}

// analyticsPartitionNamePattern 是月分区名形状锁（analytics_events_YYYY_MM）：
// DROP/CREATE 的唯一放行形状，杜绝未知表名进 DDL。
var analyticsPartitionNamePattern = regexp.MustCompile(`^analytics_events_(\d{4})_(\d{2})$`)

// dayUTC 归一到 UTC 零点。
func dayUTC(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// analyticsMonthlyPartitionName 月份起点 → 分区名。
func analyticsMonthlyPartitionName(monthStart time.Time) string {
	return fmt.Sprintf("%s_%04d_%02d", analyticsEventsTable, monthStart.Year(), int(monthStart.Month()))
}

// analyticsPartitionMonthEnd 解析分区名 → 月份上界（[lo, hi) 的 hi）。
// 非 YYYY_MM 形状返回 ok=false。
func analyticsPartitionMonthEnd(name string) (time.Time, bool) {
	m := analyticsPartitionNamePattern.FindStringSubmatch(name)
	if m == nil {
		return time.Time{}, false
	}
	year, err1 := strconv.Atoi(m[1])
	month, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil || month < 1 || month > 12 {
		return time.Time{}, false
	}
	lo := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	return lo.AddDate(0, 1, 0), true
}
