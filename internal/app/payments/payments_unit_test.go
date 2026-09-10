package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	infrapayments "github.com/torchwoodcloud/torchwood/internal/infra/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/stripe"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const unitWebhookSecret = "whsec_unit_test" // #nosec G101 -- 测试固定值

// memStore 是订单 / 回调 / 履约 / outbox 的内存库，uow.Run 失败整体回滚。
type memStore struct {
	orders       map[string]*domainpayments.Order
	byIdem       map[string]string
	callbacks    map[string]struct{}
	fulfillments map[string]*domainpayments.Fulfillment
	byFulfillID  map[string]string
	index        map[string]string
	outbox       []domainevents.Envelope
}

func newMemStore() *memStore {
	return &memStore{
		orders:       map[string]*domainpayments.Order{},
		byIdem:       map[string]string{},
		callbacks:    map[string]struct{}{},
		fulfillments: map[string]*domainpayments.Fulfillment{},
		byFulfillID:  map[string]string{},
		index:        map[string]string{},
	}
}

type memIndex struct{ s *memStore }

func indexKey(provider, kind, ref string) string { return provider + "|" + kind + "|" + ref }

func (r memIndex) Lookup(_ context.Context, provider, kind, ref string) (string, error) {
	return r.s.index[indexKey(provider, kind, ref)], nil
}

func (r memIndex) Upsert(_ context.Context, provider, kind, ref, projectID string) error {
	k := indexKey(provider, kind, ref)
	if existing, ok := r.s.index[k]; ok && existing != projectID {
		return status.Error(codes.PermissionDenied, "provider resource already bound to another project")
	}
	r.s.index[k] = projectID
	return nil
}

func (s *memStore) Run(_ context.Context, fn func(context.Context) error) error {
	snap := s.snapshot()
	if err := fn(context.Background()); err != nil {
		s.restore(snap)
		return err
	}
	return nil
}

func (s *memStore) snapshot() memStore {
	out := memStore{
		orders:       make(map[string]*domainpayments.Order, len(s.orders)),
		byIdem:       make(map[string]string, len(s.byIdem)),
		callbacks:    make(map[string]struct{}, len(s.callbacks)),
		fulfillments: make(map[string]*domainpayments.Fulfillment, len(s.fulfillments)),
		byFulfillID:  make(map[string]string, len(s.byFulfillID)),
		index:        make(map[string]string, len(s.index)),
		outbox:       append([]domainevents.Envelope(nil), s.outbox...),
	}
	for k, v := range s.orders {
		out.orders[k] = cloneOrder(v)
	}
	for k, v := range s.byIdem {
		out.byIdem[k] = v
	}
	for k := range s.callbacks {
		out.callbacks[k] = struct{}{}
	}
	for k, v := range s.fulfillments {
		out.fulfillments[k] = cloneFulfillment(v)
	}
	for k, v := range s.byFulfillID {
		out.byFulfillID[k] = v
	}
	for k, v := range s.index {
		out.index[k] = v
	}
	return out
}

func (s *memStore) restore(snap memStore) {
	s.orders = snap.orders
	s.byIdem = snap.byIdem
	s.callbacks = snap.callbacks
	s.fulfillments = snap.fulfillments
	s.byFulfillID = snap.byFulfillID
	s.index = snap.index
	s.outbox = snap.outbox
}

func cloneOrder(o *domainpayments.Order) *domainpayments.Order {
	if o == nil {
		return nil
	}
	cp := *o
	if o.PaidAt != nil {
		t := *o.PaidAt
		cp.PaidAt = &t
	}
	if o.Purpose != nil {
		cp.Purpose = append(json.RawMessage(nil), o.Purpose...)
	}
	return &cp
}

func cloneFulfillment(f *domainpayments.Fulfillment) *domainpayments.Fulfillment {
	if f == nil {
		return nil
	}
	cp := *f
	if f.Detail != nil {
		cp.Detail = make(map[string]any, len(f.Detail))
		for k, v := range f.Detail {
			cp.Detail[k] = v
		}
	}
	return &cp
}

func idemKey(projectID, key string) string  { return projectID + "\x00" + key }
func cbKey(provider, eventID string) string { return provider + "\x00" + eventID }

type memOrders struct{ s *memStore }

func (r memOrders) Insert(_ context.Context, order *domainpayments.Order) (*domainpayments.Order, bool, error) {
	k := idemKey(order.ProjectID, order.IdempotencyKey)
	if id, ok := r.s.byIdem[k]; ok {
		return cloneOrder(r.s.orders[id]), false, nil
	}
	r.s.orders[order.ID] = cloneOrder(order)
	r.s.byIdem[k] = order.ID
	return nil, true, nil
}

func (r memOrders) GetByID(_ context.Context, projectID, orderID string) (*domainpayments.Order, error) {
	return r.get(projectID, orderID)
}

func (r memOrders) GetByIDForUpdate(_ context.Context, projectID, orderID string) (*domainpayments.Order, error) {
	return r.get(projectID, orderID)
}

func (r memOrders) get(projectID, orderID string) (*domainpayments.Order, error) {
	o := r.s.orders[orderID]
	if o == nil {
		return nil, nil
	}
	if projectID != "" && o.ProjectID != projectID {
		return nil, nil
	}
	return cloneOrder(o), nil
}

