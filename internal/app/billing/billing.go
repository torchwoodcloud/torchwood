// Package billing 是 v3 平台用量计费 use-case（设计 §4）：
// Redis 小时计数落 usage_rollups、月聚合 billing_statements、Server 查询。
package billing

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	domainstorage "github.com/torchwoodcloud/torchwood/internal/domain/storage"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	defaultListLimit = 25
	maxListLimit     = 100
	// lookbackHours 与 Redis TTL 对齐：worker 停机后重启扫未落表的完整小时。
	lookbackHours = 48
)

var usageRollupLag = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "torchwood_usage_rollup_lag_seconds",
	Help: "Seconds since the newest complete hour bucket was rolled up.",
})

func init() {
	prometheus.MustRegister(usageRollupLag)
}

// Billing 是用量计费用例聚合。
type Billing struct {
	counter    domainbilling.UsageCounter
	rollups    domainbilling.UsageRepo
	statements domainbilling.StatementRepo
	projects   projects.Repository
	files      domainstorage.FileRepository
	logger     *slog.Logger
	now        func() time.Time
}

// NewBilling 构造 use-case（Wire）。
func NewBilling(
	counter domainbilling.UsageCounter,
	rollups domainbilling.UsageRepo,
	statements domainbilling.StatementRepo,
	projectsRepo projects.Repository,
	files domainstorage.FileRepository,
	logger *slog.Logger,
) *Billing {
	if logger == nil {
		logger = slog.Default()
	}
	return &Billing{
		counter:    counter,
		rollups:    rollups,
		statements: statements,
		projects:   projectsRepo,
		files:      files,
		logger:     logger,
		now:        time.Now,
	}
}

func (b *Billing) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

// RunWorkerOnce 是 5min worker 入口：采样存储快照 → 扫完整小时落 rollup → 月账单。
func (b *Billing) RunWorkerOnce(ctx context.Context, now time.Time) error {
	now = now.UTC()
	currentHour := domainbilling.HourBucket(now)
	if err := b.sampleStorage(ctx, currentHour); err != nil {
		b.logger.Error("sample storage usage failed", "error", err)
	}
	if err := b.rollupCompleteHours(ctx, now); err != nil {
		return err
	}
	return b.upsertStatements(ctx, now)
}

func (b *Billing) sampleStorage(ctx context.Context, hour time.Time) error {
	if b.projects == nil || b.files == nil || b.counter == nil {
		return nil
	}
	list, err := b.projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		p := list[i]
		total, err := b.files.SumSize(ctx, p.ID)
		if err != nil {
			b.logger.Warn("sum storage bytes failed", "project_id", p.ID, "error", err)
			continue
		}
		if total < 0 {
			total = 0
		}
		if err := b.counter.Set(ctx, p.ID, domainbilling.MetricStorageBytes, hour, total); err != nil {
			b.logger.Warn("set storage bytes bucket failed", "project_id", p.ID, "error", err)
		}
	}
	return nil
}

func (b *Billing) rollupCompleteHours(ctx context.Context, now time.Time) error {
	if b.counter == nil || b.rollups == nil {
		return nil
	}
	currentHour := domainbilling.HourBucket(now)
	oldest := currentHour.Add(-time.Duration(lookbackHours) * time.Hour)
	var newestRolled time.Time
	for hour := oldest; hour.Before(currentHour); hour = hour.Add(time.Hour) {
		buckets, err := b.counter.ListHour(ctx, hour)
		if err != nil {
			return err
		}
		for _, bucket := range buckets {
			if bucket.Value < 0 || !domainbilling.KnownMetric(bucket.Metric) {
				continue
			}
			if err := b.rollups.Upsert(ctx, &domainbilling.Rollup{
				ID:          idgen.ULID().String(),
				ProjectID:   bucket.ProjectID,
				Metric:      bucket.Metric,
				PeriodStart: hour,
				Value:       bucket.Value,
			}); err != nil {
				return err
			}
		}
		newestRolled = hour
	}
	if !newestRolled.IsZero() {
		lag := now.Sub(newestRolled.Add(time.Hour)).Seconds()
		if lag < 0 {
			lag = 0
		}
		usageRollupLag.Set(lag)
	}
	return nil
}

func (b *Billing) upsertStatements(ctx context.Context, now time.Time) error {
	if b.rollups == nil || b.statements == nil {
		return nil
	}
	currentMonth := domainbilling.MonthBucket(now)
	prevMonth := currentMonth.AddDate(0, -1, 0)
	if err := b.upsertMonth(ctx, prevMonth, domainbilling.StatementFinal, now); err != nil {
		return err
	}
	return b.upsertMonth(ctx, currentMonth, domainbilling.StatementDraft, now)
}

func (b *Billing) upsertMonth(ctx context.Context, monthStart time.Time, want domainbilling.StatementStatus, now time.Time) error {
	monthEnd := domainbilling.NextMonth(monthStart)
	if b.projects == nil {
		return nil
	}
	all, err := b.projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, p := range all {
		if p.Status != "active" {
			continue
		}
		projectID := p.ID
		metrics, err := b.rollups.SumByMetric(ctx, projectID, monthStart, monthEnd)
		if err != nil {
			return err
		}
		hours, err := b.rollups.DistinctHours(ctx, projectID, monthStart, monthEnd)
		if err != nil {
			return err
		}
		s := &domainbilling.Statement{
			ProjectID:   projectID,
			PeriodStart: monthStart,
			PeriodEnd:   monthEnd,
			Status:      want,
			Details: domainbilling.StatementDetails{
				Metrics: metrics,
				Hours:   hours,
			},
		}
		if want == domainbilling.StatementFinal {
			t := now.UTC()
			s.FinalizedAt = &t
		}
		if err := b.statements.Upsert(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (b *Billing) projectScope(ctx context.Context) (string, error) {
	principal, ok := contexts.Principal(ctx)
	if !ok || principal.ProjectID == "" {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	return principal.ProjectID, nil
}

func normalizeList(limit int) int {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	return limit
}
