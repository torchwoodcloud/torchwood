package payments

import (
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/alipay"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/iosiap"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/stripe"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/wechat"
)

// TestFourProviders_VerifyCallbackSameShape 验收：四渠道 VerifyCallback
// 归一化为同一 CallbackEvent 形状；伪造签名一律 ErrSignatureInvalid。
func TestFourProviders_VerifyCallbackSameShape(t *testing.T) {
	type probe struct {
		name     string
		provider domainpayments.PaymentProvider
		headers  http.Header
		valid    []byte
		forged   []byte
	}

	stripeSecret := "whsec_shape"
	stripeBody := []byte(`{"id":"evt_s","type":"checkout.session.completed","data":{"object":{"id":"cs_1","payment_intent":"pi_1","client_reference_id":"o1","payment_status":"paid","amount_total":1999,"currency":"usd"}}}`)
	stripeHdr := stripeSign(stripeSecret, stripeBody, 1700000000)
	stripeForged := http.Header{}
	stripeForged.Set("Stripe-Signature", "t=1700000000,v1=00")

	wxKey, wxPriv := mustRSA(t)
	wxAPIV3 := "12345678901234567890123456789012"
	wxBody, wxHdr := wechatPaid(t, wxPriv, wxAPIV3, 1700000000)
	wxForged := cloneHeader(wxHdr)
	wxForged.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString([]byte("nope")))

	aliKey, aliPriv := mustRSA(t)
	aliBody := alipayPaid(t, aliPriv)
	aliValues, err := url.ParseQuery(string(aliBody))
	require.NoError(t, err)
	aliValues.Set("sign", base64.StdEncoding.EncodeToString([]byte("forged")))
	aliForged := []byte(aliValues.Encode())

	iosChain := mustECDSAChain(t)
	iosBody := iosPaid(t, iosChain)
	iosForged := []byte(`{"signedPayload":"eyJhbGciOiJFUzI1NiJ9.e30.Zm9yZ2Vk"}`)

	cases := []probe{
		{
			name:     "stripe",
			provider: stripe.New(stripe.Config{SecretKey: "sk", WebhookSecret: stripeSecret}),
			headers:  stripeHdr, valid: stripeBody, forged: stripeBody,
		},
		{
			name: "wechat",
			provider: wechat.New(wechat.Config{
				MchID: "m", AppID: "a", APIV3Key: wxAPIV3,
				MerchantSerialNo: "s", MerchantPrivateKey: wxKey.keyPEM, PlatformCert: wxKey.certPEM,
			}),
			headers: wxHdr, valid: wxBody, forged: wxBody,
		},
		{
			name: "alipay",
			provider: alipay.New(alipay.Config{
				AppID: "app", AppPrivateKey: aliKey.keyPEM, AlipayPublicKey: aliKey.pubPEM,
			}),
			headers: http.Header{}, valid: aliBody, forged: aliForged,
		},
		{
			name: "ios_iap",
			provider: iosiap.New(iosiap.Config{
				BundleID: "com.example.app", AppleRootCert: iosChain.rootPEM, SharedSecret: "s",
			}),
			headers: http.Header{}, valid: iosBody, forged: iosForged,
		},
	}
	// stripe forged uses different headers.
	forgedHeaders := map[string]http.Header{
		"stripe":  stripeForged,
		"wechat":  wxForged,
		"alipay":  http.Header{},
		"ios_iap": http.Header{},
	}

	for _, tc := range cases {
		t.Run(tc.name+"/valid", func(t *testing.T) {
			// stripe/wechat 时间戳容忍按 now 重签，避免 5min 窗口失效。
			headers := tc.headers
			body := tc.valid
			if tc.name == "stripe" {
				now := time.Now().Unix()
				body = stripeBody
				headers = stripeSign(stripeSecret, body, now)
			}
			if tc.name == "wechat" {
				body, headers = wechatPaid(t, wxPriv, wxAPIV3, time.Now().Unix())
			}
			ev, err := tc.provider.VerifyCallback(context.Background(), headers, body)
			require.NoError(t, err)
			require.Equal(t, tc.provider.Name(), ev.Provider)
			require.NotEmpty(t, ev.ProviderEventID)
			require.Equal(t, domainpayments.CallbackPaid, ev.Type)
			require.Greater(t, ev.Amount, int64(0))
			require.NotEmpty(t, ev.Currency)
			require.NotEmpty(t, ev.OrderID)
		})
		t.Run(tc.name+"/forged", func(t *testing.T) {
			_, err := tc.provider.VerifyCallback(context.Background(), forgedHeaders[tc.name], tc.forged)
			require.ErrorIs(t, err, domainpayments.ErrSignatureInvalid)
		})
	}
}

