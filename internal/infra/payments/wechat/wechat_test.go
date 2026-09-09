package wechat

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

const testAPIV3Key = "12345678901234567890123456789012" // 32 bytes

type testKeys struct {
	priv    *rsa.PrivateKey
	certPEM string
	keyPEM  string
}

func generateTestKeys(t *testing.T) testKeys {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "WeChat Pay Test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return testKeys{priv: priv, certPEM: string(certPEM), keyPEM: string(keyPEM)}
}

func newTestAdapter(t *testing.T, keys testKeys) *Adapter {
	t.Helper()
	a := New(Config{
		MchID:              "mch_test",
		AppID:              "wx_test",
		APIV3Key:           testAPIV3Key,
		MerchantSerialNo:   "SERIAL1",
		MerchantPrivateKey: keys.keyPEM,
		PlatformCert:       keys.certPEM,
		NotifyURL:          "https://example.com/v1/payments/callbacks/wechat",
	})
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	return a
}

func encryptResource(t *testing.T, plaintext []byte) notifyResource {
	t.Helper()
	nonce := []byte("123456789012")
	block, err := aes.NewCipher([]byte(testAPIV3Key))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	ad := []byte("transaction")
	sealed := gcm.Seal(nil, nonce, plaintext, ad)
	return notifyResource{
		Algorithm:      "AEAD_AES_256_GCM",
		Ciphertext:     base64.StdEncoding.EncodeToString(sealed),
		Nonce:          string(nonce),
		AssociatedData: string(ad),
	}
}

func signedNotifyFixed(t *testing.T, keys testKeys, envelope notifyEnvelope, ts int64) (http.Header, []byte) {
	t.Helper()
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	nonce := "nonce-test"
	tsStr := strconv.FormatInt(ts, 10)
	msg := tsStr + "\n" + nonce + "\n" + string(body) + "\n"
	sum := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, keys.priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	h := http.Header{}
	h.Set(headerTimestamp, tsStr)
	h.Set(headerNonce, nonce)
	h.Set(headerSignature, base64.StdEncoding.EncodeToString(sig))
	h.Set(headerSerial, "SERIAL1")
	return h, body
}

func paidEnvelope(t *testing.T, eventID, orderID string, amount int64) notifyEnvelope {
	t.Helper()
	plain, err := json.Marshal(map[string]any{
		"out_trade_no":   orderID,
		"transaction_id": "wx_" + eventID,
		"trade_state":    "SUCCESS",
		"attach":         orderID,
		"amount":         map[string]any{"total": amount, "currency": "CNY"},
	})
	require.NoError(t, err)
	return notifyEnvelope{
		ID:        eventID,
		EventType: "TRANSACTION.SUCCESS",
		Resource:  encryptResource(t, plain),
	}
}

func TestVerifyCallback_ValidSignaturePaid(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	h, body := signedNotifyFixed(t, keys, paidEnvelope(t, "EV-1", "order_1", 1999), 1700000000)
	ev, err := a.VerifyCallback(context.Background(), h, body)
	require.NoError(t, err)
	require.Equal(t, payments.ProviderWeChat, ev.Provider)
	require.Equal(t, "EV-1", ev.ProviderEventID)
	require.Equal(t, payments.CallbackPaid, ev.Type)
	require.Equal(t, "order_1", ev.ProviderSessionID)
	require.Equal(t, "wx_EV-1", ev.ProviderOrderID)
	require.Equal(t, "order_1", ev.OrderID)
	require.Equal(t, int64(1999), ev.Amount)
	require.Equal(t, "CNY", ev.Currency)
}

