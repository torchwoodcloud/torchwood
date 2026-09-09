package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
)

// usageRepo 实现 billing.UsageRepo（项目 schema usage_rollups）。
type usageRepo struct {
	db *clients.Database
}

// NewUsageRepository 构造小时 rollup 仓储。
func NewUsageRepository(db *clients.Database) billing.UsageRepo {
	return &usageRepo{db: db}
}

func (r *usageRepo) Upsert(ctx context.Context, rollup *billing.Rollup) error {
	if rollup.ID == "" {
		rollup.ID = idgen.ULID().String()
	}
	now := time.Now().UTC()
	if rollup.CreatedAt.IsZero() {
		rollup.CreatedAt = now
	}
	rollup.UpdatedAt = now
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, rollup.ProjectID, "usage_rollups", "ur")
	if err != nil {
		return err
	}
	m := mapRollupToModel(rollup)
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).
		On("CONFLICT (project_id, metric, period_start) DO UPDATE").
		Set("value = EXCLUDED.value").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx2)
	return err
}

func (r *usageRepo) Get(ctx context.Context, projectID, metric string, periodStart time.Time) (*billing.Rollup, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "usage_rollups", "ur")
	if err != nil {
		return nil, err
	}
	m := new(model.UsageRollup)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("ur.project_id = ?", projectID).
		Where("ur.metric = ?", metric).
		Where("ur.period_start = ?", periodStart.UTC()).
		Scan(ctx2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapRollupToDomain(m), nil
}

func (r *usageRepo) List(ctx context.Context, projectID, metric string, from, to time.Time, limit int, before time.Time, beforeID string) ([]billing.Rollup, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "usage_rollups", "ur")
	if err != nil {
		return nil, err
	}
	var ms []model.UsageRollup
	q := conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("ur.project_id = ?", projectID)
	if metric != "" {
		q = q.Where("ur.metric = ?", metric)
	}
	if !from.IsZero() {
		q = q.Where("ur.period_start >= ?", from.UTC())
	}
	if !to.IsZero() {
		q = q.Where("ur.period_start < ?", to.UTC())
	}
	if !before.IsZero() {
		if beforeID != "" {
			q = q.Where("(ur.period_start, ur.id) < (?, ?)", before.UTC(), beforeID)
		} else {
			q = q.Where("ur.period_start < ?", before.UTC())
		}
	}
	if limit <= 0 {
		limit = 25
	}
	if err := q.Order("ur.period_start DESC", "ur.id DESC").Limit(limit).Scan(ctx2); err != nil {
		return nil, err
	}
	out := make([]billing.Rollup, len(ms))
	for i := range ms {
		out[i] = *mapRollupToDomain(&ms[i])
	}
	return out, nil
}

func (r *usageRepo) SumByMetric(ctx context.Context, projectID string, from, to time.Time) (map[string]int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "usage_rollups", "ur")
	if err != nil {
		return nil, err
	}
	type row struct {
		Metric string `bun:"metric"`
		Total  int64  `bun:"total"`
	}
	q := conn.NewSelect().
		ModelTableExpr(expr, sch).
		ColumnExpr("ur.metric").
		ColumnExpr("SUM(ur.value) AS total").
		Where("ur.project_id = ?", projectID).
		Group("ur.metric")
	if !from.IsZero() {
		q = q.Where("ur.period_start >= ?", from.UTC())
	}
	if !to.IsZero() {
		q = q.Where("ur.period_start < ?", to.UTC())
	}
	var rows []row
	if err := q.Scan(ctx2, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Metric] = row.Total
	}
	return out, nil
}

func (r *usageRepo) DistinctHours(ctx context.Context, projectID string, from, to time.Time) (int, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "usage_rollups", "ur")
	if err != nil {
		return 0, err
	}
	q := conn.NewSelect().
		ModelTableExpr(expr, sch).
		ColumnExpr("COUNT(DISTINCT ur.period_start)").
		Where("ur.project_id = ?", projectID)
	if !from.IsZero() {
		q = q.Where("ur.period_start >= ?", from.UTC())
	}
	if !to.IsZero() {
		q = q.Where("ur.period_start < ?", to.UTC())
	}
	var n int
	if err := q.Scan(ctx2, &n); err != nil {
		return 0, err
	}
	return n, nil
}

