package servergrpc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// PaymentsService 是支付管理面 gRPC handler（薄：scope / 角色在拦截器，
// 主体断言在 use-case；写方法自动进审计日志）。
type PaymentsService struct {
	serverv1.UnimplementedPaymentsServiceServer
	payments *apppayments.Payments
}

// NewPaymentsService constructs the server payments service.
func NewPaymentsService(payments *apppayments.Payments) *PaymentsService {
	return &PaymentsService{payments: payments}
}

// withAuditResource 把订单 id 写入审计资源槽（PR0 审计拦截器统一落库）。
func withAuditResource(ctx context.Context, resourceID string) context.Context {
	return contexts.WithAuditResource(ctx, resourceID)
}

func (s *PaymentsService) ListOrders(ctx context.Context, req *serverv1.ListOrdersRequest) (*serverv1.ListOrdersResponse, error) {
	// 时间列（created_at）方向化排序：UNSPECIFIED = DESC（历史默认）。
	ascending := req.GetSortOrder() == sharedv1.SortOrder_SORT_ORDER_ASC
	before, err := decodeServerOrderPage(req.GetPageToken(), ascending)
	if err != nil {
		return nil, invalidServerOrderCursor(err)
	}
	f := domainpayments.OrderListFilter{
		UserID:    req.GetUserId(),
		Status:    domainpayments.OrderStatus(req.GetStatus()),
		Ascending: ascending,
	}
	if ts := req.GetCreatedAfter(); ts != nil {
		f.CreatedAfter = ts.AsTime()
	}
	if ts := req.GetCreatedBefore(); ts != nil {
		f.CreatedBefore = ts.AsTime()
	}
	orders, err := s.payments.ListOrders(ctx, int(req.GetPageSize()), before, f)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.PaymentOrder, len(orders))
	for i := range orders {
		mapped, err := mapServerPaymentOrder(&orders[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	meta := &sharedv1.ListResponseMeta{PageSize: req.GetPageSize()}
	if len(orders) > 0 {
		meta.NextPageToken = encodeServerOrderCursor(orders[len(orders)-1].CreatedAt, ascending)
	}
	return &serverv1.ListOrdersResponse{Orders: out, Meta: meta}, nil
}

func (s *PaymentsService) GetOrder(ctx context.Context, req *serverv1.GetOrderRequest) (*serverv1.PaymentOrder, error) {
	// order_id required 由 buf.validate 注解在 validate 拦截器承担（09-api-guide §2.3）。
	order, err := s.payments.GetOrder(ctx, req.GetOrderId())
	if err != nil {
		return nil, err
	}
	return mapServerPaymentOrder(order)
}

func (s *PaymentsService) Refund(ctx context.Context, req *serverv1.RefundRequest) (*serverv1.PaymentOrder, error) {
	// order_id required 同上，由 buf.validate 注解承担。
	var amount int64
	if req.Amount != nil {
		amount = req.GetAmount()
	}
	order, err := s.payments.Refund(withAuditResource(ctx, req.GetOrderId()), req.GetOrderId(), amount)
	if err != nil {
		return nil, err
	}
	return mapServerPaymentOrder(order)
}

func (s *PaymentsService) ManualFulfill(ctx context.Context, req *serverv1.ManualFulfillRequest) (*serverv1.ManualFulfillResponse, error) {
	// order_id required 同上，由 buf.validate 注解承担。
	order, fulfillment, err := s.payments.ManualFulfill(withAuditResource(ctx, req.GetOrderId()), req.GetOrderId(), req.GetReason())
	if err != nil {
		return nil, err
	}
	mappedOrder, err := mapServerPaymentOrder(order)
	if err != nil {
		return nil, err
	}
	mappedFulfillment, err := mapServerFulfillment(fulfillment)
	if err != nil {
		return nil, err
	}
	return &serverv1.ManualFulfillResponse{Order: mappedOrder, Fulfillment: mappedFulfillment}, nil
}

// encodeServerOrderCursor / decodeServerOrderCursor：不透明游标 =
// base64(方向前缀 + RFC3339Nano) 的 created_at（列表固定按时间列排序）。
// 方向前缀 "a:"=ASC / "d:"=DESC（与 ledger 游标同格式）；无前缀的旧格式 =
// DESC（存量 token 兼容：旧客户端不带 sort_order，语义即 DESC）。
// 注：与 ListUserLedger 的 encodeLedgerCursor/decodeLedgerCursor 行为一致，
// 后续可合并为单一实现。
func encodeServerOrderCursor(t time.Time, ascending bool) string {
	prefix := "d:"
	if ascending {
		prefix = "a:"
	}
	return base64.RawURLEncoding.EncodeToString([]byte(prefix + t.UTC().Format(time.RFC3339Nano)))
}

func decodeServerOrderCursor(token string) (time.Time, bool, error) {
	if token == "" {
		return time.Time{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return time.Time{}, false, err
	}
	if len(raw) > 1 && (raw[0] == 'a' || raw[0] == 'd') && raw[1] == ':' {
		t, err := time.Parse(time.RFC3339Nano, string(raw[2:]))
		return t, raw[0] == 'a', err
	}
	// 旧格式：无方向前缀 = DESC。
	t, err := time.Parse(time.RFC3339Nano, string(raw))
	return t, false, err
}

// errServerOrderMismatch 标记游标方向与请求排序方向不一致（换向必须从第一页
// 重新开始；携带异向游标 = 客户端 bug 或过期缓存，fail-fast 而非静默错位）。
var errServerOrderMismatch = errors.New("page token sort order mismatch")

// decodeServerOrderPage 解析一页的游标并校验方向一致性。
func decodeServerOrderPage(token string, ascending bool) (time.Time, error) {
	t, cursorAsc, err := decodeServerOrderCursor(token)
	if err != nil {
		return time.Time{}, err
	}
	// 空 token（第一页）方向由请求决定，不参与校验。
	if token != "" && cursorAsc != ascending {
		return time.Time{}, errServerOrderMismatch
	}
	return t, nil
}

func invalidServerOrderCursor(err error) error {
	if errors.Is(err, errServerOrderMismatch) {
		return status.Error(codes.InvalidArgument, "page token belongs to another sort order; restart from the first page")
	}
	return status.Error(codes.InvalidArgument, "invalid page token")
}

func mapServerPaymentOrder(order *domainpayments.Order) (*serverv1.PaymentOrder, error) {
	if order == nil {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	purpose, err := rawToStructServer(order.Purpose)
	if err != nil {
		return nil, err
	}
	out := &serverv1.PaymentOrder{
		Id:                order.ID,
		ProjectId:         order.ProjectID,
		UserId:            order.UserID,
		Provider:          order.Provider,
		Amount:            order.Amount,
		Currency:          order.Currency,
		PurposeKind:       string(order.PurposeKind),
		Purpose:           purpose,
		Status:            string(order.Status),
		IdempotencyKey:    order.IdempotencyKey,
		ProviderSessionId: order.ProviderSessionID,
		ProviderOrderId:   order.ProviderOrderID,
		CreatedAt:         timestamppb.New(order.CreatedAt),
		ExpiresAt:         timestamppb.New(order.ExpiresAt),
	}
	if order.PaidAt != nil {
		out.PaidAt = timestamppb.New(*order.PaidAt)
	}
	return out, nil
}

func mapServerFulfillment(f *domainpayments.Fulfillment) (*serverv1.Fulfillment, error) {
	if f == nil {
		return nil, status.Error(codes.NotFound, "fulfillment not found")
	}
	detail, err := mapToStructServer(f.Detail)
	if err != nil {
		return nil, err
	}
	return &serverv1.Fulfillment{
		Id:          f.ID,
		OrderId:     f.OrderID,
		PurposeKind: string(f.PurposeKind),
		Ref:         f.Ref,
		Status:      string(f.Status),
		Detail:      detail,
		CreatedAt:   timestamppb.New(f.CreatedAt),
		UpdatedAt:   timestamppb.New(f.UpdatedAt),
	}, nil
}

// rawToStructServer 把 JSONB 转 proto Struct（空值返回 nil）。
func rawToStructServer(raw json.RawMessage) (*structpb.Struct, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, status.Errorf(codes.Internal, "decode order purpose: %v", err)
	}
	return structpb.NewStruct(m)
}

func mapToStructServer(m map[string]any) (*structpb.Struct, error) {
	if m == nil {
		return nil, nil
	}
	return structpb.NewStruct(m)
}
