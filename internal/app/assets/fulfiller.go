package assets

import (
	"context"
	"encoding/json"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// orderFulfiller 把订单 paid 履约接到资产 Grant（设计 §1.5）：
// topup → currency Grant；item_purchase → 对应定义 Grant。
// 调用点位于订单翻转的同一 sql.Tx 内。
type orderFulfiller struct {
	assets *Assets
}

// NewOrderFulfiller 构造支付履约器（Wire 注入，替换 PR1 占位实现）。
func NewOrderFulfiller(a *Assets) domainpayments.Fulfiller {
	return &orderFulfiller{assets: a}
}

func (f *orderFulfiller) Fulfill(ctx context.Context, order *domainpayments.Order) (string, error) {
	if order == nil {
		return "", status.Error(codes.Internal, "nil order")
	}
	ctx = withSystemPrincipal(ctx, order.ProjectID)
	switch order.PurposeKind {
	case domainpayments.PurposeTopup:
		return f.grantFromPurpose(ctx, order, true)
	case domainpayments.PurposeItemPurchase:
		return f.grantFromPurpose(ctx, order, false)
	case domainpayments.PurposeSubscription:
		return "", status.Error(codes.Unimplemented, "subscription fulfillment is not supported yet")
	default:
		return "", status.Errorf(codes.FailedPrecondition, "unsupported purpose_kind %q", order.PurposeKind)
	}
}

func (f *orderFulfiller) Reverse(ctx context.Context, order *domainpayments.Order) error {
	if order == nil {
		return status.Error(codes.Internal, "nil order")
	}
	ctx = withSystemPrincipal(ctx, order.ProjectID)
	var code string
	var qty int64
	switch order.PurposeKind {
	case domainpayments.PurposeTopup:
		c, _, err := parsePurpose(order.Purpose, true)
		if err != nil {
			return err
		}
		code = c
		qty = order.Amount
	case domainpayments.PurposeItemPurchase:
		c, q, err := parsePurpose(order.Purpose, false)
		if err != nil {
			return err
		}
		code = c
		qty = q
	case domainpayments.PurposeSubscription:
		return nil
	default:
		return status.Errorf(codes.FailedPrecondition, "unsupported purpose_kind %q", order.PurposeKind)
	}
	if code == "" || qty <= 0 {
		return status.Error(codes.InvalidArgument, "invalid reverse purpose")
	}
	_, err := f.assets.Consume(ctx, ConsumeCommand{
		OwnerType:      domainassets.OwnerTypeUser,
		OwnerID:        order.UserID,
		DefCode:        code,
		Quantity:       qty,
		IdempotencyKey: "reverse:" + order.ID,
		RefType:        "order",
		RefID:          order.ID,
	})
	return err
}

func (f *orderFulfiller) grantFromPurpose(ctx context.Context, order *domainpayments.Order, topup bool) (string, error) {
	code, qty, err := parsePurpose(order.Purpose, topup)
	if err != nil {
		return "", err
	}
	if topup {
		qty = order.Amount
	}
	res, err := f.assets.Grant(ctx, GrantCommand{
		OwnerType:      domainassets.OwnerTypeUser,
		OwnerID:        order.UserID,
		DefCode:        code,
		Quantity:       qty,
		IdempotencyKey: "fulfill:" + order.ID,
		RefType:        "order",
		RefID:          order.ID,
	})
	if err != nil {
		return "", err
	}
	if len(res.Entries) == 0 {
		return "order:" + order.ID, nil
	}
	return res.Entries[0].ID, nil
}

func parsePurpose(raw json.RawMessage, topup bool) (code string, qty int64, err error) {
	if topup {
		var p struct {
			CurrencyCode string `json:"currency_code"`
			Amount       int64  `json:"amount"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", 0, status.Error(codes.InvalidArgument, "invalid topup purpose: currency_code and integer amount required")
		}
		if p.CurrencyCode == "" || p.Amount <= 0 {
			return "", 0, status.Error(codes.InvalidArgument, "purpose.currency_code and positive purpose.amount are required")
		}
		return p.CurrencyCode, p.Amount, nil
	}
	var p struct {
		AssetCode string `json:"asset_code"`
		Quantity  int64  `json:"quantity"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", 0, status.Error(codes.InvalidArgument, "invalid item_purchase purpose: asset_code and integer quantity required")
	}
	if p.AssetCode == "" || p.Quantity <= 0 {
		return "", 0, status.Error(codes.InvalidArgument, "purpose.asset_code and positive purpose.quantity are required")
	}
	return p.AssetCode, p.Quantity, nil
}