func mapRollupToModel(r *billing.Rollup) *model.UsageRollup {
	return &model.UsageRollup{
		ID:          r.ID,
		ProjectID:   r.ProjectID,
		Metric:      r.Metric,
		PeriodStart: r.PeriodStart.UTC(),
		Value:       r.Value,
		CreatedAt:   r.CreatedAt.UTC(),
		UpdatedAt:   r.UpdatedAt.UTC(),
	}
}

func mapRollupToDomain(m *model.UsageRollup) *billing.Rollup {
	return &billing.Rollup{
		ID:          m.ID,
		ProjectID:   m.ProjectID,
		Metric:      m.Metric,
		PeriodStart: m.PeriodStart.UTC(),
		Value:       m.Value,
		CreatedAt:   m.CreatedAt.UTC(),
		UpdatedAt:   m.UpdatedAt.UTC(),
	}
}

// statementRepo 实现 billing.StatementRepo（项目 schema billing_statements）。
type statementRepo struct {
	db *clients.Database
}

// NewBillingStatementRepository 构造月账单仓储。
func NewBillingStatementRepository(db *clients.Database) billing.StatementRepo {
	return &statementRepo{db: db}
}

func (r *statementRepo) Upsert(ctx context.Context, s *billing.Statement) error {
	if s.ID == "" {
		s.ID = idgen.ULID().String()
	}
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	raw, err := billing.MarshalDetails(s.Details)
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, s.ProjectID, "billing_statements", "bs")
	if err != nil {
		return err
	}
	m := &model.BillingStatement{
		ID:          s.ID,
		ProjectID:   s.ProjectID,
		PeriodStart: s.PeriodStart.UTC(),
		PeriodEnd:   s.PeriodEnd.UTC(),
		Status:      string(s.Status),
		Details:     raw,
		CreatedAt:   s.CreatedAt.UTC(),
		UpdatedAt:   s.UpdatedAt.UTC(),
		FinalizedAt: s.FinalizedAt,
	}
	// 已 final 的账单不回退：冲突时仅当现行为 draft 才更新。
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).
		On("CONFLICT (project_id, period_start) DO UPDATE").
		Set("period_end = EXCLUDED.period_end").
		Set("status = EXCLUDED.status").
		Set("details = EXCLUDED.details").
		Set("updated_at = EXCLUDED.updated_at").
		Set("finalized_at = EXCLUDED.finalized_at").
		Where("bs.status <> ?", string(billing.StatementFinal)).
		Exec(ctx2)
	return err
}

func (r *statementRepo) Get(ctx context.Context, projectID string, periodStart time.Time) (*billing.Statement, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "billing_statements", "bs")
	if err != nil {
		return nil, err
	}
	m := new(model.BillingStatement)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("bs.project_id = ?", projectID).
		Where("bs.period_start = ?", periodStart.UTC()).
		Scan(ctx2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapStatementToDomain(m)
}

func (r *statementRepo) List(ctx context.Context, projectID string, limit int, before time.Time) ([]billing.Statement, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "billing_statements", "bs")
	if err != nil {
		return nil, err
	}
	var ms []model.BillingStatement
	q := conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("bs.project_id = ?", projectID)
	if !before.IsZero() {
		q = q.Where("bs.period_start < ?", before.UTC())
	}
	if limit <= 0 {
		limit = 25
	}
	if err := q.Order("bs.period_start DESC").Limit(limit).Scan(ctx2); err != nil {
		return nil, err
	}
	out := make([]billing.Statement, 0, len(ms))
	for i := range ms {
		s, err := mapStatementToDomain(&ms[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

func mapStatementToDomain(m *model.BillingStatement) (*billing.Statement, error) {
	details, err := billing.UnmarshalDetails(m.Details)
	if err != nil {
		return nil, err
	}
	return &billing.Statement{
		ID:          m.ID,
		ProjectID:   m.ProjectID,
		PeriodStart: m.PeriodStart.UTC(),
		PeriodEnd:   m.PeriodEnd.UTC(),
		Status:      billing.StatementStatus(m.Status),
		Details:     details,
		CreatedAt:   m.CreatedAt.UTC(),
		UpdatedAt:   m.UpdatedAt.UTC(),
		FinalizedAt: m.FinalizedAt,
	}, nil
}
