package bunrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// paymentOrderRepo 实现 payments.OrderRepo（public.payment_orders）。
// 所有查询走 clients.Conn(ctx)：在 RunInTx 内调用时复用外层事务
// （总则 10：订单翻转 / 履约 / outbox 同一 sql.Tx，禁止另开连接）。
type paymentOrderRepo struct {
	db *clients.Database
}

// NewPaymentOrderRepository 构造订单仓储。
func NewPaymentOrderRepository(db *clients.Database) payments.OrderRepo {
	return &paymentOrderRepo{db: db}
}

func (r *paymentOrderRepo) Insert(ctx context.Context, order *payments.Order) (*payments.Order, bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, order.ProjectID, "payment_orders", "po")
	if err != nil {
		return nil, false, err
	}
	m := mapOrderToModel(order)
	res, err := conn.NewInsert().Model(m).ModelTableExpr(expr, sch).
		On("CONFLICT (project_id, idempotency_key) DO NOTHING").
		Exec(ctx2)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, true, nil
	}
	existing, err := r.GetByIDempotencyKey(ctx2, order.ProjectID, order.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if existing == nil {
		return nil, false, errors.New("payments: idempotent insert conflict but order not found")
	}
	return existing, false, nil
}

func (r *paymentOrderRepo) GetByIDempotencyKey(ctx context.Context, projectID, key string) (*payments.Order, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_orders", "po")
	if err != nil {
		return nil, err
	}
	m := new(model.PaymentOrder)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("po.project_id = ?", projectID).
		Where("po.idempotency_key = ?", key).
		Scan(ctx2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapOrderToDomain(m), nil
}

func (r *paymentOrderRepo) GetByID(ctx context.Context, projectID, orderID string) (*payments.Order, error) {
	return r.selectOne(ctx, projectID, orderID, "")
}

func (r *paymentOrderRepo) GetByIDForUpdate(ctx context.Context, projectID, orderID string) (*payments.Order, error) {
	return r.selectOne(ctx, projectID, orderID, "UPDATE")
}

func (r *paymentOrderRepo) selectOne(ctx context.Context, projectID, orderID, lock string) (*payments.Order, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_orders", "po")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.PaymentOrder)(nil)).ModelTableExpr(expr, sch).
		Where("po.id = ?", orderID).
		Where("po.project_id = ?", projectID)
	if lock != "" {
		q = q.For(lock)
	}
	m := new(model.PaymentOrder)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapOrderToDomain(m), nil
}

func (r *paymentOrderRepo) GetByProviderRef(ctx context.Context, projectID, provider, providerSessionID, providerOrderID string) (*payments.Order, error) {
	if providerSessionID == "" && providerOrderID == "" {
		return nil, nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_orders", "po")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.PaymentOrder)(nil)).ModelTableExpr(expr, sch).
		Where("po.provider = ?", provider).
		Where("po.project_id = ?", projectID)
	if providerSessionID != "" && providerOrderID != "" {
		q = q.Where("(po.provider_session_id = ? OR po.provider_order_id = ?)", providerSessionID, providerOrderID)
	} else if providerSessionID != "" {
		q = q.Where("po.provider_session_id = ?", providerSessionID)
	} else {
		q = q.Where("po.provider_order_id = ?", providerOrderID)
	}
	q = q.Order("po.created_at").Limit(1)
	m := new(model.PaymentOrder)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapOrderToDomain(m), nil
}

func (r *paymentOrderRepo) Update(ctx context.Context, order *payments.Order, expectStatus payments.OrderStatus) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, order.ProjectID, "payment_orders", "po")
	if err != nil {
		return err
	}
	res, err := conn.NewUpdate().Model((*model.PaymentOrder)(nil)).ModelTableExpr(expr, sch).
		Set("status = ?", string(order.Status)).
		Set("updated_at = ?", order.UpdatedAt).
		Set("paid_at = ?", order.PaidAt).
		Set("provider_session_id = ?", nullIfEmpty(order.ProviderSessionID)).
		Set("provider_order_id = ?", nullIfEmpty(order.ProviderOrderID)).
		Where("po.id = ?", order.ID).
		Where("po.project_id = ?", order.ProjectID).
		Where("po.status = ?", string(expectStatus)).
		Exec(ctx2)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return status.Error(codes.Aborted, "payment order concurrently modified")
	}
	return nil
}

