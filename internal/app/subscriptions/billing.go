package subscriptions

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RunBillingCycle 扫描 platform 到期订阅：取消 / 过期 / 扣款续期（worker）。
// 全局预算 billingBatch（K22）；项目遍历按轮转游标起始（队尾饥饿防护）。
func (s *Subscriptions) RunBillingCycle(ctx context.Context, now time.Time) (int64, error) {
	if now.IsZero() {
		now = s.ts()
	}
	if s.projects == nil {
		return 0, nil
	}
	all, err := s.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := s.scanCursor.Start(n)
	remaining := billingBatch
	var billed int64
	stopped := -1
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		if remaining <= 0 {
			stopped = idx
			break
		}
		if all[idx].Status != "active" {
			continue
		}
		batch, err := s.subs.ListDueForBillingInProject(ctx, all[idx].ID, now, remaining)
		if err != nil {
			s.logger.Error("list due subscriptions failed", "project_id", all[idx].ID, "error", err)
			continue
		}
		for j := range batch {
			if err := s.processDue(ctx, &batch[j], now); err != nil {
				s.logger.Error("subscription billing cycle item failed",
					"subscription_id", batch[j].ID, "error", err)
				continue
			}
			billed++
			remaining--
			if remaining <= 0 {
				break
			}
		}
	}
	if stopped >= 0 {
		s.scanCursor.ResumeAt(stopped)
	} else {
		s.scanCursor.Complete()
	}
	return billed, nil
}

func (s *Subscriptions) processDue(ctx context.Context, seed *domainsubs.Subscription, now time.Time) error {
	return s.db.Run(ctx, func(txCtx context.Context) error {
		txCtx = withSystemPrincipal(txCtx, seed.ProjectID)
		sub, err := s.subs.GetByIDForUpdate(txCtx, seed.ProjectID, seed.ID)
		if err != nil {
			return err
		}
		if sub == nil || sub.Mode != domainsubs.ModePlatform {
			return nil
		}
		plan, err := s.plans.GetByIDForShare(txCtx, sub.ProjectID, sub.PlanID)
		if err != nil {
			return err
		}
		if plan == nil {
			return domainsubs.ErrPlanNotFound
		}

		if sub.CancelAtPeriodEnd && sub.PeriodDue(now) && !sub.Status.IsTerminal() {
			return s.applyTerminal(txCtx, sub, domainsubs.StatusCanceled, domainsubs.EventCanceled, now, "canceled")
		}
		if sub.Status == domainsubs.StatusPastDue && sub.GraceElapsed(now) {
			return s.applyTerminal(txCtx, sub, domainsubs.StatusExpired, domainsubs.EventExpired, now, "expired")
		}
		if !sub.PeriodDue(now) && sub.Status != domainsubs.StatusPastDue {
			return nil
		}
		return s.billOrPastDue(txCtx, sub, plan, now)
	})
}

func (s *Subscriptions) applyTerminal(ctx context.Context, sub *domainsubs.Subscription, to domainsubs.Status, event string, now time.Time, result string) error {
	from := sub.Status
	if err := sub.Transition(to, now); err != nil {
		return err
	}
	sub.CancelAtPeriodEnd = false
	if err := s.subs.Update(ctx, sub, from); err != nil {
		return err
	}
	subscriptionBillingCycleTotal.WithLabelValues(result).Inc()
	return s.publish(ctx, sub, event, now)
}

func (s *Subscriptions) billOrPastDue(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, now time.Time) error {
	charged, err := s.tryCharge(ctx, sub, plan)
	if err != nil {
		// 换键重建耗尽（同 cycle 全部命中终态死单，E-P2-2）与余额不足同样
		// 转入 past_due：订阅不再空转，宽限走完后按既有路径 expired。
		if isInsufficient(err) || errors.Is(err, errBillingOrderExhausted) {
			return s.markPastDue(ctx, sub, plan, now)
		}
		return err // 履约/系统错误：状态不前进，整单回滚
	}
	if !charged {
		return s.markPastDue(ctx, sub, plan, now)
	}
	if sub.BillingAssetCode == "" && plan.Amount != 0 {
		return nil
	}
	return s.applySuccess(ctx, sub, plan, now)
}

func (s *Subscriptions) tryCharge(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan) (bool, error) {
	if plan.Amount == 0 {
		return true, nil
	}
	if sub.BillingAssetCode == "" {
		cycle := strconv.FormatInt(sub.CurrentPeriodEnd.UTC().Unix(), 10)
		_, _, err := s.createBillingOrder(ctx, sub, plan, cycle)
		if err != nil {
			return false, err
		}
		return true, nil
	}
	periodKey := strconv.FormatInt(sub.CurrentPeriodEnd.UTC().Unix(), 10)
	_, err := s.assets.Consume(ctx, appassets.ConsumeCommand{
		OwnerType:      domainassets.OwnerTypeUser,
		OwnerID:        sub.UserID,
		DefCode:        sub.BillingAssetCode,
		Quantity:       plan.Amount,
		IdempotencyKey: "sub:" + sub.ID + ":bill:" + periodKey,
		RefType:        "subscription",
		RefID:          sub.ID,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Subscriptions) markPastDue(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, now time.Time) error {
	if sub.Status == domainsubs.StatusPastDue {
		return nil
	}
	from := sub.Status
	if err := sub.Transition(domainsubs.StatusPastDue, now); err != nil {
		return err
	}
	g := domainsubs.ComputeGraceUntil(now, plan.GraceDays)
	sub.GraceUntil = &g
	if err := s.subs.Update(ctx, sub, from); err != nil {
		return err
	}
	subscriptionBillingCycleTotal.WithLabelValues("past_due").Inc()
	return s.publish(ctx, sub, domainsubs.EventPastDue, now)
}

func (s *Subscriptions) applySuccess(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, now time.Time) error {
	from := sub.Status
	event := domainsubs.EventRenewed
	if from == domainsubs.StatusTrialing {
		event = domainsubs.EventActivated
	}

	start := sub.CurrentPeriodEnd
	if !start.After(now) {
		start = now
	}
	end, err := domainsubs.NextPeriodEnd(start, plan.Interval, plan.IntervalDays)
	if err != nil {
		return err
	}
	if err := sub.Transition(domainsubs.StatusActive, now); err != nil {
		return err
	}
	sub.CurrentPeriodStart = start
	sub.CurrentPeriodEnd = end
	sub.GraceUntil = nil

	if err := s.fulfillBenefits(ctx, sub, end); err != nil {
		return err
	}
	if err := s.subs.Update(ctx, sub, from); err != nil {
		return err
	}
	subscriptionBillingCycleTotal.WithLabelValues("success").Inc()
	return s.publish(ctx, sub, event, now)
}

func isInsufficient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, domainassets.ErrInsufficient) {
		return true
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.FailedPrecondition {
		return strings.Contains(strings.ToLower(st.Message()), "insufficient")
	}
	return strings.Contains(strings.ToLower(err.Error()), "insufficient")
}
