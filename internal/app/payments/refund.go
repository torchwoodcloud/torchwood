package payments

import (
	"context"
	"errors"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// requireServerWrite 断言 Server 面写操作主体（纵深防御：admin 会话 /
// API key；角色与 scope 细粒度由拦截器把关，红线 D6 同源约束）。
func (p *Payments) requireServerWrite(ctx context.Context) error {
	return appshared.RequireServerPrincipal(ctx)
}

// Refund 对已支付订单发起退款（Server 面，scope payments.write / owner+admin）。
// 翻订单状态 + 事件，并在同一工作单元调 Fulfiller.Reverse 回收已发放资产
// （A5；Reverse 失败仅记日志不阻塞翻单——资产可能已过期/耗尽）。
func (p *Payments) Refund(ctx context.Context, orderID string, amount int64) (*domainpayments.Order, error) {
	if err := p.requireServerWrite(ctx); err != nil {
		return nil, err
	}
	projectID, err := p.projectScope(ctx)
	if err != nil {
		return nil, err
	}
	order, err := p.orders.GetByID(ctx, projectID, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	switch order.Status {
	case domainpayments.OrderStatusRefunded:
		return order, nil // 幂等：重复退款返回现单。
	case domainpayments.OrderStatusRefunding:
		return order, nil // 渠道确认中。
	case domainpayments.OrderStatusPaid:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "order in status %s cannot be refunded", order.Status)
	}
	if amount < 0 {
		return nil, status.Error(codes.InvalidArgument, "refund amount must be 0 (full) or not exceed order amount")
	}
	if amount != 0 && amount != order.Amount {
		return nil, status.Error(codes.InvalidArgument, "only full refund is supported")
	}

	provider, err := p.providers.Get(order.Provider)
	if err != nil {
		return nil, err
	}
	result, err := provider.Refund(ctx, domainpayments.RefundInput{
		OrderID:         order.ID,
		ProviderOrderID: order.ProviderOrderID,
		Amount:          amount,
		OrderAmount:     order.Amount,
		IdempotencyKey:  "refund:" + order.ID,
	})
	if err != nil {
		return nil, mapProviderError(err)
	}

	now := time.Now()
	to := domainpayments.OrderStatusRefunded
	if !result.Succeeded {
		to = domainpayments.OrderStatusRefunding // 异步退款：等渠道回调确认。
	}
	eventName := domainpayments.EventOrderRefunded
	err = p.db.Run(ctx, func(txCtx context.Context) error {
		locked, err := p.orders.GetByIDForUpdate(txCtx, projectID, orderID)
		if err != nil {
			return err
		}
		if locked == nil {
			return status.Error(codes.NotFound, "order not found")
		}
		if locked.Status != domainpayments.OrderStatusPaid {
			return nil // 并发已推进（回调先翻）：保持现状态。
		}
		from := locked.Status
		if err := locked.Transition(to, now); err != nil {
			return err
		}
		if err := p.orders.Update(txCtx, locked, from); err != nil {
			return err
		}
		if to == domainpayments.OrderStatusRefunded {
			if err := p.fulfiller.Reverse(txCtx, locked); err != nil {
				p.logger.Error("reverse fulfillment on refund failed", "order_id", locked.ID, "error", err)
			}
			paymentOrdersTotal.WithLabelValues(locked.Provider, string(locked.Status)).Inc()
			return p.events.Publish(txCtx, orderEnvelope(locked, eventName, now))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return p.orders.GetByID(ctx, projectID, orderID)
}

// ManualFulfill 人工履约兜底（Server 面，设计 §6）：订单已支付但履约
// 异常时由管理员标记履约完成。PR1 履约为占位记录，本方法把履约行
// 置 done（不改变订单状态）；审计由 gRPC 审计拦截器自动记录。
func (p *Payments) ManualFulfill(ctx context.Context, orderID, reason string) (*domainpayments.Order, *domainpayments.Fulfillment, error) {
	if err := p.requireServerWrite(ctx); err != nil {
		return nil, nil, err
	}
	projectID, err := p.projectScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	order, err := p.orders.GetByID(ctx, projectID, orderID)
	if err != nil {
		return nil, nil, err
	}
	if order == nil {
		return nil, nil, status.Error(codes.NotFound, "order not found")
	}
	if order.Status != domainpayments.OrderStatusPaid {
		return nil, nil, status.Errorf(codes.FailedPrecondition, "manual fulfill requires a paid order, got %s", order.Status)
	}

	now := time.Now()
	var fulfillment *domainpayments.Fulfillment
	err = p.db.Run(ctx, func(txCtx context.Context) error {
		existing, err := p.fulfillments.GetByOrder(txCtx, projectID, orderID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Status == domainpayments.FulfillmentDone {
				fulfillment = existing
				return nil // 幂等。
			}
			ref, err := p.fulfiller.Fulfill(txCtx, order)
			if err != nil {
				return err
			}
			if ref == "" {
				ref = existing.Ref
			}
			if err := p.fulfillments.MarkDone(txCtx, projectID, existing.ID, ref, map[string]any{
				"manual": true,
				"reason": reason,
			}); err != nil {
				return err
			}
			fulfillment, err = p.fulfillments.GetByOrder(txCtx, projectID, orderID)
			return err
		}
		f := &domainpayments.Fulfillment{
			ID:          newOrderID(),
			OrderID:     order.ID,
			ProjectID:   order.ProjectID,
			PurposeKind: order.PurposeKind,
			Ref:         "order:" + order.ID,
			Status:      domainpayments.FulfillmentPending,
			Detail:      map[string]any{"manual": true, "reason": reason},
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		_, inserted, err := p.fulfillments.InsertPending(txCtx, f)
		if err != nil {
			return err
		}
		if !inserted {
			fulfillment, err = p.fulfillments.GetByOrder(txCtx, projectID, orderID)
			if err != nil {
				return err
			}
			if fulfillment != nil && fulfillment.Status == domainpayments.FulfillmentDone {
				return nil
			}
			ref, err := p.fulfiller.Fulfill(txCtx, order)
			if err != nil {
				return err
			}
			if ref == "" {
				ref = fulfillment.Ref
			}
			if err := p.fulfillments.MarkDone(txCtx, projectID, fulfillment.ID, ref, map[string]any{"manual": true, "reason": reason}); err != nil {
				return err
			}
			fulfillment, err = p.fulfillments.GetByOrder(txCtx, projectID, orderID)
			return err
		}
		ref, err := p.fulfiller.Fulfill(txCtx, order)
		if err != nil {
			return err
		}
		if ref == "" {
			ref = f.Ref
		}
		if err := p.fulfillments.MarkDone(txCtx, projectID, f.ID, ref, f.Detail); err != nil {
			return err
		}
		fulfillment, err = p.fulfillments.GetByOrder(txCtx, projectID, orderID)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return order, fulfillment, nil
}

// mapProviderError 把渠道错误映射为 gRPC 状态（渠道原文不透出）。
func mapProviderError(err error) error {
	if domainpayments.ErrNotConfigured(err) {
		return status.Error(codes.FailedPrecondition, "payment provider is not configured")
	}
	if errors.Is(err, domainpayments.ErrUnsupported) {
		return status.Error(codes.Unimplemented, "payment provider does not support this operation")
	}
	if errors.Is(err, domainpayments.ErrSignatureInvalid) {
		return status.Error(codes.Unauthenticated, "receipt verification failed")
	}
	if errors.Is(err, domainpayments.ErrReceiptBoundToOtherUser) {
		return status.Error(codes.PermissionDenied, "receipt already bound to another user")
	}
	if pe := domainpayments.AsProviderError(err); pe != nil {
		switch code := pe.Status; code {
		case 401, 403:
			return status.Error(codes.Unauthenticated, "payment provider rejected credentials")
		case 404:
			return status.Error(codes.NotFound, "payment provider resource not found")
		case 400, 402, 422:
			return status.Error(codes.FailedPrecondition, "payment provider rejected the request")
		default:
			return status.Error(codes.Internal, "payment provider request failed")
		}
	}
	return status.Error(codes.Internal, "payment provider request failed")
}
