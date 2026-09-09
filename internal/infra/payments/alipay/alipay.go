// Package alipay 是 PaymentProvider 端口的支付宝适配器（v3 设计 §1.1/§1.2）：
// alipay.trade.precreate 下单 + 异步 notify RSA2 验签归一化为 CallbackEvent；
// 回执纯文本 success/fail。
package alipay

import (
	"context"
	"crypto"
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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
)

const (
	DefaultGatewayURL = "https://openapi.alipay.com/gateway.do"
	maxCallbackBody   = 1 << 20
	providerName      = payments.ProviderAlipay
	methodPrecreate   = "alipay.trade.precreate"
	methodRefund      = "alipay.trade.refund"
)

// Config 是支付宝适配器配置（secret 一律来自环境变量）。
type Config struct {
	AppID           string
	AppPrivateKey   string // PEM（PKCS1 或 PKCS8）
	AlipayPublicKey string // PEM 或裸 base64
	NotifyURL       string
	GatewayURL      string
}

// Adapter 实现 payments.PaymentProvider 与 CallbackAcker。
type Adapter struct {
	cfg  Config
	priv *rsa.PrivateKey
	pub  *rsa.PublicKey
	now  func() time.Time
	http *http.Client
}

// New 构造适配器。凭据缺失时构造不失败，操作 fail-closed。
func New(cfg Config) *Adapter {
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = DefaultGatewayURL
	}
	a := &Adapter{
		cfg:  cfg,
		now:  time.Now,
		http: &http.Client{Timeout: 30 * time.Second},
	}
	if key, err := parseRSAPrivateKey(cfg.AppPrivateKey); err == nil {
		a.priv = key
	}
	if pub, err := parseRSAPublicKey(cfg.AlipayPublicKey); err == nil {
		a.pub = pub
	}
	return a
}

func (a *Adapter) Name() string { return providerName }

func (a *Adapter) configured() bool {
	return a.cfg.AppID != "" && a.priv != nil
}

func (a *Adapter) callbackConfigured() bool {
	return a.pub != nil
}

// CreatePayment 调用 alipay.trade.precreate，返回 qr_code。
func (a *Adapter) CreatePayment(ctx context.Context, in payments.CreatePaymentInput) (*payments.PaymentSession, error) {
	if !a.configured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	biz, err := json.Marshal(map[string]any{
		"out_trade_no":    in.OrderID,
		"total_amount":    fenToYuan(in.Amount),
		"subject":         in.Description,
		"timeout_express": timeoutExpress(in.ExpiresAt, a.now()),
	})
	if err != nil {
		return nil, err
	}
	var out precreateResponse
	if err := a.do(ctx, methodPrecreate, string(biz), &out); err != nil {
		return nil, err
	}
	if out.Response.Code != "10000" || out.Response.QRCode == "" {
		return nil, &payments.ProviderError{Provider: providerName, Status: 400}
	}
	return &payments.PaymentSession{SessionID: in.OrderID, PaymentURL: out.Response.QRCode}, nil
}

type precreateResponse struct {
	Response struct {
		Code       string `json:"code"`
		Msg        string `json:"msg"`
		OutTradeNo string `json:"out_trade_no"`
		QRCode     string `json:"qr_code"`
	} `json:"alipay_trade_precreate_response"`
}

func timeoutExpress(expiresAt, now time.Time) string {
	if expiresAt.IsZero() || !expiresAt.After(now) {
		return "15m"
	}
	d := expiresAt.Sub(now)
	m := int64(d / time.Minute)
	if m < 1 {
		m = 1
	}
	return strconv.FormatInt(m, 10) + "m"
}

// VerifyCallback 验签支付宝异步 notify（form-urlencoded），归一化为 CallbackEvent。
func (a *Adapter) VerifyCallback(ctx context.Context, headers http.Header, rawBody []byte) (*payments.CallbackEvent, error) {
	if len(rawBody) > maxCallbackBody {
		return nil, payments.ErrSignatureInvalid
	}
	if !a.callbackConfigured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	values, err := url.ParseQuery(string(rawBody))
	if err != nil {
		return nil, payments.ErrSignatureInvalid
	}
	if err := a.verifyNotify(values); err != nil {
		return nil, err
	}
	return a.normalize(values, rawBody)
}

