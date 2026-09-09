// Package wechat 是 PaymentProvider 端口的微信支付 APIv3 适配器
// （v3 设计 §1.1/§1.2）：Native 预下单 + 回调 RSA 验签 / AES-GCM 解密
// 归一化为 CallbackEvent；回执 JSON {"code":"SUCCESS"}。
package wechat

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

const (
	DefaultAPIBaseURL = "https://api.mch.weixin.qq.com"
	maxCallbackBody   = 1 << 20
	providerName      = payments.ProviderWeChat
	nativePath        = "/v3/pay/transactions/native"
	refundPath        = "/v3/refund/domestic/refunds"
	headerTimestamp   = "Wechatpay-Timestamp"
	headerNonce       = "Wechatpay-Nonce"
	headerSignature   = "Wechatpay-Signature"
	headerSerial      = "Wechatpay-Serial"
	timestampSkew     = 5 * time.Minute
)

// Config 是微信支付适配器配置（secret 一律来自环境变量）。
type Config struct {
	MchID              string
	AppID              string
	APIV3Key           string // 32 字节 APIv3 密钥
	MerchantSerialNo   string
	MerchantPrivateKey string // PEM
	PlatformCert       string // PEM
	NotifyURL          string
	APIBaseURL         string
}

// Adapter 实现 payments.PaymentProvider 与 CallbackAcker。
type Adapter struct {
	cfg      Config
	client   *http.Client
	priv     *rsa.PrivateKey
	platform *x509.Certificate
	now      func() time.Time
	nonce    func() string
}

// New 构造适配器。凭据缺失时构造不失败（服务可启动），操作 fail-closed。
func New(cfg Config) *Adapter {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL
	}
	a := &Adapter{
		cfg:    cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		now:    time.Now,
		nonce:  randomNonce,
	}
	if key, err := parseRSAPrivateKey(cfg.MerchantPrivateKey); err == nil {
		a.priv = key
	}
	if cert, err := parseCertificate(cfg.PlatformCert); err == nil {
		a.platform = cert
	}
	return a
}

func (a *Adapter) Name() string { return providerName }

func (a *Adapter) configured() bool {
	return a.cfg.MchID != "" && a.cfg.AppID != "" && a.cfg.APIV3Key != "" &&
		a.priv != nil && a.cfg.MerchantSerialNo != ""
}

func (a *Adapter) callbackConfigured() bool {
	return a.platform != nil && a.cfg.APIV3Key != ""
}

// CreatePayment 调用 Native 下单，返回 code_url 作为 PaymentURL。
func (a *Adapter) CreatePayment(ctx context.Context, in payments.CreatePaymentInput) (*payments.PaymentSession, error) {
	if !a.configured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	body := nativeRequest{
		AppID:       a.cfg.AppID,
		MchID:       a.cfg.MchID,
		Description: in.Description,
		OutTradeNo:  in.OrderID,
		NotifyURL:   a.cfg.NotifyURL,
		Attach:      in.OrderID,
		Amount: nativeAmount{
			Total:    in.Amount,
			Currency: strings.ToUpper(in.Currency),
		},
	}
	if !in.ExpiresAt.IsZero() {
		body.TimeExpire = in.ExpiresAt.Format(time.RFC3339)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out nativeResponse
	if err := a.do(ctx, http.MethodPost, nativePath, raw, &out); err != nil {
		return nil, err
	}
	if out.CodeURL == "" {
		return nil, fmt.Errorf("wechat: native response missing code_url")
	}
	return &payments.PaymentSession{SessionID: in.OrderID, PaymentURL: out.CodeURL}, nil
}

type nativeRequest struct {
	AppID       string       `json:"appid"`
	MchID       string       `json:"mchid"`
	Description string       `json:"description"`
	OutTradeNo  string       `json:"out_trade_no"`
	NotifyURL   string       `json:"notify_url,omitempty"`
	Attach      string       `json:"attach,omitempty"`
	TimeExpire  string       `json:"time_expire,omitempty"`
	Amount      nativeAmount `json:"amount"`
}

type nativeAmount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}

type nativeResponse struct {
	CodeURL string `json:"code_url"`
}

// VerifyCallback 验签并解密 APIv3 通知，归一化为 CallbackEvent。
func (a *Adapter) VerifyCallback(ctx context.Context, headers http.Header, rawBody []byte) (*payments.CallbackEvent, error) {
	if len(rawBody) > maxCallbackBody {
		return nil, payments.ErrSignatureInvalid
	}
	if !a.callbackConfigured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	if err := a.verifySignature(headers, rawBody); err != nil {
		return nil, err
	}
	return a.normalize(rawBody)
}

