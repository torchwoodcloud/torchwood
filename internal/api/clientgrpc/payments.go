package clientgrpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PaymentsService 是终端用户支付面 gRPC handler（薄：鉴权语义在
// use-case，本层只做 DTO 映射）。
type PaymentsService struct {
	clientv1.UnimplementedPaymentsServiceServer
	payments *apppayments.Payments
}

// NewPaymentsService constructs the client payments service.
func NewPaymentsService(payments *apppayments.Payments) *PaymentsService {
	return &PaymentsService{payments: payments}
}

func (s *PaymentsService) CreateOrder(ctx context.Context, req *clientv1.CreateOrderRequest) (*clientv1.CreateOrderResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	purpose, err := structToMap(req.GetPurpose())
	if err != nil {
		return nil, err
	}
	result, err := s.payments.CreateOrder(ctx, apppayments.CreateOrderCommand{
		Provider:       req.GetProvider(),
		Amount:         req.GetAmount(),
		Currency:       req.GetCurrency(),
		PurposeKind:    domainpayments.PurposeKind(req.GetPurposeKind()),
		Purpose:        purpose,
		IdempotencyKey: req.GetIdempotencyKey(),
		SuccessURL:     req.GetSuccessUrl(),
		CancelURL:      req.GetCancelUrl(),
	})
	if err != nil {
		return nil, err
	}
	order, err := mapClientPaymentOrder(result.Order)
	if err != nil {
		return nil, err
	}
	order.PaymentUrl = result.PaymentURL
	return &clientv1.CreateOrderResponse{Order: order, IdempotentReplay: result.IdempotentReplay}, nil
}

func (s *PaymentsService) GetMyOrder(ctx context.Context, req *clientv1.GetMyOrderRequest) (*clientv1.PaymentOrder, error) {
	// order_id required 由 GetMyOrderRequest 的 buf.validate 注解在 validate
	// 拦截器承担（09-api-guide §2.3）。
	order, err := s.payments.GetMyOrder(ctx, req.GetOrderId())
	if err != nil {
		return nil, err
	}
	return mapClientPaymentOrder(order)
}

func (s *PaymentsService) VerifyReceipt(ctx context.Context, req *clientv1.VerifyReceiptRequest) (*clientv1.VerifyReceiptResponse, error) {
	// order_id/receipt required 同上，由 buf.validate 注解承担。
	result, err := s.payments.VerifyReceipt(ctx, req.GetOrderId(), []byte(req.GetReceipt()))
	if err != nil {
		return nil, err
	}
	order, err := mapClientPaymentOrder(result.Order)
	if err != nil {
		return nil, err
	}
	return &clientv1.VerifyReceiptResponse{
		Order:            order,
		TransactionId:    result.TransactionID,
		IdempotentReplay: result.IdempotentReplay,
	}, nil
}

func (s *PaymentsService) ListMyOrders(ctx context.Context, req *clientv1.ListMyOrdersRequest) (*clientv1.ListMyOrdersResponse, error) {
	before, err := decodeOrderCursor(req.GetPageToken())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page token")
	}
	orders, err := s.payments.ListMyOrders(ctx, int(req.GetPageSize()), before)
	if err != nil {
		return nil, err
	}
	out := make([]*clientv1.PaymentOrder, len(orders))
	for i := range orders {
		mapped, err := mapClientPaymentOrder(&orders[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(orders) > 0 {
		meta.NextPageToken = encodeOrderCursor(orders[len(orders)-1].CreatedAt)
	}
	return &clientv1.ListMyOrdersResponse{Orders: out, Meta: meta}, nil
}

// encodeOrderCursor / decodeOrderCursor：不透明游标 = base64(RFC3339Nano)
// 的 created_at（列表固定 created_at DESC）。
func encodeOrderCursor(t time.Time) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano)))
}

func decodeOrderCursor(token string) (time.Time, error) {
	if token == "" {
		return time.Time{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(raw))
}

func mapClientPaymentOrder(order *domainpayments.Order) (*clientv1.PaymentOrder, error) {
	if order == nil {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	purpose, err := rawToStruct(order.Purpose)
	if err != nil {
		return nil, err
	}
	out := &clientv1.PaymentOrder{
		Id:             order.ID,
		Provider:       order.Provider,
		Amount:         order.Amount,
		Currency:       order.Currency,
		PurposeKind:    string(order.PurposeKind),
		Purpose:        purpose,
		Status:         string(order.Status),
		IdempotencyKey: order.IdempotencyKey,
		CreatedAt:      timestamppb.New(order.CreatedAt),
		ExpiresAt:      timestamppb.New(order.ExpiresAt),
	}
	if order.PaidAt != nil {
		out.PaidAt = timestamppb.New(*order.PaidAt)
	}
	return out, nil
}

// structToMap 把 proto Struct 转 map（缺省为空 map）。
func structToMap(s *structpb.Struct) (map[string]any, error) {
	if s == nil {
		return map[string]any{}, nil
	}
	return s.AsMap(), nil
}

// rawToStruct 把订单 purpose JSONB 转 proto Struct（空值返回 nil）。
func rawToStruct(raw json.RawMessage) (*structpb.Struct, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, status.Errorf(codes.Internal, "decode order purpose: %v", err)
	}
	return structpb.NewStruct(m)
}
