package subscriptions

import (
	"context"
	"time"

	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
)

// HandleHostedCallback 处理 Stripe Billing webhook 镜像迁移（设计 §3.1）。
// 调用点位于 payment_callback_events 插入之后的同一 sql.Tx；重放由锚点二短路。
func (s *Subscriptions) HandleHostedCallback(ctx context.Context, event *domainpayments.CallbackEvent) error {
	if event == nil || !domainpayments.IsSubscriptionEvent(event.Type) {
		return nil
	}
	sub, err := s.locateHosted(ctx, event)
	if err != nil || sub == nil {
		return err
	}
	plan, err := s.plans.GetByIDForShare(ctx, sub.ProjectID, sub.PlanID)
	if err != nil {
		return err
	}
	if plan == nil {
		return domainsubs.ErrPlanNotFound
	}
	now := event.ReceivedAt
	if now.IsZero() {
		now = s.ts()
	}
	ctx = withSystemPrincipal(ctx, sub.ProjectID)

	// 终态订阅迟到事件：保留事件登记、不再驱动状态机，避免 markPastDue /
	// applyTerminal 从终态 Transition 必报错，事务回滚连事件登记一并吞掉，
	// 渠道 3 天重推窗口耗尽后事件最终丢失、已付款订阅永久卡死（E-P2-3）。
	// 同时终态订阅的字段（period / cancel_at_period_end / provider_sub_id）
	// 不得被事件旁路改写——return nil 让调用方事务正常提交，仅留事件行。
	if sub.Status.IsTerminal() {
		return nil
	}

	if event.ProviderSubID != "" && sub.ProviderSubID == "" {
		sub.ProviderSubID = event.ProviderSubID
		sub.Provider = event.Provider
		if err := s.upsertIndex(ctx, event.Provider, domainpayments.IndexKindSubscription, event.ProviderSubID, sub.ProjectID); err != nil {
			return err
		}
	}
	// period 字段镜像改写仅对非终态订阅生效（终态在上方已短路）。
	if !event.PeriodStart.IsZero() {
		sub.CurrentPeriodStart = event.PeriodStart
	}
	if !event.PeriodEnd.IsZero() {
		sub.CurrentPeriodEnd = event.PeriodEnd
	}
	sub.CancelAtPeriodEnd = event.CancelAtPeriodEnd

	switch event.Type {
	case domainpayments.CallbackSubscriptionActivated, domainpayments.CallbackSubscriptionRenewed:
		return s.applyHostedPaid(ctx, sub, plan, event, now)
	case domainpayments.CallbackSubscriptionPastDue:
		return s.markPastDue(ctx, sub, plan, now)
	case domainpayments.CallbackSubscriptionCanceled:
		return s.applyTerminal(ctx, sub, domainsubs.StatusCanceled, domainsubs.EventCanceled, now, "canceled")
	case domainpayments.CallbackSubscriptionExpired:
		return s.applyTerminal(ctx, sub, domainsubs.StatusExpired, domainsubs.EventExpired, now, "expired")
	case domainpayments.CallbackSubscriptionUpdated:
		return s.applyHostedUpdated(ctx, sub, plan, event, now)
	}
	return nil
}

func (s *Subscriptions) locateHosted(ctx context.Context, event *domainpayments.CallbackEvent) (*domainsubs.Subscription, error) {
	projectID := event.MetadataProjectID
	if projectID == "" && s.index != nil {
		if event.LocalSubscriptionID != "" {
			pid, err := s.index.Lookup(ctx, event.Provider, domainpayments.IndexKindSubscription, event.LocalSubscriptionID)
			if err != nil {
				return nil, err
			}
			projectID = pid
		}
		if projectID == "" && event.ProviderSubID != "" {
			pid, err := s.index.Lookup(ctx, event.Provider, domainpayments.IndexKindSubscription, event.ProviderSubID)
			if err != nil {
				return nil, err
			}
			projectID = pid
		}
	}
	if projectID == "" {
		return nil, nil
	}
	if event.LocalSubscriptionID != "" {
		sub, err := s.subs.GetByIDForUpdate(ctx, projectID, event.LocalSubscriptionID)
		if err != nil || sub != nil {
			return sub, err
		}
	}
	if event.ProviderSubID != "" {
		return s.subs.GetByProviderSubIDForUpdate(ctx, projectID, event.Provider, event.ProviderSubID)
	}
	return nil, nil
}

func (s *Subscriptions) applyHostedPaid(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, event *domainpayments.CallbackEvent, now time.Time) error {
	if sub.Status.IsTerminal() {
		return nil
	}
	from := sub.Status
	eventName := domainsubs.EventRenewed
	if from == domainsubs.StatusTrialing || event.Type == domainpayments.CallbackSubscriptionActivated {
		eventName = domainsubs.EventActivated
	}
	if err := sub.Transition(domainsubs.StatusActive, now); err != nil {
		return err
	}
	end := sub.CurrentPeriodEnd
	if end.IsZero() {
		n, err := domainsubs.NextPeriodEnd(now, plan.Interval, plan.IntervalDays)
		if err != nil {
			return err
		}
		end = n
		sub.CurrentPeriodStart = now
		sub.CurrentPeriodEnd = end
	}
	sub.GraceUntil = nil
	if err := s.fulfillBenefits(ctx, sub, end); err != nil {
		return err
	}
	if err := s.subs.Update(ctx, sub, from); err != nil {
		return err
	}
	return s.publish(ctx, sub, eventName, now)
}

func (s *Subscriptions) applyHostedUpdated(ctx context.Context, sub *domainsubs.Subscription, plan *domainsubs.Plan, event *domainpayments.CallbackEvent, now time.Time) error {
	from := sub.Status
	switch event.HostedStatus {
	case "active":
		if sub.Status == domainsubs.StatusActive {
			return s.subs.Update(ctx, sub, from) // 回填 provider_sub_id / period
		}
		if sub.Status == domainsubs.StatusTrialing || sub.Status == domainsubs.StatusPastDue {
			return s.applyHostedPaid(ctx, sub, plan, event, now)
		}
	case "trialing":
		if sub.Status == domainsubs.StatusTrialing {
			return s.subs.Update(ctx, sub, from)
		}
	case "past_due", "unpaid":
		return s.markPastDue(ctx, sub, plan, now)
	case "canceled":
		return s.applyTerminal(ctx, sub, domainsubs.StatusCanceled, domainsubs.EventCanceled, now, "canceled")
	case "incomplete_expired":
		return s.applyTerminal(ctx, sub, domainsubs.StatusExpired, domainsubs.EventExpired, now, "expired")
	}
	if from != sub.Status || sub.ProviderSubID != "" {
		return s.subs.Update(ctx, sub, from)
	}
	return nil
}

var _ domainpayments.SubscriptionCallbackHandler = (*Subscriptions)(nil)
