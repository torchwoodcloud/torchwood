package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

const testWebhookSecret = "whsec_test_0123456789abcdef"

func newTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	a := New(Config{SecretKey: "sk_test_x", WebhookSecret: testWebhookSecret})
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	return a
}

// signedBody 构造合法 Stripe webhook：头 t=unix,v1=hmac(secret, "t.body")。
func signedBody(t *testing.T, secret string, body []byte, ts int64) (http.Header, []byte) {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	h := http.Header{}
	h.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", ts, sig))
	return h, body
}

func checkoutCompletedBody(eventID, sessionID, paymentIntent, orderID string, amount int64, currency, paymentStatus string) []byte {
	obj := map[string]any{
		"id":                  sessionID,
		"payment_intent":      paymentIntent,
		"client_reference_id": orderID,
		"payment_status":      paymentStatus,
		"amount_total":        amount,
		"currency":            currency,
		"metadata":            map[string]any{"order_id": orderID},
	}
	body, _ := json.Marshal(map[string]any{
		"id":      eventID,
		"type":    "checkout.session.completed",
		"created": 1700000000,
		"data":    map[string]any{"object": obj},
	})
	return body
}

func TestVerifyCallback_ValidSignaturePaid(t *testing.T) {
	a := newTestAdapter(t)
	h, body := signedBody(t, testWebhookSecret,
		checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid"), 1700000000)
	ev, err := a.VerifyCallback(context.Background(), h, body)
	require.NoError(t, err)
	require.Equal(t, payments.ProviderStripe, ev.Provider)
	require.Equal(t, "evt_1", ev.ProviderEventID)
	require.Equal(t, payments.CallbackPaid, ev.Type)
	require.Equal(t, "cs_1", ev.ProviderSessionID)
	require.Equal(t, "pi_1", ev.ProviderOrderID)
	require.Equal(t, "order_1", ev.OrderID)
	require.Equal(t, int64(1999), ev.Amount)
	require.Equal(t, "USD", ev.Currency)
}

func TestVerifyCallback_TamperedBody(t *testing.T) {
	a := newTestAdapter(t)
	h, body := signedBody(t, testWebhookSecret,
		checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid"), 1700000000)
	// 篡改金额后签名失配。
	tampered := strings.Replace(string(body), "1999", "9999", 1)
	ev, err := a.VerifyCallback(context.Background(), h, []byte(tampered))
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
	require.Nil(t, ev)
}

func TestVerifyCallback_ForgedSignature(t *testing.T) {
	a := newTestAdapter(t)
	body := checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid")
	h := http.Header{}
	h.Set("Stripe-Signature", "t=1700000000,v1=deadbeef")
	_, err := a.VerifyCallback(context.Background(), h, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_WrongSecret(t *testing.T) {
	a := newTestAdapter(t)
	h, body := signedBody(t, "whsec_other",
		checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid"), 1700000000)
	_, err := a.VerifyCallback(context.Background(), h, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_MissingOrMalformedHeader(t *testing.T) {
	a := newTestAdapter(t)
	body := checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid")
	_, err := a.VerifyCallback(context.Background(), http.Header{}, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)

	h := http.Header{}
	h.Set("Stripe-Signature", "garbage")
	_, err = a.VerifyCallback(context.Background(), h, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)

	h = http.Header{}
	h.Set("Stripe-Signature", "t=abc,v1=zz")
	_, err = a.VerifyCallback(context.Background(), h, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_StaleTimestamp(t *testing.T) {
	a := newTestAdapter(t)
	// 10 分钟前的时间戳，超过默认 5min 容忍窗（重放防护）。
	h, body := signedBody(t, testWebhookSecret,
		checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid"), 1700000000-600)
	_, err := a.VerifyCallback(context.Background(), h, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_NotConfigured(t *testing.T) {
	a := New(Config{}) // 无 secret：fail-closed。
	body := checkoutCompletedBody("evt_1", "cs_1", "pi_1", "order_1", 1999, "usd", "paid")
	h, body2 := signedBody(t, "whsec_any", body, 1700000000)
	_, err := a.VerifyCallback(context.Background(), h, body2)
	require.Error(t, err)
	require.True(t, payments.ErrNotConfigured(err))
}

func TestNormalize_EventTypes(t *testing.T) {
	a := newTestAdapter(t)
	cases := []struct {
		name       string
		body       []byte
		wantType   string
		wantAmount int64
	}{
		{
			name: "async_payment_succeeded → paid",
			body: func() []byte {
				obj := map[string]any{"id": "cs_2", "payment_intent": "pi_2", "payment_status": "paid", "amount_total": 500, "currency": "eur", "metadata": map[string]any{"order_id": "o2"}}
				b, _ := json.Marshal(map[string]any{"id": "evt_2", "type": "checkout.session.async_payment_succeeded", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackPaid,
			wantAmount: 500,
		},
		{
			name: "async_payment_failed → failed",
			body: func() []byte {
				obj := map[string]any{"id": "cs_3", "payment_intent": "pi_3", "payment_status": "unpaid", "amount_total": 700, "currency": "usd", "metadata": map[string]any{"order_id": "o3"}}
				b, _ := json.Marshal(map[string]any{"id": "evt_3", "type": "checkout.session.async_payment_failed", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackFailed,
			wantAmount: 700,
		},
		{
			name: "checkout.session.expired → failed",
			body: func() []byte {
				obj := map[string]any{"id": "cs_4", "payment_status": "unpaid", "amount_total": 100, "currency": "usd", "client_reference_id": "o4"}
				b, _ := json.Marshal(map[string]any{"id": "evt_4", "type": "checkout.session.expired", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackFailed,
			wantAmount: 100,
		},
		{
			name: "charge.refunded → refunded",
			body: func() []byte {
				obj := map[string]any{"payment_intent": "pi_5", "amount_refunded": 1999, "currency": "usd"}
				b, _ := json.Marshal(map[string]any{"id": "evt_5", "type": "charge.refunded", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackRefunded,
			wantAmount: 1999,
		},
		{
			name: "payment_intent.payment_failed → failed",
			body: func() []byte {
				obj := map[string]any{"id": "pi_6", "amount": 300, "currency": "usd", "metadata": map[string]any{"order_id": "o6"}}
				b, _ := json.Marshal(map[string]any{"id": "evt_6", "type": "payment_intent.payment_failed", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackFailed,
			wantAmount: 300,
		},
		{
			name: "invoice.paid subscription_create → activated",
			body: func() []byte {
				obj := map[string]any{"id": "in_1", "subscription": "sub_1", "billing_reason": "subscription_create", "amount_paid": 1999, "currency": "usd", "period_start": 1700000000, "period_end": 1702592000}
				b, _ := json.Marshal(map[string]any{"id": "evt_7", "type": "invoice.paid", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackSubscriptionActivated,
			wantAmount: 1999,
		},
		{
			name: "invoice.payment_failed → past_due",
			body: func() []byte {
				obj := map[string]any{"id": "in_2", "subscription": "sub_1", "amount_due": 1999, "currency": "usd"}
				b, _ := json.Marshal(map[string]any{"id": "evt_8", "type": "invoice.payment_failed", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType: payments.CallbackSubscriptionPastDue,
		},
		{
			name: "customer.subscription.deleted → canceled",
			body: func() []byte {
				obj := map[string]any{"id": "sub_1", "status": "canceled", "metadata": map[string]any{"subscription_id": "local_1"}}
				b, _ := json.Marshal(map[string]any{"id": "evt_9", "type": "customer.subscription.deleted", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType: payments.CallbackSubscriptionCanceled,
		},
		{
			name: "checkout.session.completed mode=subscription",
			body: func() []byte {
				obj := map[string]any{"id": "cs_sub", "mode": "subscription", "subscription": "sub_1", "payment_status": "paid", "amount_total": 1999, "currency": "usd", "metadata": map[string]any{"subscription_id": "local_1"}}
				b, _ := json.Marshal(map[string]any{"id": "evt_10", "type": "checkout.session.completed", "data": map[string]any{"object": obj}})
				return b
			}(),
			wantType:   payments.CallbackSubscriptionUpdated,
			wantAmount: 1999,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := a.normalize(tc.body)
			require.NoError(t, err)
			require.Equal(t, tc.wantType, ev.Type)
			if tc.wantAmount > 0 {
				require.Equal(t, tc.wantAmount, ev.Amount)
			}
		})
	}
}

func TestCreatePayment_FormAndResponse(t *testing.T) {
	var gotAuth, gotIDKey string
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/checkout/sessions", r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotIDKey = r.Header.Get("Idempotency-Key")
		require.NoError(t, r.ParseForm())
		gotForm = r.Form
		_, _ = w.Write([]byte(`{"id":"cs_test_1","url":"https://checkout.stripe.com/c/pay/cs_test_1","payment_intent":"pi_test_1"}`))
	}))
	defer srv.Close()

	a := New(Config{SecretKey: "sk_test_x", WebhookSecret: testWebhookSecret, APIBaseURL: srv.URL})
	session, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{
		OrderID:        "order_9",
		ProjectID:      "shop",
		Amount:         1999,
		Currency:       "USD",
		Description:    "Torchwood order order_9",
		ExpiresAt:      time.Unix(1700003600, 0),
		IdempotencyKey: "order:order_9",
	})
	require.NoError(t, err)
	require.Equal(t, "cs_test_1", session.SessionID)
	require.Equal(t, "https://checkout.stripe.com/c/pay/cs_test_1", session.PaymentURL)
	require.Equal(t, "Bearer sk_test_x", gotAuth)
	require.Equal(t, "order:order_9", gotIDKey)
	// 金额以最小单位整数字符串透传——绝不出现浮点 / 分转元换算。
	require.Equal(t, "1999", gotForm.Get("line_items[0][price_data][unit_amount]"))
	require.Equal(t, "usd", gotForm.Get("line_items[0][price_data][currency]"))
	require.Equal(t, "1", gotForm.Get("line_items[0][quantity]"))
	require.Equal(t, "payment", gotForm.Get("mode"))
	require.Equal(t, "order_9", gotForm.Get("client_reference_id"))
	require.Equal(t, "order_9", gotForm.Get("metadata[order_id]"))
	require.Equal(t, "shop", gotForm.Get("metadata[project_id]"))
	require.Equal(t, "pi_test_1", session.ProviderOrderID)
	require.Equal(t, strconv.FormatInt(1700003600, 10), gotForm.Get("expires_at"))
}

func TestCreatePayment_ProviderError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer srv.Close()
	a := New(Config{SecretKey: "sk_bad", APIBaseURL: srv.URL})
	_, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{OrderID: "o1", ProjectID: "shop", Amount: 100, Currency: "USD"})
	require.Error(t, err)
	pe := payments.AsProviderError(err)
	require.NotNil(t, pe)
	require.Equal(t, 401, pe.Status)
	require.Equal(t, payments.ProviderStripe, pe.Provider)
}

// TestCreatePayment_RequiresProjectMetadata 锁死设计 §9.2「必写」护栏：
// 空 ProjectID 直接拒绝，不向渠道发请求。
func TestCreatePayment_RequiresProjectMetadata(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("CreatePayment must not reach Stripe without project_id")
	}))
	defer srv.Close()
	a := New(Config{SecretKey: "sk_test_x", WebhookSecret: testWebhookSecret, APIBaseURL: srv.URL})
	_, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{OrderID: "o1", Amount: 100, Currency: "USD"})
	require.ErrorIs(t, err, payments.ErrMissingProjectMetadata)
}

func TestRefund_FormAndResponse(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/refunds", r.URL.Path)
		require.NoError(t, r.ParseForm())
		gotForm = r.Form
		_, _ = w.Write([]byte(`{"id":"re_1","status":"succeeded"}`))
	}))
	defer srv.Close()
	a := New(Config{SecretKey: "sk_test_x", APIBaseURL: srv.URL})
	res, err := a.Refund(context.Background(), payments.RefundInput{
		ProviderOrderID: "pi_1",
		Amount:          0,
		IdempotencyKey:  "refund:o1",
	})
	require.NoError(t, err)
	require.True(t, res.Succeeded)
	require.Equal(t, "re_1", res.RefundID)
	require.Equal(t, "pi_1", gotForm.Get("payment_intent"))
	require.Empty(t, gotForm.Get("amount"), "0=全额退款不传 amount")
}
