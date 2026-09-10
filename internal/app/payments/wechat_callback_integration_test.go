package payments

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
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	infrapayments "github.com/torchwoodcloud/torchwood/internal/infra/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/wechat"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// 本文件锁设计 §9.2 / PR6 验收「微信式 OrderID=本地 ULID、无 Stripe session
// 的回调能关单」的国内主路径：建单（native，SessionID=out_trade_no=本地 ULID）
// → TRANSACTION.SUCCESS 通知只凭 index(payment_session, ULID) 路由进项目 schema
// 并驱动状态机；早到（index 缺失）→ ErrProviderIndexMiss。

const wechatTestAPIV3Key = "0123456789abcdef0123456789abcdef"

// wechatEnvelope / wechatResource 镜像 wechat 适配器的通知报文形状（包私有，
// 这里按公开 JSON 契约重新声明）。
type wechatEnvelope struct {
	ID           string         `json:"id"`
	CreateTime   string         `json:"create_time"`
	EventType    string         `json:"event_type"`
	Summary      string         `json:"summary"`
	ResourceType string         `json:"resource_type"`
	Resource     wechatResource `json:"resource"`
}

type wechatResource struct {
	Algorithm      string `json:"algorithm"`
	Ciphertext     string `json:"ciphertext"`
	Nonce          string `json:"nonce"`
	AssociatedData string `json:"associated_data"`
}

type wechatTestKeys struct {
	priv    *rsa.PrivateKey
	keyPEM  string
	certPEM string
}

func generateWeChatTestKeys(t *testing.T) wechatTestKeys {
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
	return wechatTestKeys{priv: priv, keyPEM: string(keyPEM), certPEM: string(certPEM)}
}

func encryptWeChatResource(t *testing.T, plaintext []byte) wechatResource {
	t.Helper()
	nonce := []byte("123456789012")
	block, err := aes.NewCipher([]byte(wechatTestAPIV3Key))
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	sealed := gcm.Seal(nil, nonce, plaintext, []byte("transaction"))
	return wechatResource{
		Algorithm:      "AEAD_AES_256_GCM",
		Ciphertext:     base64.StdEncoding.EncodeToString(sealed),
		Nonce:          string(nonce),
		AssociatedData: "transaction",
	}
}

// signWeChatNotify 用平台证书私钥构造合法 Wechatpay-Signature 头（真实时钟，
// adapter.now 默认 time.Now，落在 5 分钟 skew 窗口内）。
func signWeChatNotify(t *testing.T, keys wechatTestKeys, envelope wechatEnvelope) (http.Header, []byte) {
	t.Helper()
	body, err := json.Marshal(envelope)
	require.NoError(t, err)
	nonce := "nonce-itest"
	tsStr := strconv.FormatInt(time.Now().Unix(), 10)
	sum := sha256.Sum256([]byte(tsStr + "\n" + nonce + "\n" + string(body) + "\n"))
	sig, err := rsa.SignPKCS1v15(rand.Reader, keys.priv, crypto.SHA256, sum[:])
	require.NoError(t, err)
	h := http.Header{}
	h.Set("Wechatpay-Timestamp", tsStr)
	h.Set("Wechatpay-Nonce", nonce)
	h.Set("Wechatpay-Signature", base64.StdEncoding.EncodeToString(sig))
	h.Set("Wechatpay-Serial", "SERIAL1")
	return h, body
}

func weChatPaidEnvelope(t *testing.T, eventID, orderID string, amount int64) wechatEnvelope {
	t.Helper()
	plain, err := json.Marshal(map[string]any{
		"out_trade_no":   orderID,
		"transaction_id": "wx_" + eventID,
		"trade_state":    "SUCCESS",
		"attach":         orderID,
		"amount":         map[string]any{"total": amount, "currency": "CNY"},
	})
	require.NoError(t, err)
	return wechatEnvelope{
		ID:        eventID,
		EventType: "TRANSACTION.SUCCESS",
		Resource:  encryptWeChatResource(t, plain),
	}
}

