// 外部测试包（analytics_test）：maintenance worker 集成（PR5 验收）——
//   - 分区预建：未来 2 月（含当月）分区创建成功、写入落正确分区；
//   - DEFAULT 非空搬运：构造 DEFAULT 分区持有目标月行 → 搬运进新分区；
//   - 保留期裁剪：整月过期分区 DROP、不足整月留待下月、user_days 同窗口
//     修剪、护栏外查询回退 PR3 语义（InvalidArgument 不静默）；
//   - tombstone 清洗：三表身份关联行硬删 + done_at + 重删幂等。
package analytics_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

type maintenanceEnv struct {
	*rollupEnv
	maintenance *appanalytics.Maintenance
	query       *appanalytics.Query
}

func setupMaintenanceEnv(t *testing.T, retentionDays int) *maintenanceEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	e := setupRollupEnv(t)
	cfg := &config.AppConfig{Analytics: &config.Analytics{RetentionDays: int32(retentionDays)}}
	m := appanalytics.NewMaintenanceFromConfig(cfg, e.workerRepo, e.workerRepo, e.projectsRepo, nil)
	query := appanalytics.NewQueryFromConfig(cfg, bunrepo.NewAnalyticsQueryRepository(e.db))
	return &maintenanceEnv{rollupEnv: e, maintenance: m, query: query}
}

// partitionExists 报告指定月分区是否存在。
func (e *maintenanceEnv) partitionExists(t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT to_regclass('%s.%s') IS NOT NULL`, e.schema, name)).Scan(&ok))
	return ok
}

func (e *maintenanceEnv) partitionRowCount(t *testing.T, name string) int {
	t.Helper()
	return e.count(fmt.Sprintf(`SELECT count(*) FROM %s.%s`, e.schema, name))
}

// TestMaintenanceIntegration_PartitionPrecreateAndDefaultEviction（PR5 验收：
// 跨月边界——预建分区生效、写入落正确分区；DEFAULT 非空搬运）。
func TestMaintenanceIntegration_PartitionPrecreateAndDefaultEviction(t *testing.T) {
	e := setupMaintenanceEnv(t, 90)

	// 时钟 2026-09-15：迁移静态分区覆盖 2026_09/10，预建应补齐 2026_11。
	clock := time.Date(2026, 9, 15, 3, 0, 0, 0, time.UTC)
	require.NoError(t, e.maintenance.RunPartitionsOnce(e.ctx, clock))
	require.True(t, e.partitionExists(t, "analytics_events_2026_09"))
	require.True(t, e.partitionExists(t, "analytics_events_2026_10"))
	require.True(t, e.partitionExists(t, "analytics_events_2026_11"), "预建未来 2 月分区")

	// 写入落正确分区：11 月事件路由进 2026_11（而非 DEFAULT）。
	require.NoError(t, e.seedEventErr("level_complete", "u1", time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)))
	require.Equal(t, 1, e.partitionRowCount(t, "analytics_events_2026_11"))
	require.Equal(t, 1, e.partitionRowCount(t, "analytics_events"), "父表可见性一致")

	// 重跑幂等（IF NOT EXISTS）。
	require.NoError(t, e.maintenance.RunPartitionsOnce(e.ctx, clock))

	// DEFAULT 非空搬运：12 月事件先落 DEFAULT（分区未建），预建 2026_12 后
	// 事务内搬进正确分区。
	decEvent := time.Date(2026, 12, 10, 9, 30, 0, 0, time.UTC)
	require.NoError(t, e.seedEventErr("level_complete", "u2", decEvent))
	require.Equal(t, 1, e.partitionRowCount(t, "analytics_events_default"), "搬运前：DEFAULT 兜底持有")
	require.False(t, e.partitionExists(t, "analytics_events_2026_12"))

	clock2 := time.Date(2026, 12, 15, 3, 0, 0, 0, time.UTC)
	require.NoError(t, e.maintenance.RunPartitionsOnce(e.ctx, clock2))
	require.True(t, e.partitionExists(t, "analytics_events_2026_12"))
	require.Equal(t, 1, e.partitionRowCount(t, "analytics_events_2026_12"), "搬运后：行落正确分区")
	require.Equal(t, 0, e.count(fmt.Sprintf(
		`SELECT count(*) FROM %s.analytics_events_default WHERE occurred_at >= ? AND occurred_at < ?`, e.schema),
		time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)),
		"DEFAULT 该月范围已清空")
	require.Equal(t, 2, e.partitionRowCount(t, "analytics_events"), "父表行数不丢")
	// 搬运行保持身份（user_id/occurred_at 原样）。
	var userID string
	require.NoError(t, e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT user_id FROM %s.analytics_events WHERE occurred_at = ?`, e.schema), decEvent).Scan(&userID))
	require.Equal(t, "u2", userID)
}