func (r memOrders) GetByProviderRef(_ context.Context, projectID, provider, sessionID, orderID string) (*domainpayments.Order, error) {
	for _, o := range r.s.orders {
		if o.Provider != provider {
			continue
		}
		if projectID != "" && o.ProjectID != projectID {
			continue
		}
		if (sessionID != "" && o.ProviderSessionID == sessionID) || (orderID != "" && o.ProviderOrderID == orderID) {
			return cloneOrder(o), nil
		}
	}
	return nil, nil
}

func (r memOrders) Update(_ context.Context, order *domainpayments.Order, expect domainpayments.OrderStatus) error {
	cur := r.s.orders[order.ID]
	if cur == nil || cur.Status != expect {
		return status.Error(codes.Aborted, "payment order concurrently modified")
	}
	r.s.orders[order.ID] = cloneOrder(order)
	return nil
}

func (r memOrders) ListByUser(context.Context, string, string, int, time.Time) ([]domainpayments.Order, error) {
	return nil, nil
}
func (r memOrders) ListByProject(context.Context, string, int, time.Time) ([]domainpayments.Order, error) {
	return nil, nil
}
func (r memOrders) CloseExpiredInProject(context.Context, string, time.Time, int) (int64, error) {
	return 0, nil
}

type memCallbacks struct{ s *memStore }

func (r memCallbacks) InsertIfAbsent(_ context.Context, event *domainpayments.CallbackEvent, _, _ string) (bool, error) {
	k := cbKey(event.Provider, event.ProviderEventID)
	if _, ok := r.s.callbacks[k]; ok {
		return false, nil
	}
	r.s.callbacks[k] = struct{}{}
	return true, nil
}

type memFulfillments struct{ s *memStore }

func (r memFulfillments) InsertPending(_ context.Context, f *domainpayments.Fulfillment) (*domainpayments.Fulfillment, bool, error) {
	if existing, ok := r.s.fulfillments[f.OrderID]; ok {
		return cloneFulfillment(existing), false, nil
	}
	cp := cloneFulfillment(f)
	cp.Status = domainpayments.FulfillmentPending
	r.s.fulfillments[f.OrderID] = cp
	r.s.byFulfillID[f.ID] = f.OrderID
	return nil, true, nil
}

func (r memFulfillments) MarkDone(_ context.Context, _, fulfillmentID, ref string, detail map[string]any) error {
	oid, ok := r.s.byFulfillID[fulfillmentID]
	if !ok {
		return errors.New("fulfillment not found")
	}
	f := r.s.fulfillments[oid]
	f.Status = domainpayments.FulfillmentDone
	f.Ref = ref
	if detail != nil {
		f.Detail = detail
	}
	return nil
}

func (r memFulfillments) MarkFailed(_ context.Context, _, fulfillmentID, reason string) error {
	oid, ok := r.s.byFulfillID[fulfillmentID]
	if !ok {
		return errors.New("fulfillment not found")
	}
	f := r.s.fulfillments[oid]
	f.Status = domainpayments.FulfillmentFailed
	f.Detail = map[string]any{"reason": reason}
	return nil
}

func (r memFulfillments) GetByOrder(_ context.Context, _, orderID string) (*domainpayments.Fulfillment, error) {
	return cloneFulfillment(r.s.fulfillments[orderID]), nil
}

type memPublisher struct{ s *memStore }

func (p memPublisher) Publish(_ context.Context, ev domainevents.Envelope) error {
	p.s.outbox = append(p.s.outbox, ev)
	return nil
}

type fakeProvider struct {
	createCalls int
	lastInput   domainpayments.CreatePaymentInput
	createErr   error
	session     *domainpayments.PaymentSession
	verify      func(http.Header, []byte) (*domainpayments.CallbackEvent, error)
	verifyErr   error
	refundRes   *domainpayments.RefundResult
}

func (f *fakeProvider) Name() string { return domainpayments.ProviderStripe }

func (f *fakeProvider) CreatePayment(_ context.Context, in domainpayments.CreatePaymentInput) (*domainpayments.PaymentSession, error) {
	f.createCalls++
	f.lastInput = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.session != nil {
		return f.session, nil
	}
	return &domainpayments.PaymentSession{SessionID: "cs_fake", PaymentURL: "https://pay.example/cs_fake"}, nil
}

func (f *fakeProvider) VerifyCallback(_ context.Context, h http.Header, raw []byte) (*domainpayments.CallbackEvent, error) {
	if f.verifyErr != nil {
		return nil, f.verifyErr
	}
	if f.verify != nil {
		return f.verify(h, raw)
	}
	var ev domainpayments.CallbackEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil, err
	}
	if ev.Provider == "" {
		ev.Provider = f.Name()
	}
	return &ev, nil
}

func (f *fakeProvider) Refund(context.Context, domainpayments.RefundInput) (*domainpayments.RefundResult, error) {
	if f.refundRes != nil {
		return f.refundRes, nil
	}
	return nil, domainpayments.ErrUnsupported
}

type fakeRegistry struct {
	p domainpayments.PaymentProvider
}

func (r fakeRegistry) Get(name string) (domainpayments.PaymentProvider, error) {
	if r.p == nil || r.p.Name() != name {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	return r.p, nil
}

type countingFulfiller struct {
	calls        int
	reverseCalls int
	err          error
	reverseErr   error
}

func (f *countingFulfiller) Fulfill(_ context.Context, order *domainpayments.Order) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return "order:" + order.ID, nil
}

func (f *countingFulfiller) Reverse(_ context.Context, _ *domainpayments.Order) error {
	f.reverseCalls++
	return f.reverseErr
}