func (a *Adapter) verifySignature(headers http.Header, rawBody []byte) error {
	ts := headers.Get(headerTimestamp)
	nonce := headers.Get(headerNonce)
	sig := headers.Get(headerSignature)
	if ts == "" || nonce == "" || sig == "" {
		return payments.ErrSignatureInvalid
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return payments.ErrSignatureInvalid
	}
	if a.now().Sub(time.Unix(unix, 0)) > timestampSkew || time.Unix(unix, 0).Sub(a.now()) > timestampSkew {
		return payments.ErrSignatureInvalid
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return payments.ErrSignatureInvalid
	}
	message := ts + "\n" + nonce + "\n" + string(rawBody) + "\n"
	sum := sha256.Sum256([]byte(message))
	pub, ok := a.platform.PublicKey.(*rsa.PublicKey)
	if !ok {
		return payments.ErrSignatureInvalid
	}
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sigBytes); err != nil {
		return payments.ErrSignatureInvalid
	}
	return nil
}

type notifyEnvelope struct {
	ID           string         `json:"id"`
	CreateTime   string         `json:"create_time"`
	EventType    string         `json:"event_type"`
	Summary      string         `json:"summary"`
	ResourceType string         `json:"resource_type"`
	Resource     notifyResource `json:"resource"`
}

type notifyResource struct {
	Algorithm      string `json:"algorithm"`
	Ciphertext     string `json:"ciphertext"`
	Nonce          string `json:"nonce"`
	AssociatedData string `json:"associated_data"`
	OriginalType   string `json:"original_type"`
}

