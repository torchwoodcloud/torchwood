package iosiap

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

type testChain struct {
	rootPEM string
	leaf    *x509.Certificate
	leafKey *ecdsa.PrivateKey
}

func generateChain(t *testing.T) testChain {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	rootTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Test Apple Root"},
		NotBefore:             time.Unix(1600000000, 0),
		NotAfter:              time.Unix(1900000000, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	require.NoError(t, err)
	rootCert, err := x509.ParseCertificate(rootDER)
	require.NoError(t, err)

	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "Test Apple Leaf"},
		NotBefore:    time.Unix(1600000000, 0),
		NotAfter:     time.Unix(1900000000, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &leafKey.PublicKey, rootKey)
	require.NoError(t, err)
	leafCert, err := x509.ParseCertificate(leafDER)
	require.NoError(t, err)

	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	return testChain{rootPEM: string(rootPEM), leaf: leafCert, leafKey: leafKey}
}

func signJWS(t *testing.T, chain testChain, payload any) string {
	t.Helper()
	payloadJSON, err := json.Marshal(payload)
	require.NoError(t, err)
	hdr := jwsHeader{
		Alg: "ES256",
		X5c: []string{base64.StdEncoding.EncodeToString(chain.leaf.Raw)},
	}
	hdrJSON, err := json.Marshal(hdr)
	require.NoError(t, err)
	hB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	pB64 := base64.RawURLEncoding.EncodeToString(payloadJSON)
	sum := sha256.Sum256([]byte(hB64 + "." + pB64))
	r, s, err := ecdsa.Sign(rand.Reader, chain.leafKey, sum[:])
	require.NoError(t, err)
	rb := r.FillBytes(make([]byte, 32))
	sb := s.FillBytes(make([]byte, 32))
	sig := append(rb, sb...)
	return hB64 + "." + pB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func newTestAdapter(t *testing.T, chain testChain) *Adapter {
	t.Helper()
	a := New(Config{
		BundleID:      "com.example.app",
		SharedSecret:  "shared_secret",
		AppleRootCert: chain.rootPEM,
	})
	a.now = func() time.Time { return time.Unix(1700000000, 0) }
	return a
}

func TestCreatePaymentUnsupported(t *testing.T) {
	a := New(Config{})
	_, err := a.CreatePayment(context.Background(), payments.CreatePaymentInput{OrderID: "o1", Amount: 199})
	require.ErrorIs(t, err, payments.ErrUnsupported)
}

func TestRefundUnsupported(t *testing.T) {
	a := New(Config{})
	_, err := a.Refund(context.Background(), payments.RefundInput{OrderID: "o1", Amount: 199})
	require.ErrorIs(t, err, payments.ErrUnsupported)
}

func TestVerifyCallback_ValidJWSPaid(t *testing.T) {
	chain := generateChain(t)
	a := newTestAdapter(t, chain)
	tx := signedTransaction{
		TransactionID:         "1000000123456789",
		OriginalTransactionID: "1000000123456789",
		ProductID:             "gold_pack",
		BundleID:              "com.example.app",
		PurchaseDate:          1700000000000,
		Price:                 1990,
		Currency:              "USD",
		AppAccountToken:       "order_1",
	}
	txJWS := signJWS(t, chain, tx)
	note := map[string]any{
		"notificationType": "ONE_TIME_CHARGE",
		"notificationUUID": "uuid-1",
		"data": map[string]any{
			"bundleId":              "com.example.app",
			"signedTransactionInfo": txJWS,
		},
	}
	signed := signJWS(t, chain, note)
	body, err := json.Marshal(asnEnvelope{SignedPayload: signed})
	require.NoError(t, err)

	ev, err := a.VerifyCallback(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	require.Equal(t, payments.ProviderIOSIAP, ev.Provider)
	require.Equal(t, "uuid-1", ev.ProviderEventID)
	require.Equal(t, payments.CallbackPaid, ev.Type)
	require.Equal(t, "1000000123456789", ev.ProviderOrderID)
	require.Equal(t, "order_1", ev.OrderID)
	require.Equal(t, int64(199), ev.Amount)
	require.Equal(t, "USD", ev.Currency)
}

func TestVerifyCallback_ForgedSignature(t *testing.T) {
	chain := generateChain(t)
	a := newTestAdapter(t, chain)
	body := []byte(`{"signedPayload":"eyJhbGciOiJFUzI1NiIsIng1YyI6WyJhYmMiXX0.eyJmb28iOiJiYXIifQ.deadbeef"}`)
	_, err := a.VerifyCallback(context.Background(), http.Header{}, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_UntrustedCert(t *testing.T) {
	good := generateChain(t)
	evil := generateChain(t)
	a := newTestAdapter(t, good)
	txJWS := signJWS(t, evil, signedTransaction{TransactionID: "t1", AppAccountToken: "o1", Price: 1000, Currency: "USD"})
	note := map[string]any{
		"notificationType": "ONE_TIME_CHARGE",
		"notificationUUID": "uuid-evil",
		"data":             map[string]any{"signedTransactionInfo": txJWS},
	}
	signed := signJWS(t, evil, note)
	body, err := json.Marshal(asnEnvelope{SignedPayload: signed})
	require.NoError(t, err)
	_, err = a.VerifyCallback(context.Background(), http.Header{}, body)
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestVerifyCallback_Refunded(t *testing.T) {
	chain := generateChain(t)
	a := newTestAdapter(t, chain)
	txJWS := signJWS(t, chain, signedTransaction{TransactionID: "t1", Price: 1990, Currency: "USD"})
	note := map[string]any{
		"notificationType": "REFUND",
		"notificationUUID": "uuid-rf",
		"data":             map[string]any{"signedTransactionInfo": txJWS},
	}
	signed := signJWS(t, chain, note)
	body, err := json.Marshal(asnEnvelope{SignedPayload: signed})
	require.NoError(t, err)
	ev, err := a.VerifyCallback(context.Background(), http.Header{}, body)
	require.NoError(t, err)
	require.Equal(t, payments.CallbackRefunded, ev.Type)
}

func TestVerifyReceipt_JWS(t *testing.T) {
	chain := generateChain(t)
	a := newTestAdapter(t, chain)
	token := signJWS(t, chain, signedTransaction{
		TransactionID: "txn-jws",
		ProductID:     "gold_pack",
		BundleID:      "com.example.app",
		Price:         4990,
		Currency:      "USD",
		PurchaseDate:  1700000000000,
	})
	got, err := a.VerifyReceipt(context.Background(), payments.VerifyReceiptInput{Receipt: []byte(token)})
	require.NoError(t, err)
	require.Equal(t, "txn-jws", got.TransactionID)
	require.Equal(t, "gold_pack", got.ProductID)
	require.Equal(t, int64(499), got.Amount)
}

func TestVerifyReceipt_LegacyAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":0,"environment":"Sandbox","latest_receipt_info":[{"transaction_id":"txn-legacy","original_transaction_id":"txn-legacy","product_id":"gold_pack","purchase_date_ms":"1700000000000"}]}`))
	}))
	defer srv.Close()
	a := New(Config{SharedSecret: "s", VerifyReceiptURL: srv.URL, SandboxVerifyURL: srv.URL, AppleRootCert: appleRootCAG3PEM})
	got, err := a.VerifyReceipt(context.Background(), payments.VerifyReceiptInput{Receipt: []byte("base64receipt")})
	require.NoError(t, err)
	require.Equal(t, "txn-legacy", got.TransactionID)
	require.Equal(t, "gold_pack", got.ProductID)
}

func TestVerifyReceipt_SandboxRetry(t *testing.T) {
	var hits []string
	prod := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "prod")
		_, _ = w.Write([]byte(`{"status":21007}`))
	}))
	defer prod.Close()
	sand := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, "sand")
		_, _ = w.Write([]byte(`{"status":0,"latest_receipt_info":[{"transaction_id":"txn-sb","product_id":"p1","purchase_date_ms":"1"}]}`))
	}))
	defer sand.Close()
	a := New(Config{SharedSecret: "s", VerifyReceiptURL: prod.URL, SandboxVerifyURL: sand.URL, AppleRootCert: appleRootCAG3PEM})
	got, err := a.VerifyReceipt(context.Background(), payments.VerifyReceiptInput{Receipt: []byte("r")})
	require.NoError(t, err)
	require.Equal(t, "txn-sb", got.TransactionID)
	require.Equal(t, []string{"prod", "sand"}, hits)
}

func TestVerifyReceipt_InvalidStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":21003}`))
	}))
	defer srv.Close()
	a := New(Config{SharedSecret: "s", VerifyReceiptURL: srv.URL, AppleRootCert: appleRootCAG3PEM})
	_, err := a.VerifyReceipt(context.Background(), payments.VerifyReceiptInput{Receipt: []byte("r")})
	require.ErrorIs(t, err, payments.ErrSignatureInvalid)
}

func TestCallbackAck(t *testing.T) {
	a := New(Config{})
	st, _, body := a.CallbackAck(true)
	require.Equal(t, http.StatusOK, st)
	require.Empty(t, body)
}

func TestMilliunitsToMinor(t *testing.T) {
	require.Equal(t, int64(199), milliunitsToMinor(1990, "USD"))
	require.Equal(t, int64(199), milliunitsToMinor(199000, "JPY"))
	require.Equal(t, int64(0), milliunitsToMinor(0, "USD"))
}

func TestEmbeddedAppleRootParses(t *testing.T) {
	pool, err := certPoolFromPEM(appleRootCAG3PEM)
	require.NoError(t, err)
	require.NotNil(t, pool)
}