type unitEnv struct {
	payments *Payments
	store    *memStore
	provider *fakeProvider
}

func setupUnit(t *testing.T, provider domainpayments.PaymentProvider, fulfiller domainpayments.Fulfiller) *unitEnv {
	t.Helper()
	store := newMemStore()
	fp, _ := provider.(*fakeProvider)
	uc := newPayments(
		nil,
		store,
		memOrders{store},
		memCallbacks{store},
		memFulfillments{store},
		fulfiller,
		fakeRegistry{p: provider},
		memPublisher{store},
		nil,
		nil,
		nil,
		memIndex{store},
	)
	return &unitEnv{payments: uc, store: store, provider: fp}
}

func unitUserCtx(projectID, userID string) context.Context {
	return contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindEndUser,
		ProjectID:      projectID,
		UserID:         userID,
		CredentialType: domainshared.CredentialTypeToken,
	})
}

func seedPayingOrder(t *testing.T, store *memStore, orderID string, amount int64) *domainpayments.Order {
	t.Helper()
	now := time.Now()
	order := &domainpayments.Order{
		ID:                orderID,
		ProjectID:         "proj-1",
		UserID:            "u1",
		Provider:          domainpayments.ProviderStripe,
		IdempotencyKey:    "idem-" + orderID,
		ProviderSessionID: "cs_" + orderID,
		Amount:            amount,
		Currency:          "USD",
		PurposeKind:       domainpayments.PurposeTopup,
		Purpose:           json.RawMessage(`{"currency_code":"gold"}`),
		Status:            domainpayments.OrderStatusPaying,
		CreatedAt:         now,
		UpdatedAt:         now,
		ExpiresAt:         now.Add(time.Hour),
	}
	store.orders[order.ID] = cloneOrder(order)
	store.byIdem[idemKey(order.ProjectID, order.IdempotencyKey)] = order.ID
	store.index[indexKey(order.Provider, domainpayments.IndexKindPaymentSession, order.ID)] = order.ProjectID
	if order.ProviderSessionID != "" {
		store.index[indexKey(order.Provider, domainpayments.IndexKindPaymentSession, order.ProviderSessionID)] = order.ProjectID
	}
	return order
}

func paidCallbackJSON(t *testing.T, eventID, orderID string, amount int64) []byte {
	t.Helper()
	body, err := json.Marshal(domainpayments.CallbackEvent{
		Provider:          domainpayments.ProviderStripe,
		ProviderEventID:   eventID,
		ProviderSessionID: "cs_" + orderID,
		ProviderOrderID:   "pi_" + orderID,
		Type:              domainpayments.CallbackPaid,
		Amount:            amount,
		Currency:          "USD",
		OrderID:           orderID,
	})
	require.NoError(t, err)
	return body
}

func refundedCallbackJSON(t *testing.T, eventID, orderID string, amount int64) []byte {
	t.Helper()
	body, err := json.Marshal(domainpayments.CallbackEvent{
		Provider:          domainpayments.ProviderStripe,
		ProviderEventID:   eventID,
		ProviderSessionID: "cs_" + orderID,
		ProviderOrderID:   "pi_" + orderID,
		Type:              domainpayments.CallbackRefunded,
		Amount:            amount,
		Currency:          "USD",
		OrderID:           orderID,
	})
	require.NoError(t, err)
	return body
}

// seedPaidOrder 直接落一张 paid 订单（退款回调用例前置）。
func seedPaidOrder(t *testing.T, store *memStore, orderID string, amount int64) *domainpayments.Order {
	t.Helper()
	order := seedPayingOrder(t, store, orderID, amount)
	order.Status = domainpayments.OrderStatusPaid
	tnow := time.Now()
	order.PaidAt = &tnow
	store.orders[order.ID] = cloneOrder(order)
	return order
}

func signStripe(t *testing.T, secret string, body []byte) http.Header {
	t.Helper()
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	h := http.Header{}
	h.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	return h
}

func TestCreateOrder_IdempotencyKeySkipsSecondCreatePayment(t *testing.T) {
	provider := &fakeProvider{}
	env := setupUnit(t, provider, NewRecordOnlyFulfiller())
	ctx := unitUserCtx("proj-1", "u1")
	cmd := CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1999,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(1999)},
		IdempotencyKey: "idem-dup",
	}

	first, err := env.payments.CreateOrder(ctx, cmd)
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)
	require.Equal(t, domainpayments.OrderStatusPaying, first.Order.Status)
	require.Equal(t, 1, provider.createCalls)
	require.Equal(t, 1, len(env.store.orders))

	second, err := env.payments.CreateOrder(ctx, cmd)
	require.NoError(t, err)
	require.True(t, second.IdempotentReplay)
	require.Equal(t, first.Order.ID, second.Order.ID)
	require.Equal(t, 1, provider.createCalls, "幂等重放不得向渠道二次 CreatePayment")
	require.Equal(t, 1, len(env.store.orders))
}