func (r *paymentOrderRepo) ListByUser(ctx context.Context, projectID, userID string, limit int, before time.Time) ([]payments.Order, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_orders", "po")
	if err != nil {
		return nil, err
	}
	var rows []model.PaymentOrder
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("po.project_id = ?", projectID).
		Where("po.user_id = ?", userID).
		Where("po.created_at < ?", before).
		Order("po.created_at DESC").
		Limit(limit).
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapOrdersToDomain(rows), nil
}

func (r *paymentOrderRepo) ListByProject(ctx context.Context, projectID string, limit int, before time.Time) ([]payments.Order, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_orders", "po")
	if err != nil {
		return nil, err
	}
	var rows []model.PaymentOrder
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("po.project_id = ?", projectID).
		Where("po.created_at < ?", before).
		Order("po.created_at DESC").
		Limit(limit).
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapOrdersToDomain(rows), nil
}

func (r *paymentOrderRepo) CloseExpiredInProject(ctx context.Context, projectID string, now time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, _, _, err := Scoped(ctx2, r.db, projectID, "payment_orders", "po"); err != nil {
		return 0, err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return 0, err
	}
	res, err := r.db.Conn(ctx2).ExecContext(ctx2, fmt.Sprintf(`
UPDATE %s.payment_orders po
SET status = ?, updated_at = ?
WHERE po.id IN (
    SELECT id FROM %s.payment_orders
    WHERE status IN ('created', 'paying') AND expires_at <= ?
    ORDER BY expires_at
    LIMIT ?
    FOR UPDATE SKIP LOCKED
)`, quoted, quoted),
		string(payments.OrderStatusClosed), now, now, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// paymentCallbackEventRepo 实现 payments.CallbackEventRepo。
type paymentCallbackEventRepo struct {
	db *clients.Database
}

// NewPaymentCallbackEventRepository 构造回调事件登记仓储。
func NewPaymentCallbackEventRepository(db *clients.Database) payments.CallbackEventRepo {
	return &paymentCallbackEventRepo{db: db}
}

// InsertIfAbsent 幂等锚点二：(provider, provider_event_id) 冲突时返回 false。
func (r *paymentCallbackEventRepo) InsertIfAbsent(ctx context.Context, event *payments.CallbackEvent, projectID, orderID string) (bool, error) {
	payload, err := json.Marshal(map[string]any{
		"provider":            event.Provider,
		"provider_event_id":   event.ProviderEventID,
		"provider_session_id": event.ProviderSessionID,
		"provider_order_id":   event.ProviderOrderID,
		"type":                event.Type,
		"amount":              event.Amount,
		"currency":            event.Currency,
		"order_id":            event.OrderID,
		"received_at":         event.ReceivedAt.UTC().Format(time.RFC3339),
		"raw":                 json.RawMessage(event.Raw),
	})
	if err != nil {
		return false, err
	}
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_callback_events", "pce")
	if err != nil {
		return false, err
	}
	res, err := conn.NewInsert().Model(&model.PaymentCallbackEvent{
		ID:              idgen.ULID().String(),
		ProjectID:       nullIfEmpty(projectID),
		Provider:        event.Provider,
		ProviderEventID: event.ProviderEventID,
		EventType:       event.Type,
		OrderID:         nullIfEmpty(orderID),
		Payload:         payload,
		CreatedAt:       event.ReceivedAt,
	}).ModelTableExpr(expr, sch).On("CONFLICT (provider, provider_event_id) DO NOTHING").Exec(ctx2)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// paymentFulfillmentRepo 实现 payments.FulfillmentRepo。
type paymentFulfillmentRepo struct {
	db *clients.Database
}

// NewPaymentFulfillmentRepository 构造履约记录仓储。
func NewPaymentFulfillmentRepository(db *clients.Database) payments.FulfillmentRepo {
	return &paymentFulfillmentRepo{db: db}
}

func (r *paymentFulfillmentRepo) InsertPending(ctx context.Context, f *payments.Fulfillment) (*payments.Fulfillment, bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, f.ProjectID, "payment_fulfillments", "pf")
	if err != nil {
		return nil, false, err
	}
	res, err := conn.NewInsert().Model(&model.PaymentFulfillment{
		ID:          f.ID,
		OrderID:     f.OrderID,
		ProjectID:   f.ProjectID,
		PurposeKind: string(f.PurposeKind),
		Ref:         f.Ref,
		Status:      string(payments.FulfillmentPending),
		Detail:      f.Detail,
		CreatedAt:   f.CreatedAt,
		UpdatedAt:   f.UpdatedAt,
	}).ModelTableExpr(expr, sch).On("CONFLICT (order_id, purpose_kind) DO NOTHING").Exec(ctx2)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, true, nil
	}
	existing, err := r.GetByOrder(ctx2, f.ProjectID, f.OrderID)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (r *paymentFulfillmentRepo) MarkDone(ctx context.Context, projectID, fulfillmentID, ref string, detail map[string]any) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_fulfillments", "pf")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model((*model.PaymentFulfillment)(nil)).ModelTableExpr(expr, sch).
		Set("status = ?", string(payments.FulfillmentDone)).
		Set("ref = ?", ref).
		Set("detail = ?", detail).
		Set("updated_at = ?", time.Now()).
		Where("pf.id = ?", fulfillmentID).
		Exec(ctx2)
	return err
}

func (r *paymentFulfillmentRepo) MarkFailed(ctx context.Context, projectID, fulfillmentID, reason string) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_fulfillments", "pf")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model((*model.PaymentFulfillment)(nil)).ModelTableExpr(expr, sch).
		Set("status = ?", string(payments.FulfillmentFailed)).
		Set("detail = ?", map[string]any{"reason": reason}).
		Set("updated_at = ?", time.Now()).
		Where("pf.id = ?", fulfillmentID).
		Exec(ctx2)
	return err
}