func setupWeChatEnv(t *testing.T) (*testEnv, wechatTestKeys) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)

	nativeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code_url":"weixin://wx_pay/%d"}`, time.Now().UnixNano())
	}))
	t.Cleanup(nativeSrv.Close)

	keys := generateWeChatTestKeys(t)
	adapter := wechat.New(wechat.Config{
		MchID:              "mch_test",
		AppID:              "wx_test",
		APIV3Key:           wechatTestAPIV3Key,
		MerchantSerialNo:   "SERIAL1",
		MerchantPrivateKey: keys.keyPEM,
		PlatformCert:       keys.certPEM,
		NotifyURL:          "https://example.com/v1/payments/callbacks/wechat",
		APIBaseURL:         nativeSrv.URL,
	})
	uc := NewPayments(
		nil,
		db,
		bunrepo.NewPaymentOrderRepository(db),
		bunrepo.NewPaymentCallbackEventRepository(db),
		bunrepo.NewPaymentFulfillmentRepository(db),
		NewRecordOnlyFulfiller(),
		infrapayments.NewRegistry(adapter),
		infraevents.NewEventOutbox(db),
		nil,
		nil,
		bunrepo.NewProjectRepository(db),
		bunrepo.NewProviderIndexRepository(db),
	)
	return &testEnv{payments: uc, db: db}, keys
}

// TestPayments_WeChatULIDCallbackClosesOrder 国内主路径端到端：
// CreateOrder(wechat native) → TRANSACTION.SUCCESS（out_trade_no=本地 ULID、
// 无 Stripe session）→ 只凭 provider_resource_index 路由进项目 schema 关单。
func TestPayments_WeChatULIDCallbackClosesOrder(t *testing.T) {
	env, keys := setupWeChatEnv(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, env.db)
	defer cleanup()
	uCtx := endUserCtx(ctx, projectID, "uwx")

	result, err := env.payments.CreateOrder(uCtx, CreateOrderCommand{
		Provider:       domainpayments.ProviderWeChat,
		Amount:         1999,
		Currency:       "CNY",
		PurposeKind:    domainpayments.PurposeTopup,
		Purpose:        map[string]any{"amount": 1999, "currency_code": "CNY"},
		IdempotencyKey: "idem-wechat-1",
	})
	require.NoError(t, err)
	orderID := result.Order.ID
	// native 渠道 SessionID 即本地 ULID（out_trade_no）。
	require.Equal(t, orderID, result.Order.ProviderSessionID)

	// 微信回调：out_trade_no=本地 ULID（OrderID），无任何 Stripe 引用。
	h, body := signWeChatNotify(t, keys, weChatPaidEnvelope(t, "EV-WX-1", orderID, 1999))
	require.NoError(t, env.payments.HandleCallback(ctx, domainpayments.ProviderWeChat, h, body))

	order, err := env.payments.GetMyOrder(uCtx, orderID)
	require.NoError(t, err)
	require.Equal(t, domainpayments.OrderStatusPaid, order.Status)
	require.Equal(t, "wx_EV-WX-1", order.ProviderOrderID)
	require.Equal(t, 1, countRows(t, env, projectID, "payment_fulfillments", "order_id = ?", orderID))
	require.Equal(t, 1, countRows(t, env, "", "document_events_outbox", "project_id = ?", projectID))

	// 早到 webhook：本地 ULID 未在 index（未建单），携带我方 ref 形态
	// （out_trade_no 非空）→ ErrProviderIndexMiss（HTTP 层映射 503，K21）。
	h2, body2 := signWeChatNotify(t, keys, weChatPaidEnvelope(t, "EV-WX-EARLY", "01EARLYULIDNOTININDEX", 100))
	err = env.payments.HandleCallback(ctx, domainpayments.ProviderWeChat, h2, body2)
	require.ErrorIs(t, err, domainpayments.ErrProviderIndexMiss)
}
