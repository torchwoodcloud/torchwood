package subscriptions

import (
	"context"

	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// compositeFulfiller 把 topup/item_purchase 交给资产履约器，subscription
// 交给本包 FulfillPaidOrder（同一 sql.Tx）。
type compositeFulfiller struct {
	items domainpayments.Fulfiller
	subs  *Subscriptions
}

// NewOrderFulfiller 构造支付履约器（Wire 注入，替换 assets.NewOrderFulfiller）。
func NewOrderFulfiller(a *appassets.Assets, s *Subscriptions) domainpayments.Fulfiller {
	return &compositeFulfiller{
		items: appassets.NewOrderFulfiller(a),
		subs:  s,
	}
}

func (f *compositeFulfiller) Fulfill(ctx context.Context, order *domainpayments.Order) (string, error) {
	if order != nil && order.PurposeKind == domainpayments.PurposeSubscription {
		return f.subs.FulfillPaidOrder(ctx, order)
	}
	return f.items.Fulfill(ctx, order)
}

func (f *compositeFulfiller) Reverse(ctx context.Context, order *domainpayments.Order) error {
	if order != nil && order.PurposeKind == domainpayments.PurposeSubscription {
		return nil
	}
	return f.items.Reverse(ctx, order)
}

// FulfillPaidOrder 订单 paid 后激活/续期订阅 + benefits（设计 §1.5 subscription）。
func (s *Subscriptions) FulfillPaidOrder(ctx context.Context, order *domainpayments.Order) (string, error) {
	if order == nil {
		return "", status.Error(codes.Internal, "nil order")
	}
	subID, _, _, err := parseSubscriptionPurpose(order.Purpose)
	if err != nil {
		return "", err
	}
	ctx = withSystemPrincipal(ctx, order.ProjectID)
	sub, err := s.subs.GetByIDForUpdate(ctx, order.ProjectID, subID)
	if err != nil {
		return "", err
	}
	if sub == nil {
		return "", status.Error(codes.NotFound, "subscription not found")
	}
	if sub.UserID != order.UserID {
		return "", status.Error(codes.FailedPrecondition, "subscription user mismatch")
	}
	if sub.Status.IsTerminal() {
		return "subscription:" + sub.ID, nil
	}
	plan, err := s.plans.GetByIDForShare(ctx, sub.ProjectID, sub.PlanID)
	if err != nil {
		return "", err
	}
	if plan == nil {
		return "", status.Error(codes.NotFound, "plan not found")
	}
	now := s.ts()
	if sub.Status == domainsubs.StatusActive && !sub.PeriodDue(now) {
		return "subscription:" + sub.ID, nil
	}
	if err := s.applySuccess(ctx, sub, plan, now); err != nil {
		return "", err
	}
	return "subscription:" + sub.ID, nil
}
