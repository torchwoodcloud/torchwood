package worker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
)

// ——rollup worker 单测（fake 仓储，不依赖 DB）：单轮预算常量、预算 ctx 传递
// 到存储层、metrics 埋点（时长直方图观测 + 失败计数）——

// fakeRollupRepo 捕获 RollupDay 收到的 ctx（断言预算经 ctx 到达存储层；
// Err 在调用时点取样——runOnce 返回后 defer cancel() 已触发派生 ctx）。
type fakeRollupRepo struct {
	lastCtx    context.Context
	lastCtxErr error
	lastCalls  int
	failDay    bool
}

func (f *fakeRollupRepo) RollupDay(ctx context.Context, _ string, _ time.Time) error {
	f.lastCtx = ctx
	f.lastCtxErr = ctx.Err()
	f.lastCalls++
	if f.failDay {
		return errors.New("boom")
	}
	return nil
}

func (f *fakeRollupRepo) RefreshDefinitionTotals30d(context.Context, string, time.Time) error {
	return nil
}

var _ domainanalytics.RollupRepository = (*fakeRollupRepo)(nil)

// fakeProjectRepo 只被 rollup 走 ListProjects；listErr 模拟轮次级失败。
type fakeProjectRepo struct {
	projects []domainprojects.Project
	listErr  error
}

func (f *fakeProjectRepo) CreateProject(context.Context, *domainprojects.Project) error { return nil }
func (f *fakeProjectRepo) GetProject(context.Context, string) (*domainprojects.Project, error) {
	return nil, nil
}
func (f *fakeProjectRepo) GetProjectByName(context.Context, string) (*domainprojects.Project, error) {
	return nil, nil
}
func (f *fakeProjectRepo) ListProjects(context.Context) ([]domainprojects.Project, error) {
	return f.projects, f.listErr
}
func (f *fakeProjectRepo) UpdateProject(context.Context, *domainprojects.Project) error {
	return nil
}
func (f *fakeProjectRepo) DeleteProject(context.Context, string) error { return nil }
func (f *fakeProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error {
	return nil
}

var _ domainprojects.Repository = (*fakeProjectRepo)(nil)

func rollupTestWorker(t *testing.T, repo *fakeRollupRepo, projects *fakeProjectRepo) *AnalyticsRollupWorker {
	t.Helper()
	return NewAnalyticsRollupWorker(appanalytics.NewRollup(repo, projects, nil), nil)
}

// TestAnalyticsRollupTimeout_BudgetAndContext（S12 项1）：单轮预算常量对齐
// maintenance 样板；预算以 deadline 经 ctx 传递到存储层；parent ctx 取消不
// 腰斩在途轮次（WithoutCancel）。
func TestAnalyticsRollupTimeout_BudgetAndContext(t *testing.T) {
	require.Equal(t, 10*time.Minute, analyticsRollupTimeout,
		"单轮预算与 analyticsMaintenanceTimeout 同量级（全项目两日重算的有界上限）")

	repo := &fakeRollupRepo{}
	w := rollupTestWorker(t, repo, &fakeProjectRepo{
		projects: []domainprojects.Project{{ID: "p1", Status: "active"}},
	})
	before := time.Now()
	w.runOnce(context.Background())

	require.Equal(t, 2, repo.lastCalls, "单项目一轮重算 [昨日, 今日] 两个日窗")
	require.NotNil(t, repo.lastCtx)
	deadline, ok := repo.lastCtx.Deadline()
	require.True(t, ok, "runOnce 必须把带预算的 ctx 传给存储层（此前无任何超时预算）")
	budget := deadline.Sub(before)
	require.Positive(t, budget)
	require.LessOrEqual(t, budget, analyticsRollupTimeout+10*time.Second,
		"预算不超过 analyticsRollupTimeout 上界（10s 余量为调度抖动）")

	// WithoutCancel 语义：parent 已取消仍执行一轮（在途轮次按预算收束，
	// analytics_maintenance 同款）。
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	repo2 := &fakeRollupRepo{}
	rollupTestWorker(t, repo2, &fakeProjectRepo{
		projects: []domainprojects.Project{{ID: "p1", Status: "active"}},
	}).runOnce(canceled)
	require.Equal(t, 2, repo2.lastCalls, "parent ctx 已取消不阻断本轮执行")
	require.NoError(t, repo2.lastCtxErr, "WithoutCancel：parent 取消不传播到在途轮次")
	dl, ok := repo2.lastCtx.Deadline()
	require.True(t, ok, "取消传播被隔离后预算仍然生效")
	require.False(t, dl.IsZero())
}

// TestAnalyticsRollupWorker_Metrics（S12 项3）：时长直方图每轮观测一次；
// 失败计数 = 单项目 RollupDay 失败数（同项目后续日 break 不重复计）；
// 轮次级（ListProjects）失败计 1。
func TestAnalyticsRollupWorker_Metrics(t *testing.T) {
	failuresBefore := testutil.ToFloat64(analyticsRollupFailuresTotal)
	obsBefore := histogramSampleCount(t, "torchwood_analytics_rollup_duration_seconds")

	// 两项目、每日 rollup 均失败：每项目计 1（昨日失败即 break），共 2。
	repo := &fakeRollupRepo{failDay: true}
	rollupTestWorker(t, repo, &fakeProjectRepo{
		projects: []domainprojects.Project{
			{ID: "p1", Status: "active"},
			{ID: "p2", Status: "active"},
			{ID: "p3", Status: "suspended"}, // 非 active 不参与
		},
	}).runOnce(context.Background())

	require.InDelta(t, failuresBefore+2, testutil.ToFloat64(analyticsRollupFailuresTotal), 0.001,
		"每个失败项目计 1 次（同项目后续日 break 不重复计）")
	require.Equal(t, 2, repo.lastCalls, "p1 失败不阻断 p2（单项目失败仅记日志）")
	require.Equal(t, obsBefore+1, histogramSampleCount(t, "torchwood_analytics_rollup_duration_seconds"),
		"每轮 rollup 观测一次时长")

	// 轮次级失败（项目列表报错）：计 1。
	failuresBefore = testutil.ToFloat64(analyticsRollupFailuresTotal)
	rollupTestWorker(t, &fakeRollupRepo{}, &fakeProjectRepo{listErr: errors.New("db down")}).
		runOnce(context.Background())
	require.InDelta(t, failuresBefore+1, testutil.ToFloat64(analyticsRollupFailuresTotal), 0.001,
		"轮次级失败计 1")
}

// histogramSampleCount 从默认 registry 聚合直方图 _count（观测次数；
// testutil.ToFloat64 只支持单值指标，不适用 Histogram）。
func histogramSampleCount(t *testing.T, name string) uint64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		var n uint64
		for _, m := range mf.GetMetric() {
			n += m.GetHistogram().GetSampleCount()
		}
		return n
	}
	return 0
}
