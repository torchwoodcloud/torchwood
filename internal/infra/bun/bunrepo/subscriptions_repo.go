package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type subscriptionPlanRepo struct {
	db *clients.Database
}

// NewSubscriptionPlanRepository 构造计划仓储。
func NewSubscriptionPlanRepository(db *clients.Database) subscriptions.PlanRepo {
	return &subscriptionPlanRepo{db: db}
}

func (r *subscriptionPlanRepo) Insert(ctx context.Context, plan *subscriptions.Plan) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, plan.ProjectID, "subscription_plans", "sp")
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(mapPlanToModel(plan)).ModelTableExpr(expr, sch).Exec(ctx2)
	if isSubUniqueViolation(err) {
		return subscriptions.ErrDuplicateCode
	}
	return err
}

func (r *subscriptionPlanRepo) GetByID(ctx context.Context, projectID, planID string) (*subscriptions.Plan, error) {
	return r.selectPlan(ctx, projectID, "sp.id = ?", planID, "")
}

func (r *subscriptionPlanRepo) GetByCode(ctx context.Context, projectID, code string) (*subscriptions.Plan, error) {
	return r.selectPlan(ctx, projectID, "sp.code = ?", code, "")
}

func (r *subscriptionPlanRepo) GetByCodeForShare(ctx context.Context, projectID, code string) (*subscriptions.Plan, error) {
	return r.selectPlan(ctx, projectID, "sp.code = ?", code, "SHARE")
}

func (r *subscriptionPlanRepo) GetByIDForShare(ctx context.Context, projectID, planID string) (*subscriptions.Plan, error) {
	return r.selectPlan(ctx, projectID, "sp.id = ?", planID, "SHARE")
}

func (r *subscriptionPlanRepo) selectPlan(ctx context.Context, projectID, pred string, arg any, lock string) (*subscriptions.Plan, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscription_plans", "sp")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.SubscriptionPlan)(nil)).ModelTableExpr(expr, sch).
		Where("sp.project_id = ?", projectID).
		Where(pred, arg)
	if lock != "" {
		q = q.For(lock)
	}
	m := new(model.SubscriptionPlan)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapPlanToDomain(m)
}

func (r *subscriptionPlanRepo) List(ctx context.Context, projectID string, includeArchived bool, limit int, before time.Time) ([]subscriptions.Plan, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscription_plans", "sp")
	if err != nil {
		return nil, err
	}
	var rows []model.SubscriptionPlan
	q := conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("sp.project_id = ?", projectID).
		Where("sp.created_at < ?", before)
	if !includeArchived {
		q = q.Where("sp.status = ?", string(subscriptions.PlanStatusActive))
	}
	if err := q.Order("sp.created_at DESC").Limit(limit).Scan(ctx2); err != nil {
		return nil, err
	}
	out := make([]subscriptions.Plan, 0, len(rows))
	for i := range rows {
		p, err := mapPlanToDomain(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, nil
}

func (r *subscriptionPlanRepo) Update(ctx context.Context, plan *subscriptions.Plan) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, plan.ProjectID, "subscription_plans", "sp")
	if err != nil {
		return err
	}
	// 列白名单（bun 更新写规范）：id/project_id/code/created_at 不可变
	//（code 是对外计费标识）。指针列 nil 经 DefaultPlaceholder 渲染为
	// DEFAULT（= NULL，无非平凡默认）。
	_, err = conn.NewUpdate().Model(mapPlanToModel(plan)).ModelTableExpr(expr, sch).
		Column("name", "amount", "currency", "interval", "interval_days",
			"grace_days", "trial_days", "benefits", "provider_overrides", "status", "updated_at").
		WherePK().
		Where("sp.project_id = ?", plan.ProjectID).
		Exec(ctx2)
	return err
}

type subscriptionRepo struct {
	db *clients.Database
}

// NewSubscriptionRepository 构造订阅仓储。
func NewSubscriptionRepository(db *clients.Database) subscriptions.SubscriptionRepo {
	return &subscriptionRepo{db: db}
}

