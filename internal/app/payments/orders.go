package payments

import (
	"context"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"time"

	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// currencyPattern 限定 ISO-4217 三字母货币码。
var currencyPattern = regexp.MustCompile(`^[A-Za-z]{3}$`)

// maxIdempotencyKeyLen 是建单幂等键长度上限。
const maxIdempotencyKeyLen = 128

// CreateOrderCommand 是建单入参（Client 面：终端用户为自己下单）。
type CreateOrderCommand struct {
	Provider       string
	Amount         int64 // 最小货币单位（bigint，禁止 float）
	Currency       string
	PurposeKind    domainpayments.PurposeKind
	Purpose        map[string]any
	IdempotencyKey string
	// ExpiresIn 可选有效期；缺省 defaultOrderTTL。
	ExpiresIn  time.Duration
	SuccessURL string
	CancelURL  string
}

// CreatedOrderSpec 是内部建单规格（公开 CreateOrder 与订阅共用校验）。
// 允许 PurposeSubscription；公开 CreateOrder 在调用前拦截该用途。
type CreatedOrderSpec struct {
	ProjectID      string
	UserID         string
	Provider       string
	Amount         int64
	Currency       string
	PurposeKind    domainpayments.PurposeKind
	Purpose        json.RawMessage
	IdempotencyKey string
	ExpiresIn      time.Duration
	Now            time.Time
}

// CreateOrderResult 是建单结果：订单 + 客户端完成支付所需载荷。
// IdempotentReplay=true 表示命中幂等键返回原单（未向渠道重复下单）。
type CreateOrderResult struct {
	Order            *domainpayments.Order
	PaymentURL       string
	IdempotentReplay bool
}

// CreateOrder 建单（Client API，session JWT）：订单先落库（created，
// 幂等键唯一），再向渠道下单翻 paying（设计 §1.3 状态机）。
func (p *Payments) CreateOrder(ctx context.Context, cmd CreateOrderCommand) (*CreateOrderResult, error) {
	projectID, userID, err := p.endUser(ctx)
	if err != nil {
		return nil, err
	}
	if cmd.PurposeKind == domainpayments.PurposeSubscription {
		// 订阅用途只允许 Subscribe 走内部入口，避免 Client 乱建订阅单。
		return nil, status.Error(codes.InvalidArgument, "subscription orders are not supported yet")
	}
	if cmd.PurposeKind == domainpayments.PurposeItemPurchase {
		return nil, status.Error(codes.InvalidArgument, "item_purchase orders are not supported yet")
	}
	if cmd.PurposeKind == domainpayments.PurposeTopup {
		if err := validateTopupAmount(cmd.Purpose, cmd.Amount); err != nil {
			return nil, err
		}
	}
	purpose, err := json.Marshal(cmd.Purpose)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid purpose payload")
	}
	order, err := NewCreatedOrder(CreatedOrderSpec{
		ProjectID:      projectID,
		UserID:         userID,
		Provider:       cmd.Provider,
		Amount:         cmd.Amount,
		Currency:       cmd.Currency,
		PurposeKind:    cmd.PurposeKind,
		Purpose:        purpose,
		IdempotencyKey: cmd.IdempotencyKey,
		ExpiresIn:      cmd.ExpiresIn,
	})
	if err != nil {
		return nil, err
	}
	provider, err := p.providers.Get(cmd.Provider)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported provider %q", cmd.Provider)
	}
	order.Provider = provider.Name()
	existing, inserted, err := p.insertOrderWithIndex(ctx, order)
	if err != nil {
		return nil, err
	}
	if !inserted {
		// 命中幂等键：返回原单。原单可能仍在 created（上次渠道下单失败）——
		// 重放建单允许对 created 原单继续走下单流程；否则原样返回。
		if existing.Status != domainpayments.OrderStatusCreated {
			return &CreateOrderResult{Order: existing, IdempotentReplay: true}, nil
		}
		order = existing
	}

	// iOS IAP 无服务端下单（设计 §1.2）：订单保持 created，等 VerifyReceipt / ASN V2。
	if provider.Name() == domainpayments.ProviderIOSIAP {
		paymentOrdersTotal.WithLabelValues(order.Provider, string(order.Status)).Inc()
		return &CreateOrderResult{Order: order, IdempotentReplay: !inserted}, nil
	}

	// 渠道下单（Checkout Session / 预下单）：成功后同一短事务回填渠道引用并翻 paying。
	successURL, cancelURL := p.resolveCheckoutURLs(cmd.SuccessURL, cmd.CancelURL)
	session, err := provider.CreatePayment(ctx, domainpayments.CreatePaymentInput{
		OrderID:        order.ID,
		ProjectID:      order.ProjectID,
		Amount:         order.Amount,
		Currency:       order.Currency,
		Description:    orderDescription(order),
		ExpiresAt:      order.ExpiresAt,
		SuccessURL:     successURL,
		CancelURL:      cancelURL,
		IdempotencyKey: "order:" + order.ID,
	})
	if err != nil {
		// 订单保持 created，等渠道重试或到期由 worker 关单。
		return nil, mapProviderError(err)
	}
	if err := p.db.Run(ctx, func(txCtx context.Context) error {
		locked, err := p.orders.GetByIDForUpdate(txCtx, order.ProjectID, order.ID)
		if err != nil {
			return err
		}
		if locked == nil {
			return status.Error(codes.NotFound, "payment order not found")
		}
		if locked.Status != domainpayments.OrderStatusCreated {
			return nil // 并发已推进（重放 / 回调先到）：保持现状态。
		}
		locked.ProviderSessionID = session.SessionID
		if session.ProviderOrderID != "" {
			locked.ProviderOrderID = session.ProviderOrderID
		}
		if err := locked.Transition(domainpayments.OrderStatusPaying, time.Now()); err != nil {
			return err
		}
		if err := p.orders.Update(txCtx, locked, domainpayments.OrderStatusCreated); err != nil {
			return err
		}
		if err := p.upsertIndex(txCtx, locked.Provider, domainpayments.IndexKindPaymentSession, session.SessionID, locked.ProjectID); err != nil {
			return err
		}
		if session.ProviderOrderID != "" {
			return p.upsertIndex(txCtx, locked.Provider, domainpayments.IndexKindPaymentOrder, session.ProviderOrderID, locked.ProjectID)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	locked, err := p.orders.GetByID(ctx, order.ProjectID, order.ID)
	if err != nil {
		return nil, err
	}
	if locked == nil {
		return nil, status.Error(codes.NotFound, "payment order not found")
	}
	paymentOrdersTotal.WithLabelValues(locked.Provider, string(locked.Status)).Inc()
	return &CreateOrderResult{Order: locked, PaymentURL: session.PaymentURL, IdempotentReplay: !inserted}, nil
}

// orderDescription 返回收银台展示名（按用途拼接，不含敏感信息）。
func orderDescription(order *domainpayments.Order) string {
	return "order " + order.ID
}

func (p *Payments) resolveCheckoutURLs(reqSuccess, reqCancel string) (string, string) {
	if strings.TrimSpace(reqSuccess) != "" && strings.TrimSpace(reqCancel) != "" {
		return strings.TrimSpace(reqSuccess), strings.TrimSpace(reqCancel)
	}
	base := ""
	if p.cfg != nil && p.cfg.GetServer().GetHttp().GetPublicUrl() != "" {
		base = strings.TrimRight(p.cfg.GetServer().GetHttp().GetPublicUrl(), "/")
	}
	if base == "" {
		base = "https://localhost"
	}
	success := strings.TrimSpace(reqSuccess)
	if success == "" {
		success = base + "/?checkout=success&session_id={CHECKOUT_SESSION_ID}"
	}
	cancel := strings.TrimSpace(reqCancel)
	if cancel == "" {
		cancel = base + "/?checkout=cancel&session_id={CHECKOUT_SESSION_ID}"
	}
	return success, cancel
}

func validateTopupAmount(purpose map[string]any, amount int64) error {
	v, ok := purpose["amount"]
	if !ok {
		return status.Error(codes.InvalidArgument, "purpose.amount is required")
	}
	var pa int64
	switch n := v.(type) {
	case int:
		pa = int64(n)
	case int32:
		pa = int64(n)
	case int64:
		pa = n
	case float32:
		// structpb 数值经 AsMap() 落到这里 / float64：非整数必须拒绝，
		// 否则 int64 截断放行 10.5 绕过 pa != amount 校验（R5 J1-3 / E-P2-4）。
		if n != float32(math.Trunc(float64(n))) {
			return status.Error(codes.InvalidArgument, "topup purpose amount must be an integer")
		}
		pa = int64(n)
	case float64:
		if n != math.Trunc(n) {
			return status.Error(codes.InvalidArgument, "topup purpose amount must be an integer")
		}
		pa = int64(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return status.Error(codes.InvalidArgument, "topup purpose amount must be an integer")
		}
		pa = i
	default:
		return status.Error(codes.InvalidArgument, "purpose.amount must be an integer")
	}
	if pa != amount {
		return status.Error(codes.InvalidArgument, "purpose.amount must equal order amount")
	}
	return nil
}

// GetMyOrder 返回本人订单（Client 面）。
func (p *Payments) GetMyOrder(ctx context.Context, orderID string) (*domainpayments.Order, error) {
	projectID, userID, err := p.endUser(ctx)
	if err != nil {
		return nil, err
	}
	order, err := p.orders.GetByID(ctx, projectID, orderID)
	if err != nil {
		return nil, err
	}
	if order == nil || order.UserID != userID {
		return nil, status.Error(codes.NotFound, "order not found")
	}
	return order, nil
}

// ListMyOrders 返回本人订单列表（Client 面，created_at 倒序游标分页）。
func (p *Payments) ListMyOrders(ctx context.Context, limit int, before time.Time) ([]domainpayments.Order, error) {
	projectID, userID, err := p.endUser(ctx)
	if err != nil {
		return nil, err
	}
	limit, before = normalizeList(limit, before)
	return p.orders.ListByUser(ctx, projectID, userID, limit, before)
}

// GetOrder 返回项目内订单（Server 面：admin 会话 / payments.read Key）。
func (p *Payments) GetOrder(ctx context.Context, orderID string) (*domainpayments.Order, error) {
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
	return order, nil
}

// ListOrders 返回项目订单列表（Server 面，created_at 倒序游标分页）。
func (p *Payments) ListOrders(ctx context.Context, limit int, before time.Time) ([]domainpayments.Order, error) {
	projectID, err := p.projectScope(ctx)
	if err != nil {
		return nil, err
	}
	limit, before = normalizeList(limit, before)
	return p.orders.ListByProject(ctx, projectID, limit, before)
}

// CloseExpiredOrders 把超时未付（created/paying 超 expires_at）订单翻
// closed（worker 周期任务，设计 §1.3；closed 不在 §5.1 事件目录，不发事件）。
// 全局预算 closeExpiredBatch（K22）；项目遍历按轮转游标起始（队尾饥饿防护）。
func (p *Payments) CloseExpiredOrders(ctx context.Context, now time.Time) (int64, error) {
	if p.projects == nil {
		return 0, nil
	}
	all, err := p.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	n := len(all)
	start := p.scanCursor.Start(n)
	remaining := closeExpiredBatch
	var closed int64
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
		c, err := p.orders.CloseExpiredInProject(ctx, all[idx].ID, now, remaining)
		if err != nil {
			p.logger.Error("close expired orders failed", "project_id", all[idx].ID, "error", err)
			continue
		}
		closed += c
		remaining -= int(c)
	}
	if stopped >= 0 {
		p.scanCursor.ResumeAt(stopped)
	} else {
		p.scanCursor.Complete()
	}
	return closed, nil
}

func (p *Payments) insertOrderWithIndex(ctx context.Context, order *domainpayments.Order) (*domainpayments.Order, bool, error) {
	var existing *domainpayments.Order
	var inserted bool
	err := p.db.Run(ctx, func(txCtx context.Context) error {
		var err error
		existing, inserted, err = InsertCreatedOrder(txCtx, p.orders, p.index, order)
		return err
	})
	return existing, inserted, err
}

// NewCreatedOrder 校验幂等键、金额、币种、TTL 并构造 created 订单。
// 允许 PurposeSubscription；公开 CreateOrder 必须自行拦截该用途。
func NewCreatedOrder(spec CreatedOrderSpec) (*domainpayments.Order, error) {
	if spec.Amount <= 0 {
		return nil, status.Error(codes.InvalidArgument, "amount must be a positive integer in the smallest currency unit")
	}
	if !currencyPattern.MatchString(spec.Currency) {
		return nil, status.Error(codes.InvalidArgument, "currency must be a 3-letter ISO-4217 code")
	}
	if !spec.PurposeKind.IsValid() {
		return nil, status.Errorf(codes.InvalidArgument, "invalid purpose_kind %q", spec.PurposeKind)
	}
	if strings.TrimSpace(spec.IdempotencyKey) == "" {
		return nil, status.Error(codes.InvalidArgument, "idempotency_key is required")
	}
	if len(spec.IdempotencyKey) > maxIdempotencyKeyLen {
		return nil, status.Errorf(codes.InvalidArgument, "idempotency_key exceeds %d characters", maxIdempotencyKeyLen)
	}
	if strings.TrimSpace(spec.Provider) == "" {
		return nil, status.Error(codes.InvalidArgument, "provider is required")
	}
	ttl := spec.ExpiresIn
	if ttl == 0 {
		ttl = defaultOrderTTL
	}
	if ttl < minOrderTTL || ttl > maxOrderTTL {
		return nil, status.Errorf(codes.InvalidArgument, "expires_in out of range (%s ~ %s)", minOrderTTL, maxOrderTTL)
	}
	now := spec.Now
	if now.IsZero() {
		now = time.Now()
	}
	return &domainpayments.Order{
		ID:             newOrderID(),
		ProjectID:      spec.ProjectID,
		UserID:         spec.UserID,
		Provider:       spec.Provider,
		IdempotencyKey: spec.IdempotencyKey,
		Amount:         spec.Amount,
		Currency:       strings.ToUpper(spec.Currency),
		PurposeKind:    spec.PurposeKind,
		Purpose:        spec.Purpose,
		Status:         domainpayments.OrderStatusCreated,
		CreatedAt:      now,
		UpdatedAt:      now,
		ExpiresAt:      now.Add(ttl),
	}, nil
}

// InsertCreatedOrder 是全仓唯一的 orders.Insert 入口：幂等落 created 单并写
// payment_session index。不含渠道 HTTP，可在订阅外层事务内调用。
func InsertCreatedOrder(ctx context.Context, orders domainpayments.OrderRepo, index domainpayments.ProviderIndexRepo, order *domainpayments.Order) (*domainpayments.Order, bool, error) {
	if orders == nil || order == nil {
		return nil, false, status.Error(codes.Internal, "order insert requires repository")
	}
	existing, inserted, err := orders.Insert(ctx, order)
	if err != nil {
		return nil, false, err
	}
	if !inserted {
		return existing, false, nil
	}
	if err := upsertProviderIndex(ctx, index, order.Provider, domainpayments.IndexKindPaymentSession, order.ID, order.ProjectID); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

func (p *Payments) upsertIndex(ctx context.Context, provider, kind, ref, projectID string) error {
	return upsertProviderIndex(ctx, p.index, provider, kind, ref, projectID)
}

func upsertProviderIndex(ctx context.Context, index domainpayments.ProviderIndexRepo, provider, kind, ref, projectID string) error {
	if index == nil || ref == "" {
		return nil
	}
	return index.Upsert(ctx, provider, kind, ref, projectID)
}

// endUser 解析 Client 面调用者：必须为登录终端用户（session JWT）。
func (p *Payments) endUser(ctx context.Context) (projectID, userID string, err error) {
	principal, ok := contexts.Principal(ctx)
	if !ok || principal.ProjectID == "" || principal.UserID == "" {
		return "", "", status.Error(codes.Unauthenticated, "unauthenticated")
	}
	return principal.ProjectID, principal.UserID, nil
}

// projectScope 解析 Server 面调用的项目上下文。
func (p *Payments) projectScope(ctx context.Context) (string, error) {
	principal, ok := contexts.Principal(ctx)
	if !ok || principal.ProjectID == "" {
		return "", status.Error(codes.Unauthenticated, "missing project context")
	}
	return principal.ProjectID, nil
}

// normalizeList 归一化分页参数。
func normalizeList(limit int, before time.Time) (int, time.Time) {
	if limit <= 0 {
		limit = defaultListLimit
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if before.IsZero() {
		before = time.Now().Add(time.Hour)
	}
	return limit, before
}