func TestCreateOrder_RejectsNonPositiveAmount(t *testing.T) {
	provider := &fakeProvider{}
	env := setupUnit(t, provider, NewRecordOnlyFulfiller())
	ctx := unitUserCtx("proj-1", "u1")
	for _, amount := range []int64{0, -100} {
		_, err := env.payments.CreateOrder(ctx, CreateOrderCommand{
			Provider: domainpayments.ProviderStripe, Amount: amount, Currency: "USD",
			PurposeKind: domainpayments.PurposeTopup, IdempotencyKey: fmt.Sprintf("bad-%d", amount),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
	require.Equal(t, 0, provider.createCalls)
	require.Empty(t, env.store.orders)
}

func TestHandleCallback_ReplaySameEventIDNoReentry(t *testing.T) {
	fulfiller := &countingFulfiller{}
	env := setupUnit(t, &fakeProvider{}, fulfiller)
	order := seedPayingOrder(t, env.store, "ord-replay", 1999)
	body := paidCallbackJSON(t, "evt_replay", order.ID, 1999)

	require.NoError(t, env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body))
	require.Equal(t, domainpayments.OrderStatusPaid, env.store.orders[order.ID].Status)
	require.NotNil(t, env.store.orders[order.ID].PaidAt)
	require.Equal(t, 1, fulfiller.calls)
	require.Equal(t, 1, len(env.store.fulfillments))
	require.Equal(t, domainpayments.FulfillmentDone, env.store.fulfillments[order.ID].Status)
	require.Equal(t, 1, len(env.store.outbox))
	require.Equal(t, domainpayments.EventOrderPaid, env.store.outbox[0].Event)
	require.Equal(t, "accounts.u1", env.store.outbox[0].Channel)
	paidAt := *env.store.orders[order.ID].PaidAt

	require.NoError(t, env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body))
	require.Equal(t, 1, fulfiller.calls, "同 event.id 重放不得二次履约")
	require.Equal(t, 1, len(env.store.fulfillments))
	require.Equal(t, 1, len(env.store.outbox), "同 event.id 重放不得二次 Publish")
	require.Equal(t, 1, len(env.store.callbacks))
	require.Equal(t, paidAt.Unix(), env.store.orders[order.ID].PaidAt.Unix(), "paid_at 不被重放覆盖")
}

func TestHandleCallback_PaidSameTxFulfillmentAndOutbox(t *testing.T) {
	fulfiller := &countingFulfiller{}
	env := setupUnit(t, &fakeProvider{}, fulfiller)
	order := seedPayingOrder(t, env.store, "ord-paid", 500)
	body := paidCallbackJSON(t, "evt_paid", order.ID, 500)

	require.NoError(t, env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body))

	got := env.store.orders[order.ID]
	require.Equal(t, domainpayments.OrderStatusPaid, got.Status)
	require.NotNil(t, got.PaidAt)
	require.Equal(t, "pi_"+order.ID, got.ProviderOrderID)
	require.Equal(t, 1, fulfiller.calls)

	f := env.store.fulfillments[order.ID]
	require.NotNil(t, f)
	require.Equal(t, domainpayments.FulfillmentDone, f.Status)
	require.Equal(t, "order:"+order.ID, f.Ref)

	require.Equal(t, 1, len(env.store.outbox))
	require.Equal(t, domainpayments.EventOrderPaid, env.store.outbox[0].Event)
	require.Equal(t, domainpayments.EventDomain, env.store.outbox[0].Domain)
	require.Equal(t, "accounts.u1", env.store.outbox[0].Channel)
	require.Equal(t, int64(500), env.store.outbox[0].Attrs["amount"])
	require.Contains(t, env.store.callbacks, cbKey(domainpayments.ProviderStripe, "evt_paid"))
}

func TestHandleCallback_FulfillerErrorRollsBack(t *testing.T) {
	fulfiller := &countingFulfiller{err: errors.New("assets not ready (PR2)")}
	env := setupUnit(t, &fakeProvider{}, fulfiller)
	order := seedPayingOrder(t, env.store, "ord-ff", 300)
	body := paidCallbackJSON(t, "evt_ff", order.ID, 300)

	err := env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body)
	require.Error(t, err)
	require.Equal(t, 1, fulfiller.calls, "必须真正走进 Fulfill 才算同事务回滚")

	got := env.store.orders[order.ID]
	require.Equal(t, domainpayments.OrderStatusPaying, got.Status, "履约失败订单保持 paying")
	require.Nil(t, got.PaidAt)
	require.Empty(t, env.store.fulfillments, "不得留下 pending/done 履约行")
	require.Empty(t, env.store.outbox, "不得 Publish")
	require.Empty(t, env.store.callbacks, "回调行与翻转同事务，失败一并回滚")
}

// TestHandleCallback_PaidZeroAmountRefusedToSettle（R5 J1-1 / E-P1-1）：
// 渠道未提供金额（Amount==0；iOS legacy verifyReceipt 与 ASN V2 Price=0
// 均恒 0）且订单金额 >0 时 fail-closed 拒绝结算——旧逻辑 0 值直接跳过金额
// 校验，客户端自报金额即可放大充值入账。整体回滚：订单不动、不履约、
// 不发事件、不落回调行（等渠道重推或人工对账）。
func TestHandleCallback_PaidZeroAmountRefusedToSettle(t *testing.T) {
	fulfiller := &countingFulfiller{}
	env := setupUnit(t, &fakeProvider{}, fulfiller)
	order := seedPayingOrder(t, env.store, "ord-zero-amt", 1999)
	body := paidCallbackJSON(t, "evt_zero_amt", order.ID, 0)

	err := env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	got := env.store.orders[order.ID]
	require.Equal(t, domainpayments.OrderStatusPaying, got.Status, "金额缺失不得翻单")
	require.Nil(t, got.PaidAt)
	require.Equal(t, 0, fulfiller.calls)
	require.Empty(t, env.store.fulfillments)
	require.Empty(t, env.store.outbox)
	require.Empty(t, env.store.callbacks, "拒绝结算整体回滚，渠道可重推")
}