func (r *paymentFulfillmentRepo) GetByOrder(ctx context.Context, projectID, orderID string) (*payments.Fulfillment, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "payment_fulfillments", "pf")
	if err != nil {
		return nil, err
	}
	m := new(model.PaymentFulfillment)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("pf.project_id = ?", projectID).
		Where("pf.order_id = ?", orderID).
		Scan(ctx2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &payments.Fulfillment{
		ID:          m.ID,
		OrderID:     m.OrderID,
		ProjectID:   m.ProjectID,
		PurposeKind: payments.PurposeKind(m.PurposeKind),
		Ref:         m.Ref,
		Status:      payments.FulfillmentStatus(m.Status),
		Detail:      m.Detail,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}, nil
}

func mapOrderToModel(order *payments.Order) *model.PaymentOrder {
	return &model.PaymentOrder{
		ID:                order.ID,
		ProjectID:         order.ProjectID,
		UserID:            order.UserID,
		Provider:          order.Provider,
		IdempotencyKey:    order.IdempotencyKey,
		ProviderSessionID: nullIfEmpty(order.ProviderSessionID),
		ProviderOrderID:   nullIfEmpty(order.ProviderOrderID),
		Amount:            order.Amount,
		Currency:          order.Currency,
		PurposeKind:       string(order.PurposeKind),
		Purpose:           order.Purpose,
		Status:            string(order.Status),
		CreatedAt:         order.CreatedAt,
		UpdatedAt:         order.UpdatedAt,
		PaidAt:            order.PaidAt,
		ExpiresAt:         order.ExpiresAt,
	}
}

func mapOrderToDomain(m *model.PaymentOrder) *payments.Order {
	return &payments.Order{
		ID:                m.ID,
		ProjectID:         m.ProjectID,
		UserID:            m.UserID,
		Provider:          m.Provider,
		IdempotencyKey:    m.IdempotencyKey,
		ProviderSessionID: derefString(m.ProviderSessionID),
		ProviderOrderID:   derefString(m.ProviderOrderID),
		Amount:            m.Amount,
		Currency:          m.Currency,
		PurposeKind:       payments.PurposeKind(m.PurposeKind),
		Purpose:           m.Purpose,
		Status:            payments.OrderStatus(m.Status),
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
		PaidAt:            m.PaidAt,
		ExpiresAt:         m.ExpiresAt,
	}
}

func mapOrdersToDomain(rows []model.PaymentOrder) []payments.Order {
	out := make([]payments.Order, len(rows))
	for i := range rows {
		out[i] = *mapOrderToDomain(&rows[i])
	}
	return out
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