func TestVerifyCallback_ForgedSignature(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	_, body := signedNotifyFixed(t, keys, paidEnvelope(t, "EV-1", "order_1", 1999), 1700000000)
	h := http.Header{}
	h.Set(headerTimestamp, "1700000000")
	h.Set(headerNonce, "nonce-test")
	h.Set(headerSignature, base64.StdEncoding.EncodeToString([]byte("deadbeef")))
	_, err := a.VerifyCallback(context.Background(), h, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_TamperedBody(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	h, body := signedNotifyFixed(t, keys, paidEnvelope(t, "EV-1", "order_1", 1999), 1700000000)
	tampered := append([]byte{}, body...)
	tampered[len(tampered)-2] ^= 0xff
	_, err := a.VerifyCallback(context.Background(), h, tampered)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_MissingHeader(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	_, err := a.VerifyCallback(context.Background(), http.Header{}, []byte(`{}`))
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_NotConfigured(t *testing.T) {
	a := New(Config{})
	_, err := a.VerifyCallback(context.Background(), http.Header{}, []byte(`{}`))
	require.True(t, payments.ErrNotConfigured(err))
}

func TestVerifyCallback_Refunded(t *testing.T) {
	keys := generateTestKeys(t)
	a := newTestAdapter(t, keys)
	plain, err := json.Marshal(map[string]any{
		"out_trade_no":   "order_1",
		"transaction_id": "wx_1",
		"refund_status":  "SUCCESS",
		"amount":         map[string]any{"refund": 1999, "total": 1999, "currency": "CNY"},
	})
	require.NoError(t, err)
	env := notifyEnvelope{ID: "EV-RF", EventType: "REFUND.SUCCESS", Resource: encryptResource(t, plain)}
	h, body := signedNotifyFixed(t, keys, env, 1700000000)
	ev, err := a.VerifyCallback(context.Background(), h, body)
	require.NoError(t, err)
	require.Equal(t, payments.CallbackRefunded, ev.Type)
	require.Equal(t, int64(1999), ev.Amount)
}

func TestCallbackAck(t *testing.T) {
	a := New(Config{})
	st, ct, body := a.CallbackAck(true)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, "application/json", ct)
	require.JSONEq(t, `{"code":"SUCCESS"}`, string(body))
	st, _, body = a.CallbackAck(false)
	require.Equal(t, http.StatusInternalServerError, st)
	require.JSONEq(t, `{"code":"FAIL"}`, string(body))
}

func TestCreatePayment_FormAndResponse(t *testing.T) {
	keys := generateTestKeys(t)
	var gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, nativePath, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"code_url":"weixin://wxpay/bizpayurl?pr=abc"}`))
	}))
	defer srv.Close()
	a := New(Config{
		MchID: "mch_test", AppID: "wx_test", APIV3Key: testAPIV3Key,
		MerchantSerialNo: "SERIAL1", MerchantPrivateKey: keys.keyPEM, PlatformCert: keys.certPEM,
		APIBaseURL: srv.URL, NotifyURL: "https://example.com/cb",
	})
	session, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{
		OrderID: "order_9", Amount: 1999, Currency: "CNY", Description: "Torchwood order order_9",
	})
	require.NoError(t, err)
	require.Equal(t, "order_9", session.SessionID)
	require.Equal(t, "weixin://wxpay/bizpayurl?pr=abc", session.PaymentURL)
	require.Contains(t, gotAuth, "WECHATPAY2-SHA256-RSA2048")
	var req nativeRequest
	require.NoError(t, json.Unmarshal(gotBody, &req))
	require.Equal(t, int64(1999), req.Amount.Total)
	require.Equal(t, "CNY", req.Amount.Currency)
	require.Equal(t, "order_9", req.OutTradeNo)
}

func TestCreatePayment_ProviderError(t *testing.T) {
	keys := generateTestKeys(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"SIGN_ERROR"}`))
	}))
	defer srv.Close()
	a := New(Config{
		MchID: "mch", AppID: "wx", APIV3Key: testAPIV3Key,
		MerchantSerialNo: "S", MerchantPrivateKey: keys.keyPEM, PlatformCert: keys.certPEM,
		APIBaseURL: srv.URL,
	})
	_, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{OrderID: "o1", Amount: 100, Currency: "CNY"})
	pe := payments.AsProviderError(err)
	require.NotNil(t, pe)
	require.Equal(t, 401, pe.Status)
}

func TestRefund_FormAndResponse(t *testing.T) {
	keys := generateTestKeys(t)
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, refundPath, r.URL.Path)
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"refund_id":"rf_1","out_refund_no":"refundorder1","status":"SUCCESS"}`))
	}))
	defer srv.Close()
	a := New(Config{
		MchID: "mch", AppID: "wx", APIV3Key: testAPIV3Key,
		MerchantSerialNo: "S", MerchantPrivateKey: keys.keyPEM, PlatformCert: keys.certPEM,
		APIBaseURL: srv.URL,
	})
	res, err := a.Refund(context.Background(), payments.RefundInput{
		OrderID: "order_1", ProviderOrderID: "wx_1", Amount: 0, OrderAmount: 1999, IdempotencyKey: "refund:order_1",
	})
	require.NoError(t, err)
	require.True(t, res.Succeeded)
	var req refundRequest
	require.NoError(t, json.Unmarshal(gotBody, &req))
	require.Equal(t, int64(1999), req.Amount.Refund)
	require.Equal(t, int64(1999), req.Amount.Total)
	require.Equal(t, "wx_1", req.TransactionID)
}