// TestHandleCallback_PaidZeroAmountFreeOrderSettles（R5 J1-1）：免费单
// （order.Amount==0）配 Amount==0 回调属于「金额一致」，不受 fail-closed
// 影响，正常翻 paid。
func TestHandleCallback_PaidZeroAmountFreeOrderSettles(t *testing.T) {
	fulfiller := &countingFulfiller{}
	env := setupUnit(t, &fakeProvider{}, fulfiller)
	order := seedPayingOrder(t, env.store, "ord-free", 0)
	body := paidCallbackJSON(t, "evt_free", order.ID, 0)

	require.NoError(t, env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body))
	require.Equal(t, domainpayments.OrderStatusPaid, env.store.orders[order.ID].Status)
	require.Equal(t, 1, fulfiller.calls)
	require.Equal(t, 1, len(env.store.outbox))
}

// TestHandleCallback_RefundedFullAmountFlipsOrder（R5 J1-2 语义收紧后的
// 正路径）：退款回调金额与订单全额一致才翻 refunded + Reverse + 事件。
func TestHandleCallback_RefundedFullAmountFlipsOrder(t *testing.T) {
	fulfiller := &countingFulfiller{}
	env := setupUnit(t, &fakeProvider{}, fulfiller)
	order := seedPaidOrder(t, env.store, "ord-refund-full", 500)
	body := refundedCallbackJSON(t, "evt_refund_full", order.ID, 500)

	require.NoError(t, env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body))
	require.Equal(t, domainpayments.OrderStatusRefunded, env.store.orders[order.ID].Status)
	require.Equal(t, 1, fulfiller.reverseCalls, "全额退款回调必须回收资产")
	require.Equal(t, 1, len(env.store.outbox))
	require.Equal(t, domainpayments.EventOrderRefunded, env.store.outbox[0].Event)
	require.Contains(t, env.store.callbacks, cbKey(domainpayments.ProviderStripe, "evt_refund_full"))
}

// TestHandleCallback_RefundedAmountMismatchKeepsOrderState（R5 J1-2 /
// E-P2-1）：部分退款（Amount < order.Amount）或渠道未提供金额（Amount==0）
// 均不得驱动状态机 / Reverse；事件行保留在回调表供对账，订单保持 paid。
func TestHandleCallback_RefundedAmountMismatchKeepsOrderState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		eventID  string
		cbAmount int64
		orderAmt int64
	}{
		{"partial", "evt_refund_partial", 100, 500},
		{"missing_amount", "evt_refund_zero", 0, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fulfiller := &countingFulfiller{}
			env := setupUnit(t, &fakeProvider{}, fulfiller)
			order := seedPaidOrder(t, env.store, "ord-refund-"+tc.name, tc.orderAmt)
			body := refundedCallbackJSON(t, tc.eventID, order.ID, tc.cbAmount)

			require.NoError(t, env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, nil, body))
			require.Equal(t, domainpayments.OrderStatusPaid, env.store.orders[order.ID].Status, "金额不一致不得翻单")
			require.Equal(t, 0, fulfiller.reverseCalls, "金额不一致不得回收资产")
			require.Empty(t, env.store.outbox)
			require.Contains(t, env.store.callbacks, cbKey(domainpayments.ProviderStripe, tc.eventID),
				"事件行保留供人工对账")
		})
	}
}

func TestHandleCallback_ForgedSignatureWritesNothing(t *testing.T) {
	adapter := stripe.New(stripe.Config{SecretKey: "sk_test_x", WebhookSecret: unitWebhookSecret})
	env := setupUnit(t, adapter, NewRecordOnlyFulfiller())
	order := seedPayingOrder(t, env.store, "ord-401", 500)

	body := []byte(`{"id":"evt_fake","type":"checkout.session.completed","data":{"object":{"id":"cs_x","client_reference_id":"ord-401","payment_status":"paid","amount_total":500,"currency":"usd"}}}`)
	h := http.Header{}
	h.Set("Stripe-Signature", "t=1700000000,v1=0000000000000000000000000000000000000000000000000000000000000000")

	err := env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, h, body)
	require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)
	require.Equal(t, domainpayments.OrderStatusPaying, env.store.orders[order.ID].Status)
	require.Empty(t, env.store.callbacks)
	require.Empty(t, env.store.fulfillments)
	require.Empty(t, env.store.outbox)
}

func TestHandleCallback_TamperedBodyWritesNothing(t *testing.T) {
	adapter := stripe.New(stripe.Config{SecretKey: "sk_test_x", WebhookSecret: unitWebhookSecret})
	env := setupUnit(t, adapter, NewRecordOnlyFulfiller())
	order := seedPayingOrder(t, env.store, "ord-tamper", 500)

	good := []byte(`{"id":"evt_tamper","type":"checkout.session.completed","data":{"object":{"id":"cs_x","client_reference_id":"ord-tamper","payment_status":"paid","amount_total":500,"currency":"usd"}}}`)
	h := signStripe(t, unitWebhookSecret, good)
	tampered := []byte(strings.Replace(string(good), `"amount_total":500`, `"amount_total":9999`, 1))

	err := env.payments.HandleCallback(context.Background(), domainpayments.ProviderStripe, h, tampered)
	require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)
	require.Equal(t, domainpayments.OrderStatusPaying, env.store.orders[order.ID].Status)
	require.Empty(t, env.store.callbacks)
	require.Empty(t, env.store.fulfillments)
	require.Empty(t, env.store.outbox)
}