// TestMaintenanceIntegration_RetentionPrune（PR5 验收：保留期）——DROP 整月
// 过期分区（不足整月留待下月）；user_days 同窗口修剪；护栏外查询回退 PR3
// 语义（InvalidArgument，不静默空结果）。
func TestMaintenanceIntegration_RetentionPrune(t *testing.T) {
	e := setupMaintenanceEnv(t, 7) // 最小保留期：cutoff = 2026-11-24

	// 先以 11 月时钟预建分区（模拟 worker 日常运行：11 月事件在分区内）。
	require.NoError(t, e.maintenance.RunPartitionsOnce(e.ctx, time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)))

	// 播种：09/10 月各一条 raw（整月过期），11 月一条（不足整月保留）；
	// user_days 一条过期（10-01）一条保留（11-25）。
	require.NoError(t, e.seedEventErr("level_complete", "u1", time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)))
	require.NoError(t, e.seedEventErr("level_complete", "u1", time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)))
	require.NoError(t, e.seedEventErr("level_complete", "u2", time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)))
	for _, d := range []string{"2026-10-01", "2026-11-25"} {
		_, err := e.db.ExecContext(e.ctx,
			fmt.Sprintf(`INSERT INTO %s.analytics_user_days (user_id, day, events) VALUES (?, ?::date, 1)`, e.schema),
			"u1", d)
		require.NoError(t, err)
	}

	clock := time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	require.NoError(t, e.maintenance.RunPruneOnce(e.ctx, clock))

	require.False(t, e.partitionExists(t, "analytics_events_2026_09"), "整月过期分区被 DROP")
	require.False(t, e.partitionExists(t, "analytics_events_2026_10"), "整月过期分区被 DROP")
	require.True(t, e.partitionExists(t, "analytics_events_2026_11"), "不足整月留待下月")
	require.Equal(t, 1, e.partitionRowCount(t, "analytics_events_2026_11"))

	// user_days 同窗口修剪。
	require.Equal(t, 1, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_days WHERE day >= ?::date`, e.schema), "2026-11-24"))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_days WHERE day < ?::date`, e.schema), "2026-11-24"))

	// 护栏外查询：90 天窗 > 保留期 7 天且无 rollup 覆盖 → InvalidArgument
	//（不静默返回残缺窗口，PR3 语义在裁剪后保持）。
	_, err := e.query.QueryTimeseries(e.ctx, appanalytics.TimeseriesCommand{
		ProjectID:   e.projectID,
		PeriodStart: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		PeriodEnd:   time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC),
		Granularity: domainanalytics.GranularityDay,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "raw retention")
}

// TestMaintenanceIntegration_TombstoneCleanup（PR5 验收：注销清洗队列）——
// tombstone → 三表硬删 → done_at；daily 聚合计数不回退（D10 近似口径）；
// 重删幂等。
func TestMaintenanceIntegration_TombstoneCleanup(t *testing.T) {
	e := setupMaintenanceEnv(t, 90)

	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	clock := dayD.Add(12 * time.Hour)

	// 用户 u1 两日活跃（rollup 产出 user_days/first_seen/daily），
	// u2 为对照用户（不受 u1 清洗影响）。
	e.seedEvent(t, "level_complete", "u1", dayD.AddDate(0, 0, -1).Add(9*time.Hour))
	e.seedEvent(t, "level_complete", "u1", dayD.Add(10*time.Hour))
	e.seedEvent(t, "level_complete", "u1", dayD.Add(12*time.Hour))
	e.seedEvent(t, "ad_watch", "u2", dayD.Add(11*time.Hour))
	e.runRollup(t, clock)

	dailyBefore := e.count(fmt.Sprintf(`SELECT COALESCE(SUM(total), 0) FROM %s.analytics_daily`, e.schema))
	require.Greater(t, dailyBefore, 0)

	// 写入 tombstone（注销钩子同端口）。
	require.NoError(t, e.workerRepo.EnqueueUserDeletion(e.ctx, e.projectID, "u1", clock))
	require.Equal(t, 1, e.count(fmt.Sprintf(
		`SELECT count(*) FROM %s.analytics_user_deletions WHERE user_id = 'u1' AND done_at IS NULL`, e.schema)))

	require.NoError(t, e.maintenance.RunCleanupOnce(e.ctx, clock))

	// 三表身份关联行清零；daily 聚合计数不回退（D10 已声明近似口径）。
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = 'u1'`, e.schema)))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_days WHERE user_id = 'u1'`, e.schema)))
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_first_seen WHERE user_id = 'u1'`, e.schema)))
	require.Equal(t, dailyBefore, e.count(fmt.Sprintf(`SELECT COALESCE(SUM(total), 0) FROM %s.analytics_daily`, e.schema)))
	require.Equal(t, 1, e.count(fmt.Sprintf(
		`SELECT count(*) FROM %s.analytics_user_deletions WHERE user_id = 'u1' AND done_at IS NOT NULL`, e.schema)))
	// 对照用户不受影响。
	require.Equal(t, 1, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = 'u2'`, e.schema)))

	// 重删幂等：重复注销（tombstone DO NOTHING）+ 重复清洗（重删 0 行、
	// done_at 幂等推进）。
	var doneAt1 time.Time
	require.NoError(t, e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT done_at FROM %s.analytics_user_deletions WHERE user_id = 'u1'`, e.schema)).Scan(&doneAt1))
	require.NoError(t, e.workerRepo.EnqueueUserDeletion(e.ctx, e.projectID, "u1", clock.Add(time.Hour)))
	require.Equal(t, 1, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_deletions`, e.schema)), "重删不产生新行")
	require.NoError(t, e.maintenance.RunCleanupOnce(e.ctx, clock.Add(time.Hour)))
	var doneAt2 time.Time
	require.NoError(t, e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT done_at FROM %s.analytics_user_deletions WHERE user_id = 'u1'`, e.schema)).Scan(&doneAt2))
	require.True(t, !doneAt2.Before(doneAt1), "done_at 幂等推进")
	require.Equal(t, 0, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events WHERE user_id = 'u1'`, e.schema)))
}
