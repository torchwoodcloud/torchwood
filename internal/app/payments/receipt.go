package payments

import (
	"context"
	"time"

	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// VerifyReceiptResult 是 iOS 验票结果。
type VerifyReceiptResult struct {
	Order            *domainpayments.Order
	TransactionID    string
	IdempotentReplay bool
}

// VerifyReceipt 校验 iOS receipt / JWS，并把对应 ios_iap 订单翻 paid
// （设计 §1.2 / §Security 5：transactionId 全局唯一；跨用户领取拒绝）。
// 履约路径复用 applyPaid，不新增状态机或事件目录。
func (p *Payments) VerifyReceipt(ctx context.Context, orderID string, receipt []byte) (*VerifyReceiptResult, error) {
	projectID, userID, err := p.endUser(ctx)
	if err != nil {
		return nil, err
	}
	if orderID == "" || len(receipt) == 0 {
		return nil, status.Error(codes.InvalidArgument, "order_id and receipt are required")
	}
	provider, err := p.providers.Get(domainpayments.ProviderIOSIAP)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "ios_iap provider is not registered")
	}
	verifier, ok := provider.(domainpayments.ReceiptVerifier)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "ios_iap receipt verification is not available")
	}
	purchased, err := verifier.VerifyReceipt(ctx, domainpayments.VerifyReceiptInput{
		Receipt:   receipt,
		UserID:    userID,
		ProjectID: projectID,
		OrderID:   orderID,
	})
	if err != nil {
		return nil, mapProviderError(err)
	}
	if purchased.TransactionID == "" {
		return nil, status.Error(codes.FailedPrecondition, "receipt verification returned no transaction")
	}

	boundPID, err := p.lookupIndex(ctx, domainpayments.ProviderIOSIAP, domainpayments.IndexKindIOSTransaction, purchased.TransactionID)
	if err != nil {
		return nil, err
	}
	if boundPID != "" {
		bound, err := p.orders.GetByProviderRef(ctx, boundPID, domainpayments.ProviderIOSIAP, "", purchased.TransactionID)
		if err != nil {
			return nil, err
		}
		if bound != nil {
			if bound.UserID != userID || bound.ProjectID != projectID {
				return nil, status.Error(codes.PermissionDenied, "receipt already bound to another user")
			}
			return &VerifyReceiptResult{Order: bound, TransactionID: purchased.TransactionID, IdempotentReplay: true}, nil
		}
	}

	order, err := p.orders.GetByID(ctx, projectID, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil || order.UserID != userID {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	if order.Provider != domainpayments.ProviderIOSIAP {
		return nil, status.Error(codes.InvalidArgument, "order is not an ios_iap order")
	}

	now := time.Now()
	event := &domainpayments.CallbackEvent{
		Provider:          domainpayments.ProviderIOSIAP,
		ProviderEventID:   "ios_txn:" + purchased.TransactionID,
		ProviderOrderID:   purchased.TransactionID,
		ProviderSessionID: purchased.OriginalTransactionID,
		OrderID:           order.ID,
		Type:              domainpayments.CallbackPaid,
		Amount:            purchased.Amount,
		Currency:          purchased.Currency,
		Raw:               append([]byte(nil), receipt...),
		ReceivedAt:        now,
	}

	err = p.db.Run(ctx, func(txCtx context.Context) error {
		inserted, err := p.callbacks.InsertIfAbsent(txCtx, event, order.ProjectID, order.ID)
		if err != nil {
			return err
		}
		if !inserted {
			return nil
		}
		locked, err := p.orders.GetByIDForUpdate(txCtx, order.ProjectID, order.ID)
		if err != nil {
			return err
		}
		if locked == nil {
			return status.Error(codes.NotFound, "order not found")
		}
		if err := p.upsertIndex(txCtx, domainpayments.ProviderIOSIAP, domainpayments.IndexKindIOSTransaction, purchased.TransactionID, projectID); err != nil {
			return err
		}
		return p.applyPaid(txCtx, locked, event, now)
	})
	if err != nil {
		return nil, err
	}

	latest, err := p.orders.GetByID(ctx, projectID, order.ID)
	if err != nil {
		return nil, err
	}
	if latest == nil {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	if boundPID, err := p.lookupIndex(ctx, domainpayments.ProviderIOSIAP, domainpayments.IndexKindIOSTransaction, purchased.TransactionID); err == nil && boundPID != "" {
		if bound2, err := p.orders.GetByProviderRef(ctx, boundPID, domainpayments.ProviderIOSIAP, "", purchased.TransactionID); err == nil && bound2 != nil {
			if bound2.ID != latest.ID && bound2.UserID == userID && bound2.ProjectID == projectID {
				return &VerifyReceiptResult{Order: bound2, TransactionID: purchased.TransactionID, IdempotentReplay: true}, nil
			}
		}
	}
	return &VerifyReceiptResult{Order: latest, TransactionID: purchased.TransactionID, IdempotentReplay: false}, nil
}