func stripeSign(secret string, body []byte, ts int64) http.Header {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	h := http.Header{}
	h.Set("Stripe-Signature", "t="+strconv.FormatInt(ts, 10)+",v1="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

type rsaBundle struct{ keyPEM, certPEM, pubPEM string }

func mustRSA(t *testing.T) (rsaBundle, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "t"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	require.NoError(t, err)
	pubDER, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	require.NoError(t, err)
	return rsaBundle{
		keyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		certPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		pubPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})),
	}, priv
}

func wechatPaid(t *testing.T, priv *rsa.PrivateKey, apiV3 string, ts int64) ([]byte, http.Header) {
	t.Helper()
	plain, err := json.Marshal(map[string]any{
		"out_trade_no": "o1", "transaction_id": "wx1", "trade_state": "SUCCESS", "attach": "o1",
		"amount": map[string]any{"total": 1999, "currency": "CNY"},
	})
	require.NoError(t, err)
	nonce := []byte("123456789012")
	block, err := aes.NewCipher([]byte(apiV3))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	ad := []byte("transaction")
	sealed := gcm.Seal(nil, nonce, plain, ad)
	env := map[string]any{
		"id": "EV-1", "event_type": "TRANSACTION.SUCCESS",
		"resource": map[string]any{
			"algorithm": "AEAD_AES_256_GCM", "ciphertext": base64.StdEncoding.EncodeToString(sealed),
			"nonce": string(nonce), "associated_data": string(ad),
		},
	}
	body, err := json.Marshal(env)
	require.NoError(t, err)
	nstr := "n1"
	tsStr := strconv.FormatInt(ts, 10)
	sum := sha256.Sum256([]byte(tsStr + "\n" + nstr + "\n" + string(body) + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	h := http.Header{}
	h.Set("Wechatpay-Timestamp", tsStr)
	h.Set("Wechatpay-Nonce", nstr)
	h.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	return body, h
}

func alipayPaid(t *testing.T, priv *rsa.PrivateKey) []byte {
	t.Helper()
	values := url.Values{}
	values.Set("notify_id", "nid_1")
	values.Set("out_trade_no", "o1")
	values.Set("trade_no", "ali_1")
	values.Set("trade_status", "TRADE_SUCCESS")
	values.Set("total_amount", "19.99")
	keys := []string{"notify_id", "out_trade_no", "total_amount", "trade_no", "trade_status"}
	var content string
	for i, k := range keys {
		if i > 0 {
			content += "&"
		}
		content += k + "=" + values.Get(k)
	}
	sum := sha256.Sum256([]byte(content))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	values.Set("sign", base64.StdEncoding.EncodeToString(sig))
	values.Set("sign_type", "RSA2")
	return []byte(values.Encode())
}

type ecdsaChain struct {
	rootPEM string
	leaf    *x509.Certificate
	leafKey *ecdsa.PrivateKey
}

func mustECDSAChain(t *testing.T) ecdsaChain {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	rootTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "root"},
		NotBefore: time.Unix(1600000000, 0), NotAfter: time.Unix(1900000000, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true, IsCA: true,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rootKey.PublicKey, rootKey)
	require.NoError(t, err)
	rootCert, err := x509.ParseCertificate(rootDER)
	require.NoError(t, err)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "leaf"},
		NotBefore: time.Unix(1600000000, 0), NotAfter: time.Unix(1900000000, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, rootCert, &leafKey.PublicKey, rootKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(leafDER)
	require.NoError(t, err)
	return ecdsaChain{
		rootPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})),
		leaf:    leaf, leafKey: leafKey,
	}
}

func iosPaid(t *testing.T, chain ecdsaChain) []byte {
	t.Helper()
	tx := map[string]any{
		"transactionId": "1001", "originalTransactionId": "1001", "productId": "gold",
		"bundleId": "com.example.app", "price": 1990, "currency": "USD", "appAccountToken": "o1",
	}
	txJWS := signES256(t, chain, tx)
	note := map[string]any{
		"notificationType": "ONE_TIME_CHARGE", "notificationUUID": "uuid-1",
		"data": map[string]any{"signedTransactionInfo": txJWS, "bundleId": "com.example.app"},
	}
	body, err := json.Marshal(map[string]any{"signedPayload": signES256(t, chain, note)})
	require.NoError(t, err)
	return body
}

func signES256(t *testing.T, chain ecdsaChain, payload any) string {
	t.Helper()
	p, err := json.Marshal(payload)
	require.NoError(t, err)
	hdr, err := json.Marshal(map[string]any{"alg": "ES256", "x5c": []string{base64.StdEncoding.EncodeToString(chain.leaf.Raw)}})
	require.NoError(t, err)
	hB64 := base64.RawURLEncoding.EncodeToString(hdr)
	pB64 := base64.RawURLEncoding.EncodeToString(p)
	sum := sha256.Sum256([]byte(hB64 + "." + pB64))
	r, s, err := ecdsa.Sign(rand.Reader, chain.leafKey, sum[:])
	require.NoError(t, err)
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return hB64 + "." + pB64 + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