func (r *subscriptionRepo) Insert(ctx context.Context, sub *subscriptions.Subscription) (*subscriptions.Subscription, bool, error) {
	m, err := mapSubToModel(sub)
	if err != nil {
		return nil, false, err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, sub.ProjectID, "subscriptions", "ss")
	if err != nil {
		return nil, false, err
	}
	res, err := conn.NewInsert().Model(m).ModelTableExpr(expr, sch).
		On("CONFLICT (project_id, idempotency_key) DO NOTHING").
		Exec(ctx2)
	if err != nil {
		// 幂等键之外的唯一冲突只可能来自 subscriptions_live_unique
		// （partial unique index，J4-3）：并发 Subscribe 双开同 (user, plan)
		// 活跃订阅，DB 层兜底映射 ErrAlreadySubscribed。
		if isSubscriptionsLiveUniqueViolation(err) {
			return nil, false, subscriptions.ErrAlreadySubscribed
		}
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, true, nil
	}
	existing, err := r.GetByIdempotencyKey(ctx2, sub.ProjectID, sub.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		return nil, false, errors.New("subscriptions: idempotent insert conflict but row not found")
	}
	return existing, false, nil
}

func (r *subscriptionRepo) GetByID(ctx context.Context, projectID, id string) (*subscriptions.Subscription, error) {
	return r.selectSub(ctx, projectID, "ss.id = ?", id, "")
}

func (r *subscriptionRepo) GetByIDForUpdate(ctx context.Context, projectID, id string) (*subscriptions.Subscription, error) {
	return r.selectSub(ctx, projectID, "ss.id = ?", id, "UPDATE")
}

func (r *subscriptionRepo) GetByIdempotencyKey(ctx context.Context, projectID, key string) (*subscriptions.Subscription, error) {
	return r.selectSub(ctx, projectID, "ss.idempotency_key = ?", key, "")
}

func (r *subscriptionRepo) GetByProviderSubID(ctx context.Context, projectID, provider, providerSubID string) (*subscriptions.Subscription, error) {
	return r.selectByProvider(ctx, projectID, provider, providerSubID, "")
}

func (r *subscriptionRepo) GetByProviderSubIDForUpdate(ctx context.Context, projectID, provider, providerSubID string) (*subscriptions.Subscription, error) {
	return r.selectByProvider(ctx, projectID, provider, providerSubID, "UPDATE")
}

func (r *subscriptionRepo) selectByProvider(ctx context.Context, projectID, provider, providerSubID, lock string) (*subscriptions.Subscription, error) {
	if provider == "" || providerSubID == "" {
		return nil, nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.Subscription)(nil)).ModelTableExpr(expr, sch).
		Where("ss.project_id = ?", projectID).
		Where("ss.provider = ?", provider).
		Where("ss.provider_sub_id = ?", providerSubID)
	if lock != "" {
		q = q.For(lock)
	}
	m := new(model.Subscription)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapSubToDomain(m)
}

func (r *subscriptionRepo) selectSub(ctx context.Context, projectID, pred string, arg any, lock string) (*subscriptions.Subscription, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.Subscription)(nil)).ModelTableExpr(expr, sch).
		Where("ss.project_id = ?", projectID).
		Where(pred, arg)
	if lock != "" {
		q = q.For(lock)
	}
	m := new(model.Subscription)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapSubToDomain(m)
}

func (r *subscriptionRepo) GetCurrentByUser(ctx context.Context, projectID, userID, planID string) (*subscriptions.Subscription, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.Subscription)(nil)).ModelTableExpr(expr, sch).
		Where("ss.project_id = ?", projectID).
		Where("ss.user_id = ?", userID)
	if planID != "" {
		q = q.Where("ss.plan_id = ?", planID)
	}
	q = q.OrderExpr("CASE WHEN ss.status IN ('trialing','active','past_due') THEN 0 ELSE 1 END").
		Order("ss.created_at DESC").
		Limit(1)
	m := new(model.Subscription)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapSubToDomain(m)
}

func (r *subscriptionRepo) ListNonTerminalByUserPlan(ctx context.Context, projectID, userID, planID string) ([]subscriptions.Subscription, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss")
	if err != nil {
		return nil, err
	}
	var rows []model.Subscription
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ss.project_id = ?", projectID).
		Where("ss.user_id = ?", userID).
		Where("ss.plan_id = ?", planID).
		Where("ss.status IN ('trialing','active','past_due')").
		Order("ss.created_at").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapSubsToDomain(rows)
}