func TestHandleCallback_UnknownProviderWritesNothing(t *testing.T) {
	env := setupUnit(t, &fakeProvider{}, NewRecordOnlyFulfiller())
	_ = seedPayingOrder(t, env.store, "ord-wx", 100)
	err := env.payments.HandleCallback(context.Background(), "wechat", nil, []byte(`{}`))
	require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)
	require.Empty(t, env.store.callbacks)
	require.Empty(t, env.store.outbox)
}

func TestNewPayments_WiresDatabaseAsUoWRunner(t *testing.T) {
	// 生产构造器签名不变：nil *clients.Database 仍可装配（handler 验签失败路径）。
	adapter := stripe.New(stripe.Config{SecretKey: "sk", WebhookSecret: unitWebhookSecret})
	uc := NewPayments(nil, nil, nil, nil, nil, NewRecordOnlyFulfiller(), infrapayments.NewRegistry(adapter), nil, nil, nil, nil, nil)
	require.NotNil(t, uc)
}

type fakeIOS struct {
	verify *domainpayments.VerifiedPurchase
	err    error
}

func (f *fakeIOS) Name() string { return domainpayments.ProviderIOSIAP }
func (f *fakeIOS) CreatePayment(context.Context, domainpayments.CreatePaymentInput) (*domainpayments.PaymentSession, error) {
	return nil, domainpayments.ErrUnsupported
}
func (f *fakeIOS) VerifyCallback(context.Context, http.Header, []byte) (*domainpayments.CallbackEvent, error) {
	return nil, domainpayments.ErrSignatureInvalid
}
func (f *fakeIOS) Refund(context.Context, domainpayments.RefundInput) (*domainpayments.RefundResult, error) {
	return nil, domainpayments.ErrUnsupported
}
func (f *fakeIOS) VerifyReceipt(context.Context, domainpayments.VerifyReceiptInput) (*domainpayments.VerifiedPurchase, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.verify, nil
}

func seedIOSOrder(t *testing.T, store *memStore, orderID, userID string, status domainpayments.OrderStatus, txn string) *domainpayments.Order {
	t.Helper()
	now := time.Now()
	order := &domainpayments.Order{
		ID:              orderID,
		ProjectID:       "proj-1",
		UserID:          userID,
		Provider:        domainpayments.ProviderIOSIAP,
		IdempotencyKey:  "idem-" + orderID,
		ProviderOrderID: txn,
		Amount:          199,
		Currency:        "USD",
		PurposeKind:     domainpayments.PurposeTopup,
		Purpose:         json.RawMessage(`{"currency_code":"gold"}`),
		Status:          status,
		CreatedAt:       now,
		UpdatedAt:       now,
		ExpiresAt:       now.Add(time.Hour),
	}
	if status == domainpayments.OrderStatusPaid {
		tnow := now
		order.PaidAt = &tnow
	}
	store.orders[order.ID] = cloneOrder(order)
	store.byIdem[idemKey(order.ProjectID, order.IdempotencyKey)] = order.ID
	store.index[indexKey(order.Provider, domainpayments.IndexKindPaymentSession, order.ID)] = order.ProjectID
	if order.ProviderOrderID != "" {
		store.index[indexKey(order.Provider, domainpayments.IndexKindIOSTransaction, order.ProviderOrderID)] = order.ProjectID
	}
	return order
}

func TestCreateOrder_IOSKeepsCreated(t *testing.T) {
	env := setupUnit(t, &fakeIOS{}, NewRecordOnlyFulfiller())
	ctx := unitUserCtx("proj-1", "u1")
	got, err := env.payments.CreateOrder(ctx, CreateOrderCommand{
		Provider: domainpayments.ProviderIOSIAP, Amount: 199, Currency: "USD",
		PurposeKind: domainpayments.PurposeTopup, Purpose: map[string]any{"currency_code": "gold", "amount": int64(199)},
		IdempotencyKey: "ios-1",
	})
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusCreated, got.Order.Status)
	require.Empty(t, got.PaymentURL)
}

func TestVerifyReceipt_PaysCreatedOrder(t *testing.T) {
	ios := &fakeIOS{verify: &domainpayments.VerifiedPurchase{
		TransactionID: "txn-1", ProductID: "gold", Amount: 199, Currency: "USD", PaidAt: time.Now(),
	}}
	fulfiller := &countingFulfiller{}
	env := setupUnit(t, ios, fulfiller)
	order := seedIOSOrder(t, env.store, "ord-ios", "u1", domainpayments.OrderStatusCreated, "")
	ctx := unitUserCtx("proj-1", "u1")

	got, err := env.payments.VerifyReceipt(ctx, order.ID, []byte("receipt-blob"))
	require.NoError(t, err)
	require.False(t, got.IdempotentReplay)
	require.Equal(t, "txn-1", got.TransactionID)
	require.Equal(t, domainpayments.OrderStatusPaid, env.store.orders[order.ID].Status)
	require.Equal(t, 1, fulfiller.calls)
	require.Equal(t, 1, len(env.store.outbox))
	require.Equal(t, domainpayments.EventOrderPaid, env.store.outbox[0].Event)
}

