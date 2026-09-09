package serverhttp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/stretchr/testify/require"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	infrapayments "github.com/torchwoodcloud/torchwood/internal/infra/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/alipay"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/iosiap"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/stripe"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/wechat"
)

// writeSpyCallbackRepo 记录 InsertIfAbsent 调用次数：验签失败路径
// 不得落 payment_callback_events（设计 §1.4 / 验收「假签无落库」）。
type writeSpyCallbackRepo struct{ inserts int }

func (s *writeSpyCallbackRepo) InsertIfAbsent(context.Context, *domainpayments.CallbackEvent, string, string) (bool, error) {
	s.inserts++
	return false, errors.New("payments: callback persist must not run after verify failure")
}

func newCallbackOnlyPayments(t *testing.T) (*apppayments.Payments, *writeSpyCallbackRepo) {
	t.Helper()
	adapter := stripe.New(stripe.Config{ // #nosec G101 -- 测试固定值
		SecretKey:     "sk_test_x",
		WebhookSecret: "whsec_handler_test",
	})
	spy := &writeSpyCallbackRepo{}
	return apppayments.NewPayments(
		nil, nil, nil, spy, nil,
		apppayments.NewRecordOnlyFulfiller(),
		infrapayments.NewRegistry(
			adapter,
			wechat.New(wechat.Config{APIV3Key: "k", PlatformCert: "x"}),
			alipay.New(alipay.Config{AlipayPublicKey: "x"}),
			iosiap.New(iosiap.Config{SharedSecret: "s"}),
		),
		nil, nil, nil, nil, nil,
	), spy
}

func postCallback(h *PaymentsHandler, provider, body string, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/payments/callbacks/"+provider, strings.NewReader(body))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.callback(rec, req, map[string]string{"provider": provider})
	return rec
}

func TestPaymentsHandler_ForgedSignatureReturns401(t *testing.T) {
	uc, spy := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	body := `{"id":"evt_1","type":"checkout.session.completed","data":{"object":{"id":"cs_1"}}}`
	hdr := http.Header{}
	hdr.Set("Stripe-Signature", "t=1700000000,v1=0000000000000000000000000000000000000000000000000000000000000000")
	rec := postCallback(h, "stripe", body, hdr)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Empty(t, rec.Body.String(), "不返回区分性错误")
	require.Equal(t, 0, spy.inserts, "假签不得落 payment_callback_events")
}

func TestPaymentsHandler_MissingSignatureHeaderReturns401(t *testing.T) {
	uc, spy := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	rec := postCallback(h, "stripe", `{}`, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, 0, spy.inserts, "缺签不得落库")
}

func TestPaymentsHandler_UnconfiguredProviderReturns401(t *testing.T) {
	adapter := stripe.New(stripe.Config{})
	uc := apppayments.NewPayments(nil, nil, nil, nil, nil,
		apppayments.NewRecordOnlyFulfiller(), infrapayments.NewRegistry(adapter), nil, nil, nil, nil, nil)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	hdr := http.Header{}
	hdr.Set("Stripe-Signature", "t=1700000000,v1=abc")
	rec := postCallback(h, "stripe", `{}`, hdr)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestPaymentsHandler_BodyTooLargeReturns401(t *testing.T) {
	uc, spy := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	big := strings.Repeat("a", maxCallbackBody+2)
	rec := postCallback(h, "stripe", big, nil)
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, 0, spy.inserts)
}

func TestPaymentsHandler_RegisterRoute(t *testing.T) {
	uc, _ := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)
	mux := runtime.NewServeMux()
	h.Register(mux)

	for _, provider := range []string{"stripe", "wechat", "alipay", "ios_iap"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/payments/callbacks/"+provider, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		require.Equal(t, http.StatusUnauthorized, rec.Code, provider)
	}
}

func TestPaymentsHandler_UnknownProvider(t *testing.T) {
	uc, spy := newCallbackOnlyPayments(t)
	err := uc.HandleCallback(context.Background(), "paypal", http.Header{}, []byte(`{}`))
	require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)
	require.Equal(t, 0, spy.inserts, "未知渠道不得落库")
}

func TestPaymentsHandler_AckFormats(t *testing.T) {
	uc, _ := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	// 处理失败（验签已过但内部错误）才走渠道回执；这里直接测 CallbackAck。
	st, ct, body := uc.CallbackAck(domainpayments.ProviderWeChat, true)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "application/json", ct)
	require.JSONEq(t, `{"code":"SUCCESS"}`, string(body))

	st, ct, body = uc.CallbackAck(domainpayments.ProviderAlipay, true)
	require.Equal(t, http.StatusOK, st)
	require.Contains(t, ct, "text/plain")
	require.Equal(t, "success", string(body))

	st, _, body = uc.CallbackAck(domainpayments.ProviderStripe, true)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, body)

	st, _, body = uc.CallbackAck(domainpayments.ProviderIOSIAP, true)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, body)

	_ = h
}

func TestPaymentsHandler_IndexMissReturns503FailBody(t *testing.T) {
	uc, spy := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	body := `{"id":"evt_early","type":"checkout.session.completed","data":{"object":{"id":"cs_1","client_reference_id":"ord_1","payment_status":"paid","amount_total":100,"currency":"usd","metadata":{"order_id":"ord_1","project_id":"shop"}}}}`
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte("whsec_handler_test"))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write([]byte(body))
	hdr := http.Header{}
	hdr.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	rec := postCallback(h, "stripe", body, hdr)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Empty(t, rec.Body.String())
	require.Equal(t, 0, spy.inserts)
}

// TestPaymentsHandler_NoPlatformRefReturns200 锁 K21「无任何我方 ref → 200 +
// 日志」：Stripe 账号级噪音（他人 PI，无 client_reference_id / metadata）
// 不 503（重试三天）、不落库。
func TestPaymentsHandler_NoPlatformRefReturns200(t *testing.T) {
	uc, spy := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	body := `{"id":"evt_noise","type":"checkout.session.completed","data":{"object":{"id":"cs_other","payment_intent":"pi_other","payment_status":"paid","amount_total":100,"currency":"usd"}}}`
	ts := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte("whsec_handler_test"))
	_, _ = fmt.Fprintf(mac, "%d.", ts)
	mac.Write([]byte(body))
	hdr := http.Header{}
	hdr.Set("Stripe-Signature", fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil))))
	rec := postCallback(h, "stripe", body, hdr)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 0, spy.inserts, "无关事件不得落 payment_callback_events")
}

// TestPaymentsHandler_RetryFailBodyPerProvider 锁 K21 表格的渠道 FAIL 体：
// writeAckRetry（503）必须复用各渠道 CallbackAck(false) 的失败序列化——
// 微信 {"code":"FAIL"}、支付宝 fail、Stripe 空体——而不是 200 SUCCESS / 500。
func TestPaymentsHandler_RetryFailBodyPerProvider(t *testing.T) {
	uc, _ := newCallbackOnlyPayments(t)
	h, err := NewPaymentsHandler(uc, nil, nil)
	require.NoError(t, err)

	cases := []struct {
		provider string
		wantBody string
		wantCT   string
	}{
		{domainpayments.ProviderWeChat, `{"code":"FAIL"}`, "application/json"},
		{domainpayments.ProviderAlipay, "fail", "text/plain"},
		{domainpayments.ProviderStripe, "", ""},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		h.writeAckRetry(rec, tc.provider)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, tc.provider)
		require.Equal(t, tc.wantBody, rec.Body.String(), tc.provider)
		if tc.wantCT != "" {
			require.Contains(t, rec.Header().Get("Content-Type"), tc.wantCT, tc.provider)
		}
	}
}