func (a *Adapter) verifyNotify(values url.Values) error {
	sign := values.Get("sign")
	if sign == "" {
		return payments.ErrSignatureInvalid
	}
	sigBytes, err := base64.StdEncoding.DecodeString(sign)
	if err != nil {
		return payments.ErrSignatureInvalid
	}
	content := signContent(values)
	sum := sha256.Sum256([]byte(content))
	if err := rsa.VerifyPKCS1v15(a.pub, crypto.SHA256, sum[:], sigBytes); err != nil {
		return payments.ErrSignatureInvalid
	}
	return nil
}

func signContent(values url.Values) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		if k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		v := values.Get(k)
		if v == "" {
			continue
		}
		if i > 0 && b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v)
	}
	return b.String()
}

func (a *Adapter) normalize(values url.Values, rawBody []byte) (*payments.CallbackEvent, error) {
	notifyID := values.Get("notify_id")
	if notifyID == "" {
		notifyID = values.Get("trade_no")
	}
	if notifyID == "" {
		return nil, fmt.Errorf("alipay: notify missing notify_id")
	}
	amount, err := yuanToFen(values.Get("total_amount"))
	if err != nil {
		amount, err = yuanToFen(values.Get("receipt_amount"))
	}
	if err != nil {
		amount = 0
	}
	out := &payments.CallbackEvent{
		Provider:          providerName,
		ProviderEventID:   notifyID,
		ProviderSessionID: values.Get("out_trade_no"),
		ProviderOrderID:   values.Get("trade_no"),
		OrderID:           values.Get("out_trade_no"),
		Amount:            amount,
		Currency:          "CNY",
		Raw:               append([]byte(nil), rawBody...),
		ReceivedAt:        a.now(),
		Type:              "ignored",
	}
	status := values.Get("trade_status")
	switch status {
	case "TRADE_SUCCESS", "TRADE_FINISHED":
		out.Type = payments.CallbackPaid
	case "TRADE_CLOSED":
		out.Type = payments.CallbackFailed
	}
	if values.Get("gmt_refund") != "" || values.Get("refund_status") == "REFUND_SUCCESS" {
		if refund, err := yuanToFen(values.Get("refund_fee")); err == nil && refund > 0 {
			out.Amount = refund
		}
		out.Type = payments.CallbackRefunded
	}
	return out, nil
}