func TestVerifyReceipt_TransactionIDReplaySameUser(t *testing.T) {
	ios := &fakeIOS{verify: &domainpayments.VerifiedPurchase{TransactionID: "txn-1", Amount: 199, Currency: "USD"}}
	env := setupUnit(t, ios, NewRecordOnlyFulfiller())
	order := seedIOSOrder(t, env.store, "ord-ios", "u1", domainpayments.OrderStatusPaid, "txn-1")
	ctx := unitUserCtx("proj-1", "u1")

	got, err := env.payments.VerifyReceipt(ctx, "other-order", []byte("receipt-blob"))
	require.NoError(t, err)
	require.True(t, got.IdempotentReplay)
	require.Equal(t, order.ID, got.Order.ID)
	require.Equal(t, 1, len(env.store.orders), "不得新建或改绑其他订单")
}

func TestVerifyReceipt_CrossUserRejected(t *testing.T) {
	ios := &fakeIOS{verify: &domainpayments.VerifiedPurchase{TransactionID: "txn-1", Amount: 199, Currency: "USD"}}
	env := setupUnit(t, ios, NewRecordOnlyFulfiller())
	seedIOSOrder(t, env.store, "ord-a", "u1", domainpayments.OrderStatusPaid, "txn-1")
	seedIOSOrder(t, env.store, "ord-b", "u2", domainpayments.OrderStatusCreated, "")
	ctx := unitUserCtx("proj-1", "u2")

	_, err := env.payments.VerifyReceipt(ctx, "ord-b", []byte("receipt-blob"))
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Equal(t, domainpayments.OrderStatusCreated, env.store.orders["ord-b"].Status)
	require.Empty(t, env.store.fulfillments)
}

// TestVerifyReceipt_LegacyZeroAmountRefused（R5 J1-1 / E-P1-1）：iOS legacy
// verifyReceipt 归一化 Amount 恒 0（iosiap.go 不填 Amount），订单金额 >0 时
// applyPaid fail-closed 拒绝结算；订单保持 created，等价目映射根治方案。
func TestVerifyReceipt_LegacyZeroAmountRefused(t *testing.T) {
	ios := &fakeIOS{verify: &domainpayments.VerifiedPurchase{TransactionID: "txn-legacy"}}
	env := setupUnit(t, ios, NewRecordOnlyFulfiller())
	order := seedIOSOrder(t, env.store, "ord-ios-legacy", "u1", domainpayments.OrderStatusCreated, "")
	ctx := unitUserCtx("proj-1", "u1")

	_, err := env.payments.VerifyReceipt(ctx, order.ID, []byte("receipt-blob"))
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, domainpayments.OrderStatusCreated, env.store.orders[order.ID].Status)
	require.Empty(t, env.store.fulfillments)
	require.Empty(t, env.store.outbox)
}

func TestCreateOrder_RejectsPurposeSubscription(t *testing.T) {
	env := setupUnit(t, &fakeProvider{}, NewRecordOnlyFulfiller())
	_, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1000,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeSubscription,
		Purpose:        map[string]any{"subscription_id": "sub_1"},
		IdempotencyKey: "sub:sub_1:activate",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Empty(t, env.store.orders)
	require.Equal(t, 0, env.provider.createCalls)
}

func TestInsertCreatedOrder_AllowsPurposeSubscriptionAndIdempotency(t *testing.T) {
	store := newMemStore()
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	order, err := NewCreatedOrder(CreatedOrderSpec{
		ProjectID:      "proj-1",
		UserID:         "u1",
		Provider:       domainpayments.ProviderStripe,
		Amount:         1000,
		Currency:       "usd",
		PurposeKind:    domainpayments.PurposeSubscription,
		Purpose:        json.RawMessage(`{"subscription_id":"sub_1","cycle":"activate"}`),
		IdempotencyKey: "sub:sub_1:activate",
		Now:            now,
	})
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusCreated, order.Status)
	require.Equal(t, "USD", order.Currency)
	require.Equal(t, now.Add(defaultOrderTTL), order.ExpiresAt)
	require.Equal(t, domainpayments.PurposeSubscription, order.PurposeKind)

	existing, inserted, err := InsertCreatedOrder(context.Background(), memOrders{store}, memIndex{store}, order)
	require.NoError(t, err)
	require.True(t, inserted)
	require.Nil(t, existing)
	require.Equal(t, order.ProjectID, store.index[indexKey(order.Provider, domainpayments.IndexKindPaymentSession, order.ID)])

	replay := *order
	replay.ID = "other-id"
	existing, inserted, err = InsertCreatedOrder(context.Background(), memOrders{store}, memIndex{store}, &replay)
	require.NoError(t, err)
	require.False(t, inserted)
	require.Equal(t, order.ID, existing.ID)
	require.Equal(t, 1, len(store.orders))
}

func TestInsertCreatedOrder_IsSoleOrdersInsertCallSite(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	re := regexp.MustCompile(`\borders\.Insert\(`)
	var hits []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "genproto", "node_modules", "console", "docs", "bin":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// #nosec G304 -- 路径来自仓库内 WalkDir 白名单枚举，非外部输入。
		body, err := os.ReadFile(path) // #nosec G122 -- 仓库内测试扫描，无符号链接攻击面
		if err != nil {
			return err
		}
		if !re.Match(body) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		hits = append(hits, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"internal/app/payments/orders.go"}, hits, "全仓 orders.Insert 只能出现在 InsertCreatedOrder")
	src, err := os.ReadFile(filepath.Join(root, "internal", "app", "payments", "orders.go"))
	require.NoError(t, err)
	require.Equal(t, 1, len(re.FindAll(src, -1)))
	require.Contains(t, string(src), "func InsertCreatedOrder(")
}

