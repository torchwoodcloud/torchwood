// 外部测试包（analytics_test）：rollup worker 集成（PR5 验收）——
//   - 重跑幂等：同窗口重跑 daily/user_days 数字不翻倍（D7 覆盖式）；
//   - 停摆恢复：跳过 1 天后补算正确（昨日终算覆盖，含晚到事件）；
//   - first_seen 极值推进（LEAST/GREATEST）与字典 total_30d 覆盖刷新。
package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// rollupEnv 聚合 rollup 集成测试装配（真实 Postgres 项目 schema analytics_*
// 表 + 真 projectRepo；raw 事件经 SQL 直接播种——rollup 的输入契约是 raw 表，
// 摄入链已在 PR2 独立覆盖）。
type rollupEnv struct {
	ctx          context.Context
	db           *clients.Database
	projectID    string
	schema       string
	rollup       *appanalytics.Rollup
	workerRepo   *bunrepo.AnalyticsWorkerRepository
	projectsRepo projects.Repository
}

func setupRollupEnv(t *testing.T) *rollupEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	workerRepo := bunrepo.NewAnalyticsWorkerRepository(db)
	projectsRepo := bunrepo.NewProjectRepository(db)
	return &rollupEnv{
		ctx:          ctx,
		db:           db,
		projectID:    projectID,
		schema:       testutil.CatalogQuoted(projectID),
		rollup:       appanalytics.NewRollup(workerRepo, projectsRepo, nil),
		workerRepo:   workerRepo,
		projectsRepo: projectsRepo,
	}
}

// runRollup 以注入时钟跑一轮 rollup（[昨日, 今日] 窗口 + 字典刷新）。
func (e *rollupEnv) runRollup(t *testing.T, clock time.Time) {
	t.Helper()
	require.NoError(t, e.rollup.RunWorkerOnce(e.ctx, clock))
}

// refreshDefs 直调字典 total_30d 刷新（独立于日窗的维护动词）。
func (e *rollupEnv) refreshDefs(t *testing.T, since time.Time) {
	t.Helper()
	require.NoError(t, e.workerRepo.RefreshDefinitionTotals30d(e.ctx, e.projectID, since))
}

func mustTotal(total, _ int64) int64 { return total }

func mustUnique(_ int64, unique int64) int64 { return unique }

// seedEvent 直插一条 raw 事件（occurred_at 任意日期——静态分区/DEFAULT 由
// PG 分区路由兜底）。
func (e *rollupEnv) seedEvent(t *testing.T, name, userID string, at time.Time) {
	t.Helper()
	require.NoError(t, e.seedEventErr(name, userID, at))
}

func (e *rollupEnv) seedEventErr(name, userID string, at time.Time) error {
	_, err := e.db.ExecContext(e.ctx,
		fmt.Sprintf(`INSERT INTO %s.analytics_events (name, user_id, session_id, source, platform, app_version, occurred_at, ingested_at, props)
VALUES (?, ?, '', 'client', '', '', ?, ?, '{}')`, e.schema),
		name, userID, at.UTC(), at.UTC())
	return err
}

// dailyRow 读 (day, name) 的 total/unique_users（无行 = 0/0）。
func (e *rollupEnv) dailyRow(t *testing.T, day time.Time, name string) (total, unique int64) {
	t.Helper()
	err := e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT total, unique_users FROM %s.analytics_daily WHERE day = ?::date AND name = ?`, e.schema),
		day.Format("2006-01-02"), name).Scan(&total, &unique)
	if err != nil {
		return 0, 0
	}
	return total, unique
}

func (e *rollupEnv) userDayEvents(t *testing.T, userID string, day time.Time) int64 {
	t.Helper()
	var n int64
	err := e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT events FROM %s.analytics_user_days WHERE user_id = ? AND day = ?::date`, e.schema),
		userID, day.Format("2006-01-02")).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

func (e *rollupEnv) userDaysCount(t *testing.T) int {
	t.Helper()
	return e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_days`, e.schema))
}

func (e *rollupEnv) firstSeenRow(t *testing.T, userID string) (first, last time.Time, ok bool) {
	t.Helper()
	err := e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT first_day::timestamptz, last_day::timestamptz FROM %s.analytics_user_first_seen WHERE user_id = ?`, e.schema),
		userID).Scan(&first, &last)
	return first, last, err == nil
}

func (e *rollupEnv) count(query string, args ...any) int {
	var n int
	_ = e.db.QueryRowContext(e.ctx, query, args...).Scan(&n)
	return n
}

