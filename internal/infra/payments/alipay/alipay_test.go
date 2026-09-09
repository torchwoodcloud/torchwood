package alipay

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

type testKeys struct {
	priv   *rsa.PrivateKey
	keyPEM string
	pubPEM string
}

func generateTestKeys(t *testing.T) testKeys {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyDER := x509.MarshalPKCS1PrivateKey(priv)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyDER})
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return testKeys{priv: priv, keyPEM: string(keyPEM), pubPEM: string(pubPEM)}
}

func newTestAdapter(t *testing.T, keys testKeys) *Adapter {
	t.Helper()
	a := New(Config{
		AppID:           "app_test",
		AppPrivateKey:   keys.keyPEM,
		AlipayPublicKey: keys.pubPEM,
		NotifyURL:       "https://example.com/v1/payments/callbacks/alipay",
	})
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	return a
}

func signedNotify(t *testing.T, keys testKeys, fields map[string]string) []byte {
	t.Helper()
	values := url.Values{}
	for k, v := range fields {
		values.Set(k, v)
	}
	content := signContent(values)
	sum := sha256.Sum256([]byte(content))
	sig, err := rsa.SignPKCS1v15(rand.Reader, keys.priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	values.Set("sign", base64.StdEncoding.EncodeToString(sig))
	values.Set("sign_type", "RSA2")
	return []byte(values.Encode())
}

func TestFenYuanRoundTrip(t *testing.T) {
	cases := []int64{1, 99, 100, 1999, 0}
	for _, fen := range cases {
		yuan := fenToYuan(fen)
		got, err := yuanToFen(yuan)
		require.NoError(t, err)
		require.Equal(t, fen, got)
	}
	n, err := yuanToFen("19.9")
	require.NoError(t, err)
	require.Equal(t, int64(1990), n)
	_, err = yuanToFen("19.999")
	require.Error(t, err)
}

func TestVerifyCallback_ValidSignaturePaid(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	body := signedNotify(t, keys, map[string]string{
		"notify_id":    "nid_1",
		"out_trade_no": "order_1",
		"trade_no":     "ali_1",
		"trade_status": "TRADE_SUCCESS",
		"total_amount": "19.99",
		"app_id":       "app_test",
	})
	ev, err := a.VerifyCallback(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	require.Equal(t, payments.ProviderAlipay, ev.Provider)
	require.Equal(t, "nid_1", ev.ProviderEventID)
	require.Equal(t, payments.CallbackPaid, ev.Type)
	require.Equal(t, "order_1", ev.OrderID)
	require.Equal(t, "ali_1", ev.ProviderOrderID)
	require.Equal(t, int64(1999), ev.Amount)
	require.Equal(t, "CNY", ev.Currency)
}

func TestVerifyCallback_ForgedSignature(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	body := signedNotify(t, keys, map[string]string{
		"notify_id": "nid_1", "out_trade_no": "order_1", "trade_status": "TRADE_SUCCESS", "total_amount": "19.99",
	})
	values, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	values.Set("sign", base64.StdEncoding.EncodeToString([]byte("deadbeef")))
	_, err = a.VerifyCallback(context.Background(), http.Header{}, []byte(values.Encode()))
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_TamperedAmount(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	body := signedNotify(t, keys, map[string]string{
		"notify_id": "nid_1", "out_trade_no": "order_1", "trade_status": "TRADE_SUCCESS", "total_amount": "19.99",
	})
	values, err := url.ParseQuery(string(body))
	require.NoError(t, err)
	values.Set("total_amount", "99.99")
	_, err = a.VerifyCallback(context.Background(), http.Header{}, []byte(values.Encode()))
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_MissingSign(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	_, err := a.VerifyCallback(context.Background(), http.Header{}, []byte("trade_status=TRADE_SUCCESS"))
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_NotConfigured(t *testing.T) {
	a := New(Config{})
	_, err := a.VerifyCallback(context.Background(), http.Header{}, []byte("a=b"))
	require.True(t, payments.ErrNotConfigured(err))
}

func TestVerifyCallback_Closed(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	body := signedNotify(t, keys, map[string]string{
		"notify_id": "nid_c", "out_trade_no": "order_1", "trade_status": "TRADE_CLOSED", "total_amount": "1.00",
	})
	ev, err := a.VerifyCallback(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	require.Equal(t, payments.CallbackFailed, ev.Type)
}

func TestCallbackAck(t *testing.T) {
	a := New(Config{})
	st, ct, body := a.CallbackAck(true)
	require.Equal(t, http.StatusOK, st)
	require.Contains(t, ct, "text/plain")
	require.Equal(t, "success", string(body))
	st, _, body = a.CallbackAck(false)
	require.Equal(t, "fail", string(body))
	require.Equal(t, http.StatusInternalServerError, st)
}

func TestCreatePayment_FormAndResponse(t *testing.T) {
	keys := generateTestKeys(t)
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"alipay_trade_precreate_response":{"code":"10000","msg":"Success","out_trade_no":"order_9","qr_code":"https://qr.alipay.com/baxxxx"}}`))
	}))
	defer srv.Close()
	a := New(Config{
		AppID: "app_test", AppPrivateKey: keys.keyPEM, AlipayPublicKey: keys.pubPEM,
		GatewayURL: srv.URL, NotifyURL: "https://example.com/cb",
	})
	session, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{
		OrderID: "order_9", Amount: 1999, Currency: "CNY", Description: "Torchwood order order_9",
	})
	require.NoError(t, err)
	require.Equal(t, "order_9", session.SessionID)
	require.Equal(t, "https://qr.alipay.com/baxxxx", session.PaymentURL)
	require.Equal(t, methodPrecreate, gotForm.Get("method"))
	require.Contains(t, gotForm.Get("biz_content"), `"total_amount":"19.99"`)
	require.Contains(t, gotForm.Get("biz_content"), `"out_trade_no":"order_9"`)
	require.NotEmpty(t, gotForm.Get("sign"))
}

func TestCreatePayment_ProviderError(t *testing.T) {
	keys := generateTestKeys(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"alipay_trade_precreate_response":{"code":"40002","msg":"Invalid"}}`))
	}))
	defer srv.Close()
	a := New(Config{AppID: "app", AppPrivateKey: keys.keyPEM, AlipayPublicKey: keys.pubPEM, GatewayURL: srv.URL})
	_, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{OrderID: "o1", Amount: 100, Currency: "CNY"})
	pe := payments.AsProviderError(err)
	require.NotNil(t, pe)
}

func TestRefund_FormAndResponse(t *testing.T) {
	keys := generateTestKeys(t)
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"alipay_trade_refund_response":{"code":"10000","msg":"Success","trade_no":"ali_1"}}`))
	}))
	defer srv.Close()
	a := New(Config{AppID: "app", AppPrivateKey: keys.keyPEM, AlipayPublicKey: keys.pubPEM, GatewayURL: srv.URL})
	res, err := a.Refund(context.Background(), payments.RefundInput{
		OrderID: "order_1", ProviderOrderID: "ali_1", Amount: 0, OrderAmount: 1999, IdempotencyKey: "refund:order_1",
	})
	require.NoError(t, err)
	require.True(t, res.Succeeded)
	form, err := url.ParseQuery(string(gotBody))
	require.NoError(t, err)
	require.Equal(t, methodRefund, form.Get("method"))
	require.Contains(t, form.Get("biz_content"), `"refund_amount":"19.99"`)
}
