package subscriptions

import (
	"context"
	"time"

	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GetMySubscription 返回本人当前订阅（Client 面）。
func (s *Subscriptions) GetMySubscription(ctx context.Context, planCode string) (*domainsubs.Subscription, *domainsubs.Plan, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	var planID string
	if planCode != "" {
		code, err := validateCode(planCode)
		if err != nil {
			return nil, nil, err
		}
		plan, err := s.plans.GetByCode(ctx, projectID, code)
		if err != nil {
			return nil, nil, err
		}
		if plan == nil {
			return nil, nil, status.Error(codes.NotFound, "plan not found")
		}
		planID = plan.ID
	}
	sub, err := s.subs.GetCurrentByUser(ctx, projectID, userID, planID)
	if err != nil {
		return nil, nil, err
	}
	if sub == nil {
		return nil, nil, status.Error(codes.NotFound, "subscription not found")
	}
	plan, err := s.plans.GetByID(ctx, projectID, sub.PlanID)
	if err != nil {
		return nil, nil, err
	}
	return sub, plan, nil
}

// CancelAtPeriodEnd 期末生效取消（Client 面，设计验收）。
func (s *Subscriptions) CancelAtPeriodEnd(ctx context.Context, subscriptionID string) (*domainsubs.Subscription, *domainsubs.Plan, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	if subscriptionID == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "subscription_id is required")
	}
	now := s.ts()
	var out *domainsubs.Subscription
	err = s.db.Run(ctx, func(txCtx context.Context) error {
		sub, err := s.subs.GetByIDForUpdate(txCtx, projectID, subscriptionID)
		if err != nil {
			return err
		}
		if sub == nil || sub.UserID != userID {
			return domainsubs.ErrNotFound
		}
		if sub.Status.IsTerminal() || sub.CancelAtPeriodEnd {
			out = sub
			return nil
		}
		from := sub.Status
		sub.CancelAtPeriodEnd = true
		sub.UpdatedAt = now
		if sub.Mode == domainsubs.ModeHosted && sub.ProviderSubID != "" && s.hosted != nil {
			if err := s.hosted.CancelAtPeriodEnd(txCtx, sub.ProviderSubID); err != nil {
				return err
			}
		}
		if err := s.subs.Update(txCtx, sub, from); err != nil {
			return err
		}
		out = sub
		return nil
	})
	if err != nil {
		return nil, nil, mapError(err)
	}
	plan, err := s.plans.GetByID(ctx, projectID, out.PlanID)
	if err != nil {
		return nil, nil, err
	}
	return out, plan, nil
}

// GetSubscription 按 id 取订阅（Server 面）。
func (s *Subscriptions) GetSubscription(ctx context.Context, subscriptionID string) (*domainsubs.Subscription, *domainsubs.Plan, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if subscriptionID == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "subscription_id is required")
	}
	sub, err := s.subs.GetByID(ctx, projectID, subscriptionID)
	if err != nil {
		return nil, nil, err
	}
	if sub == nil {
		return nil, nil, status.Error(codes.NotFound, "subscription not found")
	}
	plan, err := s.plans.GetByID(ctx, projectID, sub.PlanID)
	if err != nil {
		return nil, nil, err
	}
	return sub, plan, nil
}

// ListProjectSubscriptions 项目订阅列表（Server 面）。
func (s *Subscriptions) ListProjectSubscriptions(ctx context.Context, limit int, before time.Time) ([]domainsubs.Subscription, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	limit, before = normalizeList(limit, before)
	return s.subs.ListByProject(ctx, projectID, limit, before)
}

// ForceCancel 立即取消（Server 面）。
func (s *Subscriptions) ForceCancel(ctx context.Context, subscriptionID string) (*domainsubs.Subscription, *domainsubs.Plan, error) {
	return s.forceTerminal(ctx, subscriptionID, domainsubs.StatusCanceled, domainsubs.EventCanceled)
}

// ForceExpire 立即过期（Server 面）。
func (s *Subscriptions) ForceExpire(ctx context.Context, subscriptionID string) (*domainsubs.Subscription, *domainsubs.Plan, error) {
	return s.forceTerminal(ctx, subscriptionID, domainsubs.StatusExpired, domainsubs.EventExpired)
}

func (s *Subscriptions) forceTerminal(ctx context.Context, subscriptionID string, to domainsubs.Status, event string) (*domainsubs.Subscription, *domainsubs.Plan, error) {
	if err := requireServerWrite(ctx); err != nil {
		return nil, nil, err
	}
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if subscriptionID == "" {
		return nil, nil, status.Error(codes.InvalidArgument, "subscription_id is required")
	}
	now := s.ts()
	var out *domainsubs.Subscription
	err = s.db.Run(ctx, func(txCtx context.Context) error {
		sub, err := s.subs.GetByIDForUpdate(txCtx, projectID, subscriptionID)
		if err != nil {
			return err
		}
		if sub == nil {
			return domainsubs.ErrNotFound
		}
		if sub.Status == to {
			out = sub
			return nil
		}
		from := sub.Status
		if err := sub.Transition(to, now); err != nil {
			return err
		}
		sub.CancelAtPeriodEnd = false
		if to == domainsubs.StatusCanceled && sub.Mode == domainsubs.ModeHosted && sub.ProviderSubID != "" && s.hosted != nil {
			if err := s.hosted.CancelNow(txCtx, sub.ProviderSubID); err != nil {
				return err
			}
		}
		if err := s.subs.Update(txCtx, sub, from); err != nil {
			return err
		}
		if err := s.publish(txCtx, sub, event, now); err != nil {
			return err
		}
		out = sub
		return nil
	})
	if err != nil {
		return nil, nil, mapError(err)
	}
	plan, err := s.plans.GetByID(ctx, projectID, out.PlanID)
	if err != nil {
		return nil, nil, err
	}
	return out, plan, nil
}