func (e *rollupEnv) defTotal30d(t *testing.T, name string) (int64, bool) {
	t.Helper()
	var n int64
	err := e.db.QueryRowContext(e.ctx,
		fmt.Sprintf(`SELECT total_30d FROM %s.analytics_event_definitions WHERE name = ?`, e.schema), name).Scan(&n)
	return n, err == nil
}

// seedDefinition 直插字典行（total_30d 预置非 0 值，验证覆盖归零）。
func (e *rollupEnv) seedDefinition(t *testing.T, name string, at time.Time, total30d int64) {
	t.Helper()
	_, err := e.db.ExecContext(e.ctx,
		fmt.Sprintf(`INSERT INTO %s.analytics_event_definitions (name, first_seen, last_seen, total_30d) VALUES (?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET total_30d = EXCLUDED.total_30d`, e.schema),
		name, at, at, total30d)
	require.NoError(t, err)
}

// TestRollupIntegration_IdempotentRerun（PR5 验收：重跑幂等）——同窗口重跑
// daily/user_days/first_seen 数字不翻倍（覆盖式非累加，D7）。
func TestRollupIntegration_IdempotentRerun(t *testing.T) {
	// 固定时钟：D = 2026-09-11；rollup 窗口 = [D-1 终算, D 部分]。
	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	clock := dayD.Add(12 * time.Hour)
	e := setupRollupEnv(t)

	// raw 播种：3×level_complete（u1×2, u2×1）、1×ad_watch（u3）、
	// 1×cron_tick（空归属——server 无归属事件）。
	base := dayD.Add(10 * time.Hour)
	e.seedEvent(t, "level_complete", "u1", base)
	e.seedEvent(t, "level_complete", "u1", base.Add(time.Hour))
	e.seedEvent(t, "level_complete", "u2", base.Add(2*time.Hour))
	e.seedEvent(t, "ad_watch", "u3", base.Add(3*time.Hour))
	e.seedEvent(t, "cron_tick", "", base.Add(4*time.Hour))

	e.runRollup(t, clock)

	require.Equal(t, int64(3), mustTotal(e.dailyRow(t, dayD, "level_complete")), "daily total")
	require.Equal(t, int64(2), mustUnique(e.dailyRow(t, dayD, "level_complete")), "daily unique_users")
	require.Equal(t, int64(1), mustTotal(e.dailyRow(t, dayD, "ad_watch")))
	require.Equal(t, int64(1), mustTotal(e.dailyRow(t, dayD, "cron_tick")))

	require.Equal(t, int64(2), e.userDayEvents(t, "u1", dayD))
	require.Equal(t, int64(1), e.userDayEvents(t, "u2", dayD))
	require.Equal(t, int64(1), e.userDayEvents(t, "u3", dayD))
	// 空归属不进用户粒度表。
	require.Equal(t, 3, e.userDaysCount(t))

	// rollup 与 raw 路径口径一致（D6/D8：source 切换数字无缝）。
	require.Equal(t, 5, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_events`, e.schema)))

	// 同窗口重跑：全表数字不变（覆盖式）。
	e.runRollup(t, clock)
	require.Equal(t, int64(3), mustTotal(e.dailyRow(t, dayD, "level_complete")))
	require.Equal(t, int64(2), mustUnique(e.dailyRow(t, dayD, "level_complete")))
	require.Equal(t, int64(2), e.userDayEvents(t, "u1", dayD))
	require.Equal(t, 3, e.userDaysCount(t), "user_days 行数不增（覆盖非累加）")
	require.Equal(t, 3, e.count(fmt.Sprintf(`SELECT count(*) FROM %s.analytics_user_first_seen`, e.schema)))
}

// TestRollupIntegration_StoppageRecovery（PR5 验收：停摆恢复）——worker 跳过
// 1 天后重启，昨日终算覆盖式补算正确（含停摆期间落库的晚到事件）。
func TestRollupIntegration_StoppageRecovery(t *testing.T) {
	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	dayDm1 := dayD.AddDate(0, 0, -1)
	dayDm2 := dayD.AddDate(0, 0, -2)

	// 阶段 1（D-2 当天正常运行）：raw 2 条 on D-2，rollup 正常。
	clock := dayDm2.Add(12 * time.Hour)
	e := setupRollupEnv(t)
	base := func(day time.Time) time.Time { return day.Add(10 * time.Hour) }
	e.seedEvent(t, "level_complete", "u1", base(dayDm2))
	e.seedEvent(t, "level_complete", "u1", base(dayDm2).Add(time.Hour))
	e.runRollup(t, clock)
	require.Equal(t, int64(2), mustTotal(e.dailyRow(t, dayDm2, "level_complete")))

	// 停摆窗口（D-1 全天）：落库 3 条 on D-1 + 2 条补传 on D-1（钳制窗内的
	// 晚到事件）+ 7 条 on D。worker 全天未跑。
	for i := 0; i < 3; i++ {
		e.seedEvent(t, "level_complete", "u1", base(dayDm1).Add(time.Duration(i)*time.Hour))
	}
	e.seedEvent(t, "level_complete", "u2", base(dayDm1).Add(4*time.Hour))
	e.seedEvent(t, "level_complete", "u2", base(dayDm1).Add(5*time.Hour))
	for i := 0; i < 7; i++ {
		e.seedEvent(t, "ad_watch", "u3", base(dayD).Add(time.Duration(i)*time.Minute))
	}

	// 阶段 2（D 恢复）：窗口 [D-1 终算, D 部分] 补算——D-1 恒 5（覆盖终算，
	// 不与停摆前的任何部分值叠加）。
	clock2 := dayD.Add(12 * time.Hour)
	e.runRollup(t, clock2)

	require.Equal(t, int64(5), mustTotal(e.dailyRow(t, dayDm1, "level_complete")), "昨日终算补算")
	require.Equal(t, int64(2), mustUnique(e.dailyRow(t, dayDm1, "level_complete")))
	require.Equal(t, int64(7), mustTotal(e.dailyRow(t, dayD, "ad_watch")))
	require.Equal(t, int64(2), mustTotal(e.dailyRow(t, dayDm2, "level_complete")), "历史日不被触碰")

	require.Equal(t, int64(3), e.userDayEvents(t, "u1", dayDm1))
	require.Equal(t, int64(2), e.userDayEvents(t, "u2", dayDm1))
	require.Equal(t, int64(7), e.userDayEvents(t, "u3", dayD))

	first, last, ok := e.firstSeenRow(t, "u1")
	require.True(t, ok)
	require.Equal(t, dayDm2.Format("2006-01-02"), first.Format("2006-01-02"), "first_day 锚定首日")
	require.Equal(t, dayDm1.Format("2006-01-02"), last.Format("2006-01-02"), "last_day 推进到补算日")
	_, _, ok = e.firstSeenRow(t, "u3")
	require.True(t, ok)
}

// TestRollupIntegration_FirstSeenExtremesAndDefTotals（PR5：first_seen 极值
// upsert + 字典 total_30d 覆盖刷新）。
func TestRollupIntegration_FirstSeenExtremesAndDefTotals(t *testing.T) {
	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	dayDm1 := dayD.AddDate(0, 0, -1)
	clock := dayD.Add(12 * time.Hour)
	e := setupRollupEnv(t)

	// 首轮：u1 在 D 活跃。
	e.seedEvent(t, "level_complete", "u1", dayD.Add(10*time.Hour))
	e.runRollup(t, clock)
	first, last, ok := e.firstSeenRow(t, "u1")
	require.True(t, ok)
	require.Equal(t, dayD.Format("2006-01-02"), first.Format("2006-01-02"))

	// 补传 D-1 的事件（钳制窗内晚到）：重算后 first_day 回溯到 D-1（LEAST），
	// last_day 保持 D（GREATEST）——极值语义幂等且方向正确。
	e.seedEvent(t, "level_complete", "u1", dayDm1.Add(9*time.Hour))
	e.runRollup(t, clock)
	first, last, ok = e.firstSeenRow(t, "u1")
	require.True(t, ok)
	require.Equal(t, dayDm1.Format("2006-01-02"), first.Format("2006-01-02"), "first_day = LEAST")
	require.Equal(t, dayD.Format("2006-01-02"), last.Format("2006-01-02"), "last_day = GREATEST")

	// 字典 total_30d：窗口内有事件的 3 条、窗口外无事件的归 0。
	e.seedDefinition(t, "level_complete", clock, 999)
	e.seedDefinition(t, "ghost_event", clock, 999)
	since := clock.Add(-domainanalytics.DefTotalsWindow)
	e.refreshDefs(t, since)

	n, ok := e.defTotal30d(t, "level_complete")
	require.True(t, ok)
	require.Equal(t, int64(2), n, "total_30d = 窗口内 count（覆盖式）")
	n, ok = e.defTotal30d(t, "ghost_event")
	require.True(t, ok)
	require.Equal(t, int64(0), n, "窗口无事件的名字归 0（覆盖非累加）")
}