// TestCreateOrder_RejectsTopupAmountMismatch（A2）：purpose.amount 必须等于
// Order.Amount，1 分钱买天量资产在建单即被拒（InvalidArgument，不触渠道）。
func TestCreateOrder_RejectsTopupAmountMismatch(t *testing.T) {
	env := setupUnit(t, &fakeProvider{}, NewRecordOnlyFulfiller())
	_, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": 1_000_000_000_000},
		IdempotencyKey: "idem-topup-mismatch",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 0, env.provider.createCalls)
	require.Empty(t, env.store.orders)
}

// TestCreateOrder_AcceptsTopupAmountEqual（A2）：amount == purpose.amount 放行。
func TestCreateOrder_AcceptsTopupAmountEqual(t *testing.T) {
	env := setupUnit(t, &fakeProvider{}, NewRecordOnlyFulfiller())
	res, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         100,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": 100},
		IdempotencyKey: "idem-topup-equal",
	})
	require.NoError(t, err)
	require.Equal(t, int64(100), res.Order.Amount)
	require.Equal(t, domainpayments.OrderStatusPaying, res.Order.Status)
}

// TestCreateOrder_RejectsTopupNonIntegerAmount（R5 J1-3 / E-P2-4）：
// purpose.amount 非整数一律 InvalidArgument。structpb 数值经 AsMap() 变
// float64，旧逻辑 int64 截断放行 10.5→10 绕过 pa != amount 校验；paid 履约
// 侧 Amount 反序列化失败会永久回滚，用户已付款订单最终 closed。float32 /
// json.Number 分支同步收紧。
func TestCreateOrder_RejectsTopupNonIntegerAmount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		amount any
	}{
		{"float64", 10.5},
		{"float32", float32(10.5)},
		{"json_number", json.Number("10.5")},
		{"float64_fraction_like_int", 100.25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &fakeProvider{}
			env := setupUnit(t, provider, NewRecordOnlyFulfiller())
			_, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
				Provider:       domainpayments.ProviderStripe,
				Amount:         10,
				Currency:       "USD",
				PurposeKind:    domainpayments.PurposeTopup,
				Purpose:        map[string]any{"currency_code": "gold", "amount": tc.amount},
				IdempotencyKey: "idem-topup-frac-" + tc.name,
			})
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			require.Equal(t, 0, provider.createCalls, "非整数金额不得触渠道")
			require.Empty(t, env.store.orders)
		})
	}
}

// TestCreateOrder_AcceptsTopupIntegralFloatAmount（R5 J1-3 边界）：
// float64 整数值（structpb AsMap 的真实形态）仍放行，不误伤整数充值。
func TestCreateOrder_AcceptsTopupIntegralFloatAmount(t *testing.T) {
	provider := &fakeProvider{}
	env := setupUnit(t, provider, NewRecordOnlyFulfiller())
	res, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         100,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": float64(100)},
		IdempotencyKey: "idem-topup-int-float",
	})
	require.NoError(t, err)
	require.Equal(t, int64(100), res.Order.Amount)
	require.Equal(t, 1, provider.createCalls)
}

// TestCreateOrder_RejectsPurposeItemPurchase（A2）：Client 面拒绝 item_purchase
// （无服务端定价目录，构建方用 Server Grant 发货）。
func TestCreateOrder_RejectsPurposeItemPurchase(t *testing.T) {
	env := setupUnit(t, &fakeProvider{}, NewRecordOnlyFulfiller())
	_, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         100,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeItemPurchase,
		Purpose:        map[string]any{"asset_code": "sword", "quantity": 1},
		IdempotencyKey: "idem-item-reject",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 0, env.provider.createCalls)
	require.Empty(t, env.store.orders)
}

// TestCreateOrder_CreatePaymentAlwaysHasURLs（A3）：即使请求未带 success/cancel
// URL，传给渠道的 CreatePaymentInput 也必须非空（public_url→兜底，不留空）。
func TestCreateOrder_CreatePaymentAlwaysHasURLs(t *testing.T) {
	env := setupUnit(t, &fakeProvider{}, NewRecordOnlyFulfiller())
	_, err := env.payments.CreateOrder(unitUserCtx("proj-1", "u1"), CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         100,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": 100},
		IdempotencyKey: "idem-url-nonempty",
	})
	require.NoError(t, err)
	require.Equal(t, 1, env.provider.createCalls)
	require.NotEmpty(t, env.provider.lastInput.SuccessURL)
	require.NotEmpty(t, env.provider.lastInput.CancelURL)
}

// TestRefund_RejectsPartialRefund（A5）：一期仅支持全额退款，amount != 0 且
// != order.Amount 直接 InvalidArgument，不触渠道、订单保持 paid。
func TestRefund_RejectsPartialRefund(t *testing.T) {
	provider := &fakeProvider{refundRes: &domainpayments.RefundResult{Succeeded: true}}
	env := setupUnit(t, provider, NewRecordOnlyFulfiller())
	order := seedPayingOrder(t, env.store, "ord-partial", 100)
	order.Status = domainpayments.OrderStatusPaid
	env.store.orders[order.ID] = order

	admin := contexts.WithPrincipal(context.Background(), &domainshared.Principal{
		ActorKind:      domainshared.ActorKindAdmin,
		ProjectID:      "proj-1",
		CredentialType: domainshared.CredentialTypeSession,
	})
	_, err := env.payments.Refund(admin, order.ID, 1)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, domainpayments.OrderStatusPaid, env.store.orders[order.ID].Status)
}
