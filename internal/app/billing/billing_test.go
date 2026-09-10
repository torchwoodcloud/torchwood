package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	domainprojects "github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	infrabilling "github.com/torchwoodcloud/torchwood/internal/infra/billing"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

type memRollups struct {
	mu   sync.Mutex
	rows map[string]*domainbilling.Rollup
}

func newMemRollups() *memRollups {
	return &memRollups{rows: map[string]*domainbilling.Rollup{}}
}

func rollupKey(projectID, metric string, start time.Time) string {
	return projectID + "|" + metric + "|" + start.UTC().Format(time.RFC3339)
}

func (m *memRollups) Upsert(_ context.Context, r *domainbilling.Rollup) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := rollupKey(r.ProjectID, r.Metric, r.PeriodStart)
	cp := *r
	if existing, ok := m.rows[k]; ok {
		cp.ID = existing.ID
		cp.CreatedAt = existing.CreatedAt
	}
	cp.Value = r.Value // 覆盖，不累加
	m.rows[k] = &cp
	return nil
}

func (m *memRollups) Get(_ context.Context, projectID, metric string, periodStart time.Time) (*domainbilling.Rollup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[rollupKey(projectID, metric, periodStart)]
	if r == nil {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

func (m *memRollups) List(_ context.Context, projectID, metric string, from, to time.Time, limit int, before time.Time, beforeID string) ([]domainbilling.Rollup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domainbilling.Rollup
	for _, r := range m.rows {
		if r.ProjectID != projectID {
			continue
		}
		if metric != "" && r.Metric != metric {
			continue
		}
		if !from.IsZero() && r.PeriodStart.Before(from) {
			continue
		}
		if !to.IsZero() && !r.PeriodStart.Before(to) {
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func (m *memRollups) SumByMetric(_ context.Context, projectID string, from, to time.Time) (map[string]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int64{}
	for _, r := range m.rows {
		if r.ProjectID != projectID {
			continue
		}
		if !from.IsZero() && r.PeriodStart.Before(from) {
			continue
		}
		if !to.IsZero() && !r.PeriodStart.Before(to) {
			continue
		}
		out[r.Metric] += r.Value
	}
	return out, nil
}

func (m *memRollups) DistinctHours(_ context.Context, projectID string, from, to time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[time.Time]struct{}{}
	for _, r := range m.rows {
		if r.ProjectID != projectID {
			continue
		}
		if !from.IsZero() && r.PeriodStart.Before(from) {
			continue
		}
		if !to.IsZero() && !r.PeriodStart.Before(to) {
			continue
		}
		seen[r.PeriodStart] = struct{}{}
	}
	return len(seen), nil
}

type memStatements struct {
	mu   sync.Mutex
	rows map[string]*domainbilling.Statement
}

func newMemStatements() *memStatements {
	return &memStatements{rows: map[string]*domainbilling.Statement{}}
}

func statementKey(projectID string, start time.Time) string {
	return projectID + "|" + start.UTC().Format(time.RFC3339)
}

func (m *memStatements) Upsert(_ context.Context, s *domainbilling.Statement) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := statementKey(s.ProjectID, s.PeriodStart)
	if existing, ok := m.rows[k]; ok && existing.Status == domainbilling.StatementFinal {
		return nil
	}
	cp := *s
	if cp.Details.Metrics != nil {
		metrics := make(map[string]int64, len(cp.Details.Metrics))
		for mk, mv := range cp.Details.Metrics {
			metrics[mk] = mv
		}
		cp.Details.Metrics = metrics
	}
	m.rows[k] = &cp
	return nil
}

func (m *memStatements) Get(_ context.Context, projectID string, periodStart time.Time) (*domainbilling.Statement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.rows[statementKey(projectID, periodStart)]
	if s == nil {
		return nil, nil
	}
	cp := *s
	return &cp, nil
}

func (m *memStatements) List(_ context.Context, projectID string, limit int, before time.Time) ([]domainbilling.Statement, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domainbilling.Statement
	for _, s := range m.rows {
		if s.ProjectID != projectID {
			continue
		}
		if !before.IsZero() && !s.PeriodStart.Before(before) {
			continue
		}
		out = append(out, *s)
	}
	return out, nil
}

type listProjectsStub struct {
	domainprojects.Repository
	list []domainprojects.Project
}

func (s *listProjectsStub) ListProjects(context.Context) ([]domainprojects.Project, error) {
	return s.list, nil
}

func newTestBilling(t *testing.T) (*infrabilling.RedisCounter, *memRollups, *memStatements, *Billing) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := infrabilling.NewRedisCounter(rdb)
	rollups := newMemRollups()
	statements := newMemStatements()
	b := NewBilling(counter, rollups, statements, &listProjectsStub{list: []domainprojects.Project{{ID: "proj-1", Status: "active"}}}, nil, nil)
	b.now = func() time.Time { return time.Date(2026, 8, 20, 16, 5, 0, 0, time.UTC) }
	return counter, rollups, statements, b
}

func TestRollupIdempotentAndStatementReconciles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	counter, rollups, statements, b := newTestBilling(t)

	// 当前=16:05 UTC；完整小时=15:00 与更早。模拟 worker 停机 1h 后重启：
	// 14:00 与 15:00 的 bucket 都还在 Redis。
	now := time.Date(2026, 8, 20, 16, 5, 0, 0, time.UTC)
	h14 := time.Date(2026, 8, 20, 14, 0, 0, 0, time.UTC)
	h15 := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	require.NoError(t, counter.IncrAt(ctx, "proj-1", domainbilling.MetricAPICalls, h14, 10))
	require.NoError(t, counter.IncrAt(ctx, "proj-1", domainbilling.MetricAPICalls, h15, 7))
	require.NoError(t, counter.IncrAt(ctx, "proj-1", domainbilling.MetricFunctionDurationMS, h15, 100))
	require.NoError(t, counter.Set(ctx, "proj-1", domainbilling.MetricStorageBytes, h15, 4096))

	require.NoError(t, b.RunWorkerOnce(ctx, now))
	require.NoError(t, b.RunWorkerOnce(ctx, now)) // 重跑不翻倍

	r14, err := rollups.Get(ctx, "proj-1", domainbilling.MetricAPICalls, h14)
	require.NoError(t, err)
	require.NotNil(t, r14)
	require.Equal(t, int64(10), r14.Value)

	r15, err := rollups.Get(ctx, "proj-1", domainbilling.MetricAPICalls, h15)
	require.NoError(t, err)
	require.Equal(t, int64(7), r15.Value)

	sum, err := rollups.SumByMetric(ctx, "proj-1", domainbilling.MonthBucket(now), domainbilling.NextMonth(now))
	require.NoError(t, err)
	require.Equal(t, int64(17), sum[domainbilling.MetricAPICalls])
	require.Equal(t, int64(100), sum[domainbilling.MetricFunctionDurationMS])
	require.Equal(t, int64(4096), sum[domainbilling.MetricStorageBytes])

	st, err := statements.Get(ctx, "proj-1", domainbilling.MonthBucket(now))
	require.NoError(t, err)
	require.NotNil(t, st)
	require.Equal(t, domainbilling.StatementDraft, st.Status)
	require.Equal(t, sum, st.Details.Metrics)

	principal := &shared.Principal{ProjectID: "proj-1"}
	qctx := contexts.WithPrincipal(ctx, principal)
	usage, err := b.GetUsage(qctx, UsageQuery{
		PeriodStart: domainbilling.MonthBucket(now),
		PeriodEnd:   domainbilling.NextMonth(now),
	})
	require.NoError(t, err)
	require.Equal(t, int64(17), usage.Metrics[domainbilling.MetricAPICalls])
	require.Equal(t, int64(4096), usage.Metrics[domainbilling.MetricStorageBytes])
}

func TestStatementFinalizesPreviousMonth(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	counter, _, statements, b := newTestBilling(t)
	july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	hour := time.Date(2026, 7, 31, 23, 0, 0, 0, time.UTC)
	require.NoError(t, counter.IncrAt(ctx, "proj-1", domainbilling.MetricAPICalls, hour, 3))

	now := time.Date(2026, 8, 1, 0, 10, 0, 0, time.UTC)
	require.NoError(t, b.RunWorkerOnce(ctx, now))

	st, err := statements.Get(ctx, "proj-1", july)
	require.NoError(t, err)
	require.NotNil(t, st)
	require.Equal(t, domainbilling.StatementFinal, st.Status)
	require.Equal(t, int64(3), st.Details.Metrics[domainbilling.MetricAPICalls])
	require.NotNil(t, st.FinalizedAt)

	// final 后重跑不改明细（即使 Redis 被改）。
	require.NoError(t, counter.IncrAt(ctx, "proj-1", domainbilling.MetricAPICalls, hour, 9))
	require.NoError(t, b.RunWorkerOnce(ctx, now))
	st2, err := statements.Get(ctx, "proj-1", july)
	require.NoError(t, err)
	require.Equal(t, int64(3), st2.Details.Metrics[domainbilling.MetricAPICalls])
}

func TestGetUsageRejectsUnknownMetric(t *testing.T) {
	t.Parallel()
	_, _, _, b := newTestBilling(t)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{ProjectID: "proj-1"})
	_, err := b.GetUsage(ctx, UsageQuery{Metric: "realtime_messages"})
	require.Error(t, err)
}
