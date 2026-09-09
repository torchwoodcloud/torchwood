package client

import (
	"context"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	"google.golang.org/protobuf/types/known/structpb"
)

// PaymentsService 封装 Client API 的支付服务（建单 + 本人订单查询）。
type PaymentsService struct{ c *Client }

// CreateOrder 创建支付订单。amount 为最小货币单位 int64（禁止 float）。
func (p *PaymentsService) CreateOrder(ctx context.Context, req *clientv1.CreateOrderRequest) (*clientv1.CreateOrderResponse, error) {
	return p.c.payments.CreateOrder(ctx, req)
}

// CreateOrderValues 用基本类型建单（purpose 为 JSON 兼容 map）。
func (p *PaymentsService) CreateOrderValues(ctx context.Context, idempotencyKey, provider string, amount int64, currency, purposeKind string, purpose map[string]any) (*clientv1.CreateOrderResponse, error) {
	var purposeStruct *structpb.Struct
	if len(purpose) > 0 {
		st, err := toStruct(purpose)
		if err != nil {
			return nil, err
		}
		purposeStruct = st
	}
	return p.CreateOrder(ctx, &clientv1.CreateOrderRequest{
		IdempotencyKey: idempotencyKey,
		Provider:       provider,
		Amount:         amount,
		Currency:       currency,
		PurposeKind:    purposeKind,
		Purpose:        purposeStruct,
	})
}

// GetMyOrder 查询本人订单。
func (p *PaymentsService) GetMyOrder(ctx context.Context, orderID string) (*clientv1.PaymentOrder, error) {
	return p.c.payments.GetMyOrder(ctx, &clientv1.GetMyOrderRequest{OrderId: orderID})
}

// ListMyOrders 列出本人订单。
func (p *PaymentsService) ListMyOrders(ctx context.Context, pageSize int32, pageToken string) (*clientv1.ListMyOrdersResponse, error) {
	return p.c.payments.ListMyOrders(ctx, &clientv1.ListMyOrdersRequest{
		PageSize:  pageSize,
		PageToken: pageToken,
	})
}

// VerifyReceipt 校验 iOS IAP receipt / StoreKit 2 JWS 并履约对应订单。
func (p *PaymentsService) VerifyReceipt(ctx context.Context, orderID, receipt string) (*clientv1.VerifyReceiptResponse, error) {
	return p.c.payments.VerifyReceipt(ctx, &clientv1.VerifyReceiptRequest{
		OrderId: orderID,
		Receipt: receipt,
	})
}