// Refund 调用 alipay.trade.refund。金额 fen→元字符串（整数运算，禁止 float）。
func (a *Adapter) Refund(ctx context.Context, in payments.RefundInput) (*payments.RefundResult, error) {
	if !a.configured() {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	refund := in.Amount
	if refund <= 0 {
		refund = in.OrderAmount
	}
	if refund <= 0 {
		return nil, fmt.Errorf("alipay: refund amount must be a positive integer in fen")
	}
	bizMap := map[string]any{
		"refund_amount":  fenToYuan(refund),
		"out_request_no": firstNonEmpty(in.IdempotencyKey, in.OrderID),
	}
	if in.ProviderOrderID != "" {
		bizMap["trade_no"] = in.ProviderOrderID
	} else {
		bizMap["out_trade_no"] = in.OrderID
	}
	biz, err := json.Marshal(bizMap)
	if err != nil {
		return nil, err
	}
	var out refundAPIResponse
	if err := a.do(ctx, methodRefund, string(biz), &out); err != nil {
		return nil, err
	}
	ok := out.Response.Code == "10000"
	return &payments.RefundResult{
		RefundID:  firstNonEmpty(out.Response.TradeNo, in.IdempotencyKey),
		Succeeded: ok,
	}, nil
}

type refundAPIResponse struct {
	Response struct {
		Code    string `json:"code"`
		Msg     string `json:"msg"`
		TradeNo string `json:"trade_no"`
	} `json:"alipay_trade_refund_response"`
}

// CallbackAck 支付宝约定纯文本 success / fail。
func (a *Adapter) CallbackAck(success bool) (int, string, []byte) {
	if success {
		return http.StatusOK, "text/plain; charset=utf-8", []byte("success")
	}
	return http.StatusInternalServerError, "text/plain; charset=utf-8", []byte("fail")
}

func (a *Adapter) do(ctx context.Context, method, bizContent string, out any) error {
	params := map[string]string{
		"app_id":      a.cfg.AppID,
		"method":      method,
		"format":      "JSON",
		"charset":     "utf-8",
		"sign_type":   "RSA2",
		"timestamp":   a.now().Format("2006-01-02 15:04:05"),
		"version":     "1.0",
		"biz_content": bizContent,
	}
	if a.cfg.NotifyURL != "" {
		params["notify_url"] = a.cfg.NotifyURL
	}
	sign, err := a.signParams(params)
	if err != nil {
		return err
	}
	params["sign"] = sign
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.GatewayURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded;charset=utf-8")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("alipay: request %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCallbackBody))
	if err != nil {
		return fmt.Errorf("alipay: read response %s: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &payments.ProviderError{Provider: providerName, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("alipay: decode response %s: %w", method, err)
	}
	return nil
}

func (a *Adapter) signParams(params map[string]string) (string, error) {
	if a.priv == nil {
		return "", payments.ErrProviderNotConfigured(providerName)
	}
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if k == "sign" || v == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(params[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// fenToYuan 把最小单位（分）转支付宝元字符串，只用整数运算。
func fenToYuan(fen int64) string {
	neg := fen < 0
	if neg {
		fen = -fen
	}
	s := strconv.FormatInt(fen/100, 10) + "." + fmt.Sprintf("%02d", fen%100)
	if neg {
		return "-" + s
	}
	return s
}

// yuanToFen 把支付宝元字符串转分，拒绝超过 2 位小数，禁止 float。
func yuanToFen(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	whole, frac, ok := strings.Cut(s, ".")
	if !ok {
		n, err := strconv.ParseInt(whole, 10, 64)
		if err != nil {
			return 0, err
		}
		if neg {
			n = -n
		}
		return n * 100, nil
	}
	if whole == "" {
		whole = "0"
	}
	switch len(frac) {
	case 0:
		frac = "00"
	case 1:
		frac += "0"
	case 2:
	default:
		return 0, fmt.Errorf("alipay: amount has more than 2 decimal places")
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, err
	}
	n := w*100 + f
	if neg {
		n = -n
	}
	return n, nil
}

func parseRSAPrivateKey(pemData string) (*rsa.PrivateKey, error) {
	pemData = strings.TrimSpace(pemData)
	if pemData == "" {
		return nil, fmt.Errorf("empty key")
	}
	if !strings.Contains(pemData, "BEGIN") {
		pemData = "-----BEGIN RSA PRIVATE KEY-----\n" + pemData + "\n-----END RSA PRIVATE KEY-----"
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

func parseRSAPublicKey(pemData string) (*rsa.PublicKey, error) {
	pemData = strings.TrimSpace(pemData)
	if pemData == "" {
		return nil, fmt.Errorf("empty key")
	}
	if !strings.Contains(pemData, "BEGIN") {
		pemData = "-----BEGIN PUBLIC KEY-----\n" + pemData + "\n-----END PUBLIC KEY-----"
	}
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("invalid pem")
	}
	if pub, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if rsaPub, ok := pub.(*rsa.PublicKey); ok {
			return rsaPub, nil
		}
		return nil, fmt.Errorf("not rsa")
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		if rsaPub, ok := cert.PublicKey.(*rsa.PublicKey); ok {
			return rsaPub, nil
		}
	}
	return x509.ParsePKCS1PublicKey(block.Bytes)
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
