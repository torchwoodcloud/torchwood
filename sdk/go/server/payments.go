package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// PaymentsService 封装 Server API 的支付管理（读 + 退款/人工履约）。
type PaymentsService struct {
	c   *Client
	api serverv1.PaymentsServiceClient
}

// ListOrders 列出项目订单。
func (s *PaymentsService) ListOrders(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListOrdersResponse, error) {
	return s.api.ListOrders(ctx, req)
}

// GetOrder 按 ID 获取订单。
func (s *PaymentsService) GetOrder(ctx context.Context, orderID string) (*serverv1.PaymentOrder, error) {
	return s.api.GetOrder(ctx, &serverv1.GetOrderRequest{OrderId: orderID})
}

// Refund 对已支付订单发起退款（一期只翻订单状态，不回收资产）。
func (s *PaymentsService) Refund(ctx context.Context, req *serverv1.RefundRequest) (*serverv1.PaymentOrder, error) {
	return s.api.Refund(ctx, req)
}

// ManualFulfill 人工标记履约完成。
func (s *PaymentsService) ManualFulfill(ctx context.Context, req *serverv1.ManualFulfillRequest) (*serverv1.ManualFulfillResponse, error) {
	return s.api.ManualFulfill(ctx, req)
}