type transactionResource struct {
	AppID         string `json:"appid"`
	MchID         string `json:"mchid"`
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	TradeType     string `json:"trade_type"`
	TradeState    string `json:"trade_state"`
	Attach        string `json:"attach"`
	SuccessTime   string `json:"success_time"`
	Amount        struct {
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

type refundResource struct {
	OutTradeNo    string `json:"out_trade_no"`
	TransactionID string `json:"transaction_id"`
	OutRefundNo   string `json:"out_refund_no"`
	RefundID      string `json:"refund_id"`
	RefundStatus  string `json:"refund_status"`
	Amount        struct {
		Refund   int64  `json:"refund"`
		Total    int64  `json:"total"`
		Currency string `json:"currency"`
	} `json:"amount"`
}

func (a *Adapter) normalize(rawBody []byte) (*payments.CallbackEvent, error) {
	var env notifyEnvelope
	if err := json.Unmarshal(rawBody, &env); err != nil {
		return nil, fmt.Errorf("wechat: decode notify: %w", err)
	}
	if env.ID == "" {
		return nil, fmt.Errorf("wechat: notify missing id")
	}
	plain, err := decryptResource(a.cfg.APIV3Key, env.Resource)
	if err != nil {
		return nil, fmt.Errorf("wechat: decrypt resource: %w", err)
	}
	out := &payments.CallbackEvent{
		Provider:        providerName,
		ProviderEventID: env.ID,
		Raw:             append([]byte(nil), rawBody...),
		ReceivedAt:      a.now(),
		Type:            "ignored",
	}
	switch {
	case strings.HasPrefix(env.EventType, "TRANSACTION."):
		var tx transactionResource
		if err := json.Unmarshal(plain, &tx); err != nil {
			return nil, fmt.Errorf("wechat: decode transaction: %w", err)
		}
		out.ProviderSessionID = tx.OutTradeNo
		out.ProviderOrderID = tx.TransactionID
		out.OrderID = firstNonEmpty(tx.Attach, tx.OutTradeNo)
		out.Amount = tx.Amount.Total
		out.Currency = strings.ToUpper(tx.Amount.Currency)
		switch tx.TradeState {
		case "SUCCESS":
			out.Type = payments.CallbackPaid
		case "CLOSED", "PAYERROR", "REVOKED":
			out.Type = payments.CallbackFailed
		}
	case strings.HasPrefix(env.EventType, "REFUND."):
		var rf refundResource
		if err := json.Unmarshal(plain, &rf); err != nil {
			return nil, fmt.Errorf("wechat: decode refund: %w", err)
		}
		out.ProviderSessionID = rf.OutTradeNo
		out.ProviderOrderID = rf.TransactionID
		out.OrderID = rf.OutTradeNo
		out.Amount = rf.Amount.Refund
		out.Currency = strings.ToUpper(rf.Amount.Currency)
		if rf.RefundStatus == "SUCCESS" {
			out.Type = payments.CallbackRefunded
		}
	}
	return out, nil
}

func decryptResource(apiV3Key string, res notifyResource) ([]byte, error) {
	if res.Algorithm != "" && res.Algorithm != "AEAD_AES_256_GCM" {
		return nil, fmt.Errorf("unsupported algorithm %s", res.Algorithm)
	}
	raw, err := base64.StdEncoding.DecodeString(res.Ciphertext)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher([]byte(apiV3Key))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, []byte(res.Nonce), raw, []byte(res.AssociatedData))
}

// Refund 调用国内退款 API（金额最小单位 fen，bigint 直传）。
func (a *Adapter) Refund(ctx context.Context, in payments.RefundInput) (*payments.RefundResult, error) {
	if !a.configured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	if in.ProviderOrderID == "" && in.OrderID == "" {
		return nil, fmt.Errorf("wechat: refund requires transaction id or order id")
	}
	refund := in.Amount
	if refund <= 0 {
		refund = in.OrderAmount
	}
	total := in.OrderAmount
	if total <= 0 {
		total = refund
	}
	if refund <= 0 || total <= 0 {
		return nil, fmt.Errorf("wechat: refund amount must be a positive integer in fen")
	}
	body := refundRequest{
		OutRefundNo: refundOutNo(in),
		NotifyURL:   a.cfg.NotifyURL,
		Amount: refundAmount{
			Refund:   refund,
			Total:    total,
			Currency: "CNY",
		},
	}
	if in.ProviderOrderID != "" {
		body.TransactionID = in.ProviderOrderID
	} else {
		body.OutTradeNo = in.OrderID
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var out refundResponse
	if err := a.do(ctx, http.MethodPost, refundPath, raw, &out); err != nil {
		return nil, err
	}
	return &payments.RefundResult{
		RefundID:  firstNonEmpty(out.RefundID, out.OutRefundNo),
		Succeeded: out.Status == "SUCCESS",
	}, nil
}

type refundRequest struct {
	TransactionID string       `json:"transaction_id,omitempty"`
	OutTradeNo    string       `json:"out_trade_no,omitempty"`
	OutRefundNo   string       `json:"out_refund_no"`
	NotifyURL     string       `json:"notify_url,omitempty"`
	Amount        refundAmount `json:"amount"`
}

type refundAmount struct {
	Refund   int64  `json:"refund"`
	Total    int64  `json:"total"`
	Currency string `json:"currency"`
}

type refundResponse struct {
	RefundID    string `json:"refund_id"`
	OutRefundNo string `json:"out_refund_no"`
	Status      string `json:"status"`
}

func refundOutNo(in payments.RefundInput) string {
	if in.IdempotencyKey != "" {
		// 微信 out_refund_no 限 64 字符；幂等键可能含冒号。
		s := strings.ReplaceAll(in.IdempotencyKey, ":", "")
		if len(s) > 64 {
			s = s[:64]
		}
		return s
	}
	return in.OrderID
}

// CallbackAck 微信支付 APIv3 JSON 回执。
func (a *Adapter) CallbackAck(success bool) (int, string, []byte) {
	if success {
		return http.StatusOK, "application/json", []byte(`{"code":"SUCCESS"}`)
	}
	return http.StatusInternalServerError, "application/json", []byte(`{"code":"FAIL"}`)
}

func (a *Adapter) do(ctx context.Context, method, path string, body []byte, out any) error {
	url := a.cfg.APIBaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	ts := strconv.FormatInt(a.now().Unix(), 10)
	nonce := a.nonce()
	sign, err := a.signRequest(method, path, ts, nonce, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		`WECHATPAY2-SHA256-RSA2048 mchid="%s",nonce_str="%s",timestamp="%s",serial_no="%s",signature="%s"`,
		a.cfg.MchID, nonce, ts, a.cfg.MerchantSerialNo, sign))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("wechat: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCallbackBody))
	if err != nil {
		return fmt.Errorf("wechat: read response %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &payments.ProviderError{Provider: providerName, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("wechat: decode response %s: %w", path, err)
	}
	return nil
}

func (a *Adapter) signRequest(method, path, timestamp, nonce string, body []byte) (string, error) {
	if a.priv == nil {
		return "", payments.ErrProviderNotConfigured(providerName)
	}
	message := method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + string(body) + "\n"
	sum := sha256.Sum256([]byte(message))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKey(pemData string) (*rsa.PrivateKey, error) {
	pemData = strings.TrimSpace(pemData)
	if pemData == "" {
		return nil, fmt.Errorf("empty key")
	}
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("invalid pem")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("not rsa")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func parseCertificate(pemData string) (*x509.Certificate, error) {
	pemData = strings.TrimSpace(pemData)
	if pemData == "" {
		return nil, fmt.Errorf("empty cert")
	}
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("invalid pem")
	}
	return x509.ParseCertificate(block.Bytes)
}

func randomNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var (
	_ payments.PaymentProvider = (*Adapter)(nil)
	_ payments.CallbackAcker   = (*Adapter)(nil)
)
