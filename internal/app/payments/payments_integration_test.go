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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"strconv"

	"github.com/stretchr/testify/require"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainshared "github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	infrapayments "github.com/torchwoodcloud/torchwood/internal/infra/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/stripe"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	testWebhookSecret = "whsec_integration" // #nosec G101 -- 测试固定值
	testSecretKey     = "sk_test_integration"
)

// failingFulfiller 注入履约失败，验证「任一失败整体回滚」。
type failingFulfiller struct{ err error }

func (f failingFulfiller) Fulfill(_ context.Context, _ *domainpayments.Order) (string, error) {
	return "", f.err
}

func (f failingFulfiller) Reverse(_ context.Context, _ *domainpayments.Order) error { return nil }

// testEnv 组装端到端用例环境（真实 stripe adapter + httptest 渠道服务 +
// 真实 outbox publisher + 真实 bun repo）。
type testEnv struct {
	payments *Payments
	db       *clients.Database
	stripe   *stripe.Adapter
}

func setupEnv(t *testing.T, fulfiller domainpayments.Fulfiller, refundSucceeded bool) *testEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)

	checkoutSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w,
			`{"id":"cs_%d","url":"https://checkout.stripe.com/c/pay/cs_%d","payment_intent":"pi_%d"}`,
			time.Now().UnixNano(), time.Now().UnixNano(), time.Now().UnixNano())
	}))
	t.Cleanup(checkoutSrv.Close)

	refundStatus := "succeeded"
	if !refundSucceeded {
		refundStatus = "pending"
	}
	refundSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"re_%d","status":%q}`, time.Now().UnixNano(), refundStatus)
	}))
	t.Cleanup(refundSrv.Close)

	// 同一进程内按 path 分流 checkout / refund。
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/refunds" {
			refundSrv.Config.Handler.ServeHTTP(w, r)
			return
		}
		checkoutSrv.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(apiSrv.Close)

	adapter := stripe.New(stripe.Config{
		SecretKey:     testSecretKey,
		WebhookSecret: testWebhookSecret,
		APIBaseURL:    apiSrv.URL,
	})
	uc := NewPayments(
		nil,
		db,
		bunrepo.NewPaymentOrderRepository(db),
		bunrepo.NewPaymentCallbackEventRepository(db),
		bunrepo.NewPaymentFulfillmentRepository(db),
		fulfiller,
		infrapayments.NewRegistry(adapter),
		infraevents.NewEventOutbox(db),
		nil,
		nil,
		bunrepo.NewProjectRepository(db),
		bunrepo.NewProviderIndexRepository(db),
	)
	return &testEnv{payments: uc, db: db, stripe: adapter}
}

func endUserCtx(ctx context.Context, projectID, userID string) context.Context {
	return contexts.WithPrincipal(ctx, &domainshared.Principal{
		ActorKind:      domainshared.ActorKindEndUser,
		ProjectID:      projectID,
		UserID:         userID,
		CredentialType: domainshared.CredentialTypeToken,
	})
}

func adminCtx(ctx context.Context, projectID string) context.Context {
	return contexts.WithPrincipal(ctx, &domainshared.Principal{
		ActorKind:      domainshared.ActorKindAdmin,
		ProjectID:      projectID,
		CredentialType: domainshared.CredentialTypeSession,
	})
}

// signStripeBody 构造合法 Stripe webhook 头 + body。
func signStripeBody(t *testing.T, body []byte) (http.Header, []byte) {
	t.Helper()
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(testWebhookSecret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	h := http.Header{}
	h.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	return h, body
}

func paidEventBody(t *testing.T, eventID, orderID, sessionID string, amount int64) []byte {
	t.Helper()
	obj := map[string]any{
		"id":                  sessionID,
		"payment_intent":      "pi_" + sessionID,
		"client_reference_id": orderID,
		"payment_status":      "paid",
		"amount_total":        amount,
		"currency":            "USD",
		"metadata":            map[string]any{"order_id": orderID, "project_id": "p"},
	}
	body, err := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    "checkout.session.completed",
		"created": time.Now().Unix(),
		"data":    map[string]any{"object": obj},
	})
	require.NoError(t, err)
	return body
}

// createPaidOrder 建单并推送 paid 回调，返回订单 id。
func createPaidOrder(t *testing.T, env *testEnv, ctx context.Context, idemKey, userID string, amount int64) string {
	t.Helper()
	result, err := env.payments.CreateOrder(ctx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         amount,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": amount},
		IdempotencyKey: idemKey,
	})
	require.NoError(t, err)
	orderID := result.Order.ID
	h, body := signStripeBody(t, paidEventBody(t, "evt_"+idemKey, orderID, orderID, amount))
	require.NoError(t, env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body))
	return orderID
}

// 查询辅助。
func countRows(t *testing.T, env *testEnv, projectID, table, where string, args ...any) int {
	t.Helper()
	tbl := table
	if table != "document_events_outbox" && projectID != "" {
		tbl = testutil.CatalogQuoted(projectID) + "." + table
	}
	n, err := env.db.NewSelect().TableExpr(tbl).Where(where, args...).Count(context.Background())
	require.NoError(t, err)
	return int(n)
}

func TestPayments_CreateOrderIdempotencyReturnsOriginal(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")

	first, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1999,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(1999)},
		IdempotencyKey: "idem-1",
	})
	require.NoError(t, err)
	require.False(t, first.IdempotentReplay)
	require.Equal(t, domainpayments.OrderStatusPaying, first.Order.Status)
	require.NotEmpty(t, first.PaymentURL)

	// 同幂等键重放：返回原单（不新建行）。
	second, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1999,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(1999)},
		IdempotencyKey: "idem-1",
	})
	require.NoError(t, err)
	require.True(t, second.IdempotentReplay)
	require.Equal(t, first.Order.ID, second.Order.ID)
	require.Equal(t, 1, countRows(t, env, projectID, "payment_orders", "idempotency_key = ?", "idem-1"))

	// 金额非法：负数 / 零拒绝。
	for _, amount := range []int64{0, -100} {
		_, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
			Provider: domainpayments.ProviderStripe, Amount: amount, Currency: "USD",
			PurposeKind: domainpayments.PurposeTopup, IdempotencyKey: fmt.Sprintf("bad-%d", amount),
		})
		require.Error(t, err)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}
}

func TestPayments_CallbackPaidSameTxFulfillmentAndOutbox(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")

	result, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1999,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(1999)},
		IdempotencyKey: "idem-paid",
	})
	require.NoError(t, err)
	orderID := result.Order.ID

	h, body := signStripeBody(t, paidEventBody(t, "evt_paid_1", orderID, orderID, 1999))
	require.NoError(t, env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body))

	// 订单翻 paid + paid_at 落值。
	order, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusPaid, order.Status)
	require.NotNil(t, order.PaidAt)
	require.NotEmpty(t, order.ProviderOrderID)

	// 履约行：done + 幂等 ref。
	require.Equal(t, 1, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
	var fStatus string
	require.NoError(t, env.db.NewSelect().TableExpr(testutil.CatalogQuoted(projectID)+".payment_fulfillments").
		Column("status").Where("order_id = ?", orderID).Scan(ctx, &fStatus))
	require.Equal(t, "done", fStatus)
	var fRef string
	require.NoError(t, env.db.NewSelect().TableExpr(testutil.CatalogQuoted(projectID)+".payment_fulfillments").
		Column("ref").Where("order_id = ?", orderID).Scan(ctx, &fRef))
	require.Equal(t, "order:"+orderID, fRef)

	// outbox 事件：channel 落 accounts.{userId}，payload 含 payments.orders.paid。
	require.Equal(t, 1, countRows(t, env, projectID, "document_events_outbox",
		"channel = ? AND payload->>'event' = ?", "accounts.u1", domainpayments.EventOrderPaid))

	// 同 event.id 重放：幂等成功（nil），状态机不重入、无新行。
	require.NoError(t, env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body))
	require.Equal(t, 1, countRows(t, env, projectID, "payment_callback_events", "provider_event_id = ?", "evt_paid_1"))
	require.Equal(t, 1, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
	require.Equal(t, 1, countRows(t, env, projectID, "document_events_outbox",
		"channel = ? AND payload->>'event' = ?", "accounts.u1", domainpayments.EventOrderPaid))
	order2, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, order.PaidAt.Unix(), order2.PaidAt.Unix(), "paid_at 不被重放覆盖")
}

func TestPayments_CallbackVerifyFailNoRowsWritten(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")

	result, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         500,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(500)},
		IdempotencyKey: "idem-401",
	})
	require.NoError(t, err)
	orderID := result.Order.ID

	// 假签名：HMAC 用错 secret。
	body := paidEventBody(t, "evt_fake", orderID, orderID, 500)
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte("whsec_attacker"))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	h := http.Header{}
	h.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	err = env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body)
	require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)

	// 篡改 body：验签必失配（篡改金额字段）。
	h2, goodBody := signStripeBody(t, paidEventBody(t, "evt_tamper", orderID, orderID, 500))
	tampered := strings.Replace(string(goodBody), `"amount_total":500`, `"amount_total":9999`, 1)
	err = env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h2, []byte(tampered))
	require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)

	// 验签失败零落库（订单未动、无回调行、无 outbox 行）。
	require.Equal(t, 0, countRows(t, env, projectID, "payment_callback_events", "provider = ?", domainpayments.ProviderStripe))
	order, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusPaying, order.Status)
	require.Nil(t, order.PaidAt)
	require.Equal(t, 0, countRows(t, env, projectID, "document_events_outbox", "channel = ?", "accounts.u1"))
}

func TestPayments_CallbackAmountMismatchRollsBackEverything(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")

	result, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         1999,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(1999)},
		IdempotencyKey: "idem-mismatch",
	})
	require.NoError(t, err)
	orderID := result.Order.ID

	// 金额不一致：整体回滚（回调登记行一并回滚，等待渠道重推）。
	h, body := signStripeBody(t, paidEventBody(t, "evt_mismatch", orderID, orderID, 9999))
	err = env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body)
	require.Error(t, err)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Equal(t, 0, countRows(t, env, projectID, "payment_callback_events", "provider_event_id = ?", "evt_mismatch"))
	require.Equal(t, 0, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
	require.Equal(t, 0, countRows(t, env, projectID, "document_events_outbox", "channel = ?", "accounts.u1"))
	order, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusPaying, order.Status)
}

func TestPayments_CallbackFulfillFailureRollsBack(t *testing.T) {
	env := setupEnv(t, failingFulfiller{err: errors.New("assets not ready (PR2)")}, true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")

	result, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         300,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(300)},
		IdempotencyKey: "idem-ff",
	})
	require.NoError(t, err)
	orderID := result.Order.ID

	h, body := signStripeBody(t, paidEventBody(t, "evt_ff", orderID, orderID, 300))
	err = env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body)
	require.Error(t, err)

	// 「钱到了货没发」被同事务杜绝：订单不翻、履约不留半行、事件不发。
	order, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusPaying, order.Status)
	require.Equal(t, 0, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
	require.Equal(t, 0, countRows(t, env, projectID, "document_events_outbox", "channel = ?", "accounts.u1"))
	require.Equal(t, 0, countRows(t, env, projectID, "payment_callback_events", "provider_event_id = ?", "evt_ff"))
}

func TestPayments_CloseExpiredOrders(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")

	result, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderStripe,
		Amount:         100,
		Currency:       "USD",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"currency_code": "gold", "amount": int64(100)},
		IdempotencyKey: "idem-expire",
		ExpiresIn:      time.Minute,
	})
	require.NoError(t, err)
	orderID := result.Order.ID

	// 未到期不关单。
	closed, err := env.payments.CloseExpiredOrders(ctx, time.Now())
	require.NoError(t, err)
	require.Equal(t, int64(0), closed)

	// 超时后关单：paying → closed；paid 订单不受影响。
	closed, err = env.payments.CloseExpiredOrders(ctx, time.Now().Add(2*time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(1), closed)
	order, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusClosed, order.Status)

	// 迟到 paid 回调：不重入 closed（PR1 人工兜底，D12）。
	h, body := signStripeBody(t, paidEventBody(t, "evt_late", orderID, orderID, 100))
	require.NoError(t, env.payments.HandleCallback(ctx, domainpayments.ProviderStripe, h, body))
	order, err = env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusClosed, order.Status)
	require.Equal(t, 0, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
}

func TestPayments_RefundFlow(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")
	aCtx := adminCtx(ctx, projectID)

	orderID := createPaidOrder(t, env, uCtx, "idem-refund", "u1", 1999)

	// 端用户不可退款（红线：资金操作仅 Server 面）。
	_, err := env.payments.Refund(uCtx, orderID, 0)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// admin 退款（渠道同步成功）：paid → refunded + 事件。
	refunded, err := env.payments.Refund(aCtx, orderID, 0)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusRefunded, refunded.Status)
	require.Equal(t, 1, countRows(t, env, projectID, "document_events_outbox",
		"channel = ? AND payload->>'event' = ?", "accounts.u1", domainpayments.EventOrderRefunded))

	// 重复退款幂等：返回现单，不重复调渠道。
	refunded2, err := env.payments.Refund(aCtx, orderID, 0)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusRefunded, refunded2.Status)
	require.Equal(t, 1, countRows(t, env, projectID, "document_events_outbox",
		"channel = ? AND payload->>'event' = ?", "accounts.u1", domainpayments.EventOrderRefunded))
}

func TestPayments_ManualFulfill(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "u1")
	aCtx := adminCtx(ctx, projectID)

	orderID := createPaidOrder(t, env, uCtx, "idem-mf", "u1", 500)

	// 端用户不可人工履约。
	_, _, err := env.payments.ManualFulfill(uCtx, orderID, "why")
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	order, fulfillment, err := env.payments.ManualFulfill(aCtx, orderID, "customer support")
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusPaid, order.Status, "人工履约不改订单状态")
	require.NotNil(t, fulfillment)
	require.Equal(t, domainpayments.FulfillmentDone, fulfillment.Status)
	require.Equal(t, "order:"+orderID, fulfillment.Ref)

	// 幂等：重复标记返回 done。
	_, fulfillment2, err := env.payments.ManualFulfill(aCtx, orderID, "again")
	require.NoError(t, err)
	require.Equal(t, domainpayments.FulfillmentDone, fulfillment2.Status)
	require.Equal(t, 1, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
}

// TestPayments_CloseExpiredOrders_GlobalBudgetAcrossProjects 锁 K22：
// closeExpiredBatch 是每 tick 的**全局**预算（不是 per-project 再传同一 limit）。
// 两项目共 610 张过期单：第一轮全局只关 500；轮转游标推进后第二轮清剩余 110。
func TestPayments_CloseExpiredOrders_GlobalBudgetAcrossProjects(t *testing.T) {
	env := setupEnv(t, NewRecordOnlyFulfiller(), true)
	ctx := context.Background()
	pA, _, cA := testutil.CreateTestProject(ctx, env.db)
	defer cA()
	pB, _, cB := testutil.CreateTestProject(ctx, env.db)
	defer cB()

	repo := bunrepo.NewPaymentOrderRepository(env.db)
	seed := func(projectID string, count int, prefix string) {
		t.Helper()
		for i := 0; i < count; i++ {
			now := time.Now()
			o := &domainpayments.Order{
				ID:             prefix + strconv.Itoa(i),
				ProjectID:      projectID,
				UserID:         "u1",
				Provider:       domainpayments.ProviderStripe,
				IdempotencyKey: "idem-" + prefix + strconv.Itoa(i),
				Amount:         100,
				Currency:       "USD",
				PurposeKind:    domainpayments.PurposeTopup,
				Purpose:        []byte(`{}`),
				Status:         domainpayments.OrderStatusCreated,
				CreatedAt:      now,
				UpdatedAt:      now,
				ExpiresAt:      now.Add(-time.Hour),
			}
			_, inserted, err := repo.Insert(ctx, o)
			require.NoError(t, err)
			require.True(t, inserted, "%s/%s%d", projectID, prefix, i)
		}
	}
	seed(pA, 600, "budgetA")
	seed(pB, 10, "budgetB")

	now := time.Now()
	r1, err := env.payments.CloseExpiredOrders(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(closeExpiredBatch), r1, "一轮全局预算恒为 closeExpiredBatch")

	remaining := 610 - closeExpiredBatch
	r2, err := env.payments.CloseExpiredOrders(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(remaining), r2, "轮转后第二轮清剩余")

	r3, err := env.payments.CloseExpiredOrders(ctx, now)
	require.NoError(t, err)
	require.Equal(t, int64(0), r3)
}
