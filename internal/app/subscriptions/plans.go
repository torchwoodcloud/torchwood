package subscriptions

import (
	"context"
	"strings"
	"time"

	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CreatePlanCommand 是创建计划入参。
type CreatePlanCommand struct {
	Code              string
	Name              string
	Amount            int64
	Currency          string
	Interval          domainsubs.Interval
	IntervalDays      int64
	GraceDays         int32
	TrialDays         int32
	Benefits          domainsubs.Benefits
	ProviderOverrides domainsubs.ProviderOverrides
}

// UpdatePlanCommand 是更新计划入参（未设置=不修改；code 不可变）。
type UpdatePlanCommand struct {
	PlanID            string
	Name              *string
	Amount            *int64
	Currency          *string
	Interval          *domainsubs.Interval
	IntervalDays      *int64
	GraceDays         *int32
	TrialDays         *int32
	Benefits          *domainsubs.Benefits
	ProviderOverrides *domainsubs.ProviderOverrides
	Status            *domainsubs.PlanStatus
}

// CreatePlan 创建订阅计划（Server 面）。
func (s *Subscriptions) CreatePlan(ctx context.Context, cmd CreatePlanCommand) (*domainsubs.Plan, error) {
	if err := requireServerWrite(ctx); err != nil {
		return nil, err
	}
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	code, err := validateCode(cmd.Code)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cmd.Name) == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if !currencyPattern.MatchString(cmd.Currency) {
		return nil, status.Error(codes.InvalidArgument, "currency must be a 3-letter ISO-4217 code")
	}
	now := s.ts()
	plan := &domainsubs.Plan{
		ID:                newID(),
		ProjectID:         projectID,
		Code:              code,
		Name:              strings.TrimSpace(cmd.Name),
		Amount:            cmd.Amount,
		Currency:          strings.ToUpper(cmd.Currency),
		Interval:          cmd.Interval,
		IntervalDays:      cmd.IntervalDays,
		GraceDays:         cmd.GraceDays,
		TrialDays:         cmd.TrialDays,
		Benefits:          cmd.Benefits,
		ProviderOverrides: cmd.ProviderOverrides,
		Status:            domainsubs.PlanStatusActive,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := plan.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if err := s.plans.Insert(ctx, plan); err != nil {
		return nil, mapError(err)
	}
	return plan, nil
}

// GetPlan 按 id 取计划。
func (s *Subscriptions) GetPlan(ctx context.Context, planID string) (*domainsubs.Plan, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if planID == "" {
		return nil, status.Error(codes.InvalidArgument, "plan_id is required")
	}
	plan, err := s.plans.GetByID(ctx, projectID, planID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, status.Error(codes.NotFound, "plan not found")
	}
	return plan, nil
}

// ListPlans 列出项目计划（Server 面含归档）。
func (s *Subscriptions) ListPlans(ctx context.Context, includeArchived bool, limit int, before time.Time) ([]domainsubs.Plan, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	limit, before = normalizeList(limit, before)
	return s.plans.List(ctx, projectID, includeArchived, limit, before)
}

// ListClientPlans 列出活跃计划（Client 面）。
func (s *Subscriptions) ListClientPlans(ctx context.Context, limit int, before time.Time) ([]domainsubs.Plan, error) {
	projectID, _, err := endUser(ctx)
	if err != nil {
		return nil, err
	}
	limit, before = normalizeList(limit, before)
	return s.plans.List(ctx, projectID, false, limit, before)
}

// UpdatePlan 更新计划（未设置=不修改）。
func (s *Subscriptions) UpdatePlan(ctx context.Context, cmd UpdatePlanCommand) (*domainsubs.Plan, error) {
	if err := requireServerWrite(ctx); err != nil {
		return nil, err
	}
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if cmd.PlanID == "" {
		return nil, status.Error(codes.InvalidArgument, "plan_id is required")
	}
	plan, err := s.plans.GetByID(ctx, projectID, cmd.PlanID)
	if err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, status.Error(codes.NotFound, "plan not found")
	}
	if cmd.Name != nil {
		if strings.TrimSpace(*cmd.Name) == "" {
			return nil, status.Error(codes.InvalidArgument, "name is required")
		}
		plan.Name = strings.TrimSpace(*cmd.Name)
	}
	if cmd.Amount != nil {
		plan.Amount = *cmd.Amount
	}
	if cmd.Currency != nil {
		if !currencyPattern.MatchString(*cmd.Currency) {
			return nil, status.Error(codes.InvalidArgument, "currency must be a 3-letter ISO-4217 code")
		}
		plan.Currency = strings.ToUpper(*cmd.Currency)
	}
	if cmd.Interval != nil {
		plan.Interval = *cmd.Interval
	}
	if cmd.IntervalDays != nil {
		plan.IntervalDays = *cmd.IntervalDays
	}
	if cmd.GraceDays != nil {
		plan.GraceDays = *cmd.GraceDays
	}
	if cmd.TrialDays != nil {
		plan.TrialDays = *cmd.TrialDays
	}
	if cmd.Benefits != nil {
		plan.Benefits = *cmd.Benefits
	}
	if cmd.ProviderOverrides != nil {
		plan.ProviderOverrides = *cmd.ProviderOverrides
	}
	if cmd.Status != nil {
		switch *cmd.Status {
		case domainsubs.PlanStatusActive, domainsubs.PlanStatusArchived:
			plan.Status = *cmd.Status
		default:
			return nil, status.Errorf(codes.InvalidArgument, "invalid plan status %q", *cmd.Status)
		}
	}
	if err := plan.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	plan.UpdatedAt = s.ts()
	if err := s.plans.Update(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// DeletePlan 归档计划（不物理删除，以免既有订阅失引用）。
func (s *Subscriptions) DeletePlan(ctx context.Context, planID string) error {
	if err := requireServerWrite(ctx); err != nil {
		return err
	}
	st := domainsubs.PlanStatusArchived
	_, err := s.UpdatePlan(ctx, UpdatePlanCommand{PlanID: planID, Status: &st})
	return err
}