func (r *subscriptionRepo) ListByUser(ctx context.Context, projectID, userID string, limit int, before time.Time) ([]subscriptions.Subscription, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss")
	if err != nil {
		return nil, err
	}
	var rows []model.Subscription
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ss.project_id = ?", projectID).
		Where("ss.user_id = ?", userID).
		Where("ss.created_at < ?", before).
		Order("ss.created_at DESC").
		Limit(limit).
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapSubsToDomain(rows)
}

func (r *subscriptionRepo) ListByProject(ctx context.Context, projectID string, limit int, before time.Time) ([]subscriptions.Subscription, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss")
	if err != nil {
		return nil, err
	}
	var rows []model.Subscription
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ss.project_id = ?", projectID).
		Where("ss.created_at < ?", before).
		Order("ss.created_at DESC").
		Limit(limit).
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapSubsToDomain(rows)
}

func (r *subscriptionRepo) Update(ctx context.Context, sub *subscriptions.Subscription, expectStatus subscriptions.Status) error {
	m, err := mapSubToModel(sub)
	if err != nil {
		return err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, sub.ProjectID, "subscriptions", "ss")
	if err != nil {
		return err
	}
	// 列白名单（bun 更新写规范）：id/project_id/idempotency_key/created_at
	// 不可变；status 保留 expectStatus CAS（并发修改返回 Aborted）。指针列
	// nil 渲染为 DEFAULT（= NULL，无非平凡默认）。
	res, err := conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("user_id", "plan_id", "mode", "provider", "provider_sub_id",
			"status", "current_period_start", "current_period_end", "cancel_at_period_end",
			"grace_until", "billing_asset_code", "benefits", "updated_at").
		WherePK().
		Where("ss.project_id = ?", sub.ProjectID).
		Where("ss.status = ?", string(expectStatus)).
		Exec(ctx2)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return status.Error(codes.Aborted, "subscription concurrently modified")
	}
	return nil
}

func (r *subscriptionRepo) ListDueForBillingInProject(ctx context.Context, projectID string, now time.Time, limit int) ([]subscriptions.Subscription, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, _, err := Scoped(ctx2, r.db, projectID, "subscriptions", "ss"); err != nil {
		return nil, err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return nil, err
	}
	var rows []model.Subscription
	err = r.db.Conn(ctx2).NewRaw(fmt.Sprintf(`
SELECT id, project_id, user_id, plan_id, mode, provider, provider_sub_id, status,
       current_period_start, current_period_end, cancel_at_period_end, grace_until,
       billing_asset_code, benefits, idempotency_key, created_at, updated_at
FROM %s.subscriptions
WHERE mode = ?
  AND status IN ('trialing','active','past_due')
  AND (current_period_end <= ? OR (status = 'past_due' AND (grace_until IS NULL OR grace_until <= ?)))
ORDER BY current_period_end
LIMIT ?
FOR UPDATE SKIP LOCKED`, quoted),
		string(subscriptions.ModePlatform), now, now, limit).Scan(ctx2, &rows)
	if err != nil {
		return nil, err
	}
	return mapSubsToDomain(rows)
}

func mapPlanToModel(p *subscriptions.Plan) *model.SubscriptionPlan {
	benefits, _ := subscriptions.MarshalBenefits(p.Benefits)
	overrides, _ := subscriptions.MarshalOverrides(p.ProviderOverrides)
	return &model.SubscriptionPlan{
		ID:                p.ID,
		ProjectID:         p.ProjectID,
		Code:              p.Code,
		Name:              p.Name,
		Amount:            p.Amount,
		Currency:          p.Currency,
		Interval:          string(p.Interval),
		IntervalDays:      p.IntervalDays,
		GraceDays:         p.GraceDays,
		TrialDays:         p.TrialDays,
		Benefits:          benefits,
		ProviderOverrides: overrides,
		Status:            string(p.Status),
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
	}
}

