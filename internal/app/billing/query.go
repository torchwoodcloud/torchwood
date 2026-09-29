package billing

import (
	"context"
	"time"

	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// UsageQuery 是 GetUsage 入参。
type UsageQuery struct {
	Metric      string
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// UsageResult 是 GetUsage 出参。
type UsageResult struct {
	ProjectID   string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Metrics     map[string]int64
}

// GetUsage 返回时间窗内各 metric 合计；未完成小时从 Redis 并入（活用量）。
func (b *Billing) GetUsage(ctx context.Context, q UsageQuery) (*UsageResult, error) {
	projectID, err := b.projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if q.Metric != "" && !domainbilling.KnownMetric(q.Metric) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown metric %q", q.Metric)
	}
	from, to := q.PeriodStart, q.PeriodEnd
	if from.IsZero() {
		from = domainbilling.MonthBucket(b.clock())
	} else {
		from = from.UTC()
	}
	if to.IsZero() {
		to = domainbilling.NextMonth(from)
	} else {
		to = to.UTC()
	}
	if !to.After(from) {
		return nil, status.Error(codes.InvalidArgument, "period_end must be after period_start")
	}
	totals, err := b.rollups.SumByMetric(ctx, projectID, from, to)
	if err != nil {
		return nil, err
	}
	if totals == nil {
		totals = map[string]int64{}
	}
	b.mergeLiveHour(ctx, projectID, from, to, totals)
	if q.Metric != "" {
		totals = map[string]int64{q.Metric: totals[q.Metric]}
	}
	return &UsageResult{
		ProjectID:   projectID,
		PeriodStart: from,
		PeriodEnd:   to,
		Metrics:     totals,
	}, nil
}

func (b *Billing) mergeLiveHour(ctx context.Context, projectID string, from, to time.Time, totals map[string]int64) {
	if b.counter == nil {
		return
	}
	hour := domainbilling.HourBucket(b.clock())
	if hour.Before(from) || !hour.Before(to) {
		return
	}
	for _, metric := range domainbilling.AllMetrics {
		n, err := b.counter.Get(ctx, projectID, metric, hour)
		if err != nil || n == 0 {
			continue
		}
		totals[metric] += n
	}
}

// ListRollupsQuery 是 ListRollups 入参。
type ListRollupsQuery struct {
	Metric      string
	PeriodStart time.Time
	PeriodEnd   time.Time
	Limit       int
	PageToken   string
}

// ListRollups 列出项目小时 rollup（period_start DESC）。
func (b *Billing) ListRollups(ctx context.Context, q ListRollupsQuery) ([]domainbilling.Rollup, string, error) {
	projectID, err := b.projectScope(ctx)
	if err != nil {
		return nil, "", err
	}
	if q.Metric != "" && !domainbilling.KnownMetric(q.Metric) {
		return nil, "", status.Errorf(codes.InvalidArgument, "unknown metric %q", q.Metric)
	}
	// rollup 游标 = 时间 keyset + id 决胜（period_start 可同刻多行），
	// 固定 period_start DESC；方向闸拒绝异向 token，legacy 无前缀（含无
	// 决胜段）token 照常兼容。
	c, err := crud.DecodeTimeCursorDirection(q.PageToken, false)
	if err != nil {
		return nil, "", status.Error(codes.InvalidArgument, "invalid page token")
	}
	before, beforeID := c.Time, c.Tiebreak
	limit := normalizeList(q.Limit)
	from, to := q.PeriodStart, q.PeriodEnd
	rows, err := b.rollups.List(ctx, projectID, q.Metric, from, to, limit, before, beforeID)
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(rows) == limit {
		last := rows[len(rows)-1]
		next = crud.EncodeTimeCursor(crud.TimeCursor{Time: last.PeriodStart, Tiebreak: last.ID})
	}
	return rows, next, nil
}

// ListStatements 列出项目月账单（period_start DESC）。
func (b *Billing) ListStatements(ctx context.Context, limit int, pageToken string) ([]domainbilling.Statement, string, error) {
	projectID, err := b.projectScope(ctx)
	if err != nil {
		return nil, "", err
	}
	c, err := crud.DecodeTimeCursorDirection(pageToken, false)
	if err != nil {
		return nil, "", status.Error(codes.InvalidArgument, "invalid page token")
	}
	before := c.Time
	limit = normalizeList(limit)
	rows, err := b.statements.List(ctx, projectID, limit, before)
	if err != nil {
		return nil, "", err
	}
	var next string
	if len(rows) == limit {
		next = crud.EncodeTimeCursor(crud.TimeCursor{Time: rows[len(rows)-1].PeriodStart})
	}
	return rows, next, nil
}