func mapPlanToDomain(m *model.SubscriptionPlan) (*subscriptions.Plan, error) {
	benefits, err := subscriptions.UnmarshalBenefits(m.Benefits)
	if err != nil {
		return nil, err
	}
	overrides, err := subscriptions.UnmarshalOverrides(m.ProviderOverrides)
	if err != nil {
		return nil, err
	}
	return &subscriptions.Plan{
		ID:                m.ID,
		ProjectID:         m.ProjectID,
		Code:              m.Code,
		Name:              m.Name,
		Amount:            m.Amount,
		Currency:          m.Currency,
		Interval:          subscriptions.Interval(m.Interval),
		IntervalDays:      m.IntervalDays,
		GraceDays:         m.GraceDays,
		TrialDays:         m.TrialDays,
		Benefits:          benefits,
		ProviderOverrides: overrides,
		Status:            subscriptions.PlanStatus(m.Status),
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}, nil
}

func mapSubToModel(s *subscriptions.Subscription) (*model.Subscription, error) {
	benefits, err := subscriptions.MarshalBenefits(s.Benefits)
	if err != nil {
		return nil, err
	}
	return &model.Subscription{
		ID:                 s.ID,
		ProjectID:          s.ProjectID,
		UserID:             s.UserID,
		PlanID:             s.PlanID,
		Mode:               string(s.Mode),
		Provider:           nullIfEmpty(s.Provider),
		ProviderSubID:      nullIfEmpty(s.ProviderSubID),
		Status:             string(s.Status),
		CurrentPeriodStart: s.CurrentPeriodStart,
		CurrentPeriodEnd:   s.CurrentPeriodEnd,
		CancelAtPeriodEnd:  s.CancelAtPeriodEnd,
		GraceUntil:         s.GraceUntil,
		BillingAssetCode:   nullIfEmpty(s.BillingAssetCode),
		Benefits:           benefits,
		IdempotencyKey:     s.IdempotencyKey,
		CreatedAt:          s.CreatedAt,
		UpdatedAt:          s.UpdatedAt,
	}, nil
}

func mapSubToDomain(m *model.Subscription) (*subscriptions.Subscription, error) {
	benefits, err := subscriptions.UnmarshalBenefits(m.Benefits)
	if err != nil {
		return nil, err
	}
	return &subscriptions.Subscription{
		ID:                 m.ID,
		ProjectID:          m.ProjectID,
		UserID:             m.UserID,
		PlanID:             m.PlanID,
		Mode:               subscriptions.Mode(m.Mode),
		Provider:           derefString(m.Provider),
		ProviderSubID:      derefString(m.ProviderSubID),
		Status:             subscriptions.Status(m.Status),
		CurrentPeriodStart: m.CurrentPeriodStart,
		CurrentPeriodEnd:   m.CurrentPeriodEnd,
		CancelAtPeriodEnd:  m.CancelAtPeriodEnd,
		GraceUntil:         m.GraceUntil,
		BillingAssetCode:   derefString(m.BillingAssetCode),
		Benefits:           benefits,
		IdempotencyKey:     m.IdempotencyKey,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}, nil
}

func mapSubsToDomain(rows []model.Subscription) ([]subscriptions.Subscription, error) {
	out := make([]subscriptions.Subscription, 0, len(rows))
	for i := range rows {
		s, err := mapSubToDomain(&rows[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, nil
}

func isSubUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23505"
	}
	s := err.Error()
	return strings.Contains(s, "SQLSTATE 23505") || strings.Contains(s, "unique constraint")
}

// subscriptionsLiveUniqueIndex 是 (project_id, user_id, plan_id) 活跃订阅
// partial unique index 的名字（projectschema 000010 迁移创建，各项目 schema
// 各一份）。状态串与 domain subscriptions 状态常量一致。
const subscriptionsLiveUniqueIndex = "subscriptions_live_unique"

// isSubscriptionsLiveUniqueViolation 识别 subscriptions_live_unique 的 23505
// 冲突（ON CONFLICT 只吞幂等键约束，本索引冲突会以错误浮出）。
// 约束名字段与错误文本一起比对（pgdriver 部分路径 Field('N') 为空）。
func isSubscriptionsLiveUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	name := ""
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) && pgErr.Field('C') == "23505" {
		name = pgErr.Field('N')
	}
	return strings.Contains(strings.ToLower(name+" "+err.Error()), subscriptionsLiveUniqueIndex)
}
