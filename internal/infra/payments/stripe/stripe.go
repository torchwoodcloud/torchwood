// Package stripe 是 PaymentProvider 端口的 Stripe 适配器（v3 设计 §1.1/§1.2）：
// 下单走 Checkout Session，回调验签走 Stripe-Signature（HMAC-SHA256），
// 渠道差异全部收敛在本包，use-case 只见归一化 CallbackEvent。
package stripe

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
)

const (
	// DefaultAPIBaseURL 是 Stripe REST API 地址（测试可注入 httptest 地址）。
	DefaultAPIBaseURL = "https://api.stripe.com"
	// signatureHeader 是 webhook 签名头：t=<unix>,v1=<hex hmac>。
	signatureHeader = "Stripe-Signature"
	// defaultTolerance 是签名时间戳容忍窗口（对齐 stripe-go 默认 5min）。
	defaultTolerance = 5 * time.Minute
	// maxCallbackBody 限制回调体大小（Stripe webhook 一般 < 64KiB）。
	maxCallbackBody = 1 << 20
	// providerName 对齐 payments.ProviderStripe。
	providerName = payments.ProviderStripe
)

// Config 是 Stripe 适配器配置（secret 一律来自环境变量，不进 config.yaml）。
type Config struct {
	SecretKey     string // sk_...（TORCHWOOD_PAYMENTS_STRIPE_SECRET_KEY）
	WebhookSecret string // whsec_...（TORCHWOOD_PAYMENTS_STRIPE_WEBHOOK_SECRET）
	APIBaseURL    string // 默认 https://api.stripe.com；测试注入 httptest
}

// Adapter 实现 payments.PaymentProvider。
type Adapter struct {
	cfg       Config
	client    *http.Client
	tolerance time.Duration
	now       func() time.Time
}

// New 构造 Stripe 适配器。secret 未配置时构造不失败（服务可启动），
// 但 CreatePayment / VerifyCallback 一律 fail-closed（未配置即拒绝）。
func New(cfg Config) *Adapter {
	if cfg.APIBaseURL == "" {
		cfg.APIBaseURL = DefaultAPIBaseURL
	}
	return &Adapter{
		cfg:       cfg,
		client:    &http.Client{Timeout: 30 * time.Second},
		tolerance: defaultTolerance,
		now:       time.Now,
	}
}

func (a *Adapter) Name() string { return providerName }

//nolint:unused // configured 报告渠道是否已配置凭据（预留，未接入调用方）。
func (a *Adapter) configured() bool {
	return a.cfg.SecretKey != "" && a.cfg.WebhookSecret != ""
}

// CreatePayment 创建 Checkout Session（设计 §1.2：Stripe 下单渠道）。
func (a *Adapter) CreatePayment(ctx context.Context, in payments.CreatePaymentInput) (*payments.PaymentSession, error) {
	if a.cfg.SecretKey == "" {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	if in.ProjectID == "" {
		// metadata[project_id] 必写（设计 §9.2）：它是 K21 区分「早到 webhook
		// （503 重试）」与「他人账号噪音（200 丢弃）」的依据，缺失会吞掉早到事件。
		return nil, payments.ErrMissingProjectMetadata
	}
	form := url.Values{}
	form.Set("mode", "payment")
	form.Set("client_reference_id", in.OrderID)
	form.Set("metadata[order_id]", in.OrderID)
	form.Set("metadata[project_id]", in.ProjectID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("line_items[0][price_data][currency]", strings.ToLower(in.Currency))
	// 金额最小货币单位（bigint）直接透传，任何浮点换算都不允许。
	form.Set("line_items[0][price_data][unit_amount]", strconv.FormatInt(in.Amount, 10))
	form.Set("line_items[0][price_data][product_data][name]", in.Description)
	if !in.ExpiresAt.IsZero() {
		form.Set("expires_at", strconv.FormatInt(in.ExpiresAt.Unix(), 10))
	}
	if in.SuccessURL != "" {
		form.Set("success_url", in.SuccessURL)
	}
	if in.CancelURL != "" {
		form.Set("cancel_url", in.CancelURL)
	}

	var out checkoutSessionResponse
	reqID := in.IdempotencyKey
	if reqID == "" {
		reqID = in.OrderID
	}
	if err := a.do(ctx, http.MethodPost, "/v1/checkout/sessions", form, reqID, &out); err != nil {
		return nil, err
	}
	if out.ID == "" || out.URL == "" {
		return nil, fmt.Errorf("stripe: checkout session response missing id/url")
	}
	return &payments.PaymentSession{SessionID: out.ID, PaymentURL: out.URL, ProviderOrderID: out.PaymentIntent}, nil
}

// checkoutSessionResponse 是 Checkout Session 的最小响应形状。
type checkoutSessionResponse struct {
	ID            string `json:"id"`
	URL           string `json:"url"`
	PaymentIntent string `json:"payment_intent"`
}

// webhookEvent 是 Stripe webhook 事件的形状（只取归一化所需字段）。
type webhookEvent struct {
	ID      string           `json:"id"`
	Type    string           `json:"type"`
	Created int64            `json:"created"`
	Data    webhookEventData `json:"data"`
}

type webhookEventData struct {
	Object json.RawMessage `json:"object"`
}

// checkoutSessionObject 是 checkout.session.* 事件载荷。
type checkoutSessionObject struct {
	ID                string            `json:"id"`
	Mode              string            `json:"mode"`
	PaymentIntent     string            `json:"payment_intent"`
	Subscription      string            `json:"subscription"`
	ClientReferenceID string            `json:"client_reference_id"`
	PaymentStatus     string            `json:"payment_status"`
	AmountTotal       int64             `json:"amount_total"`
	Currency          string            `json:"currency"`
	Metadata          map[string]string `json:"metadata"`
}

// stripeSubscriptionObject 是 customer.subscription.* 事件载荷。
type stripeSubscriptionObject struct {
	ID                 string            `json:"id"`
	Status             string            `json:"status"`
	CurrentPeriodStart int64             `json:"current_period_start"`
	CurrentPeriodEnd   int64             `json:"current_period_end"`
	CancelAtPeriodEnd  bool              `json:"cancel_at_period_end"`
	Metadata           map[string]string `json:"metadata"`
	Items              struct {
		Data []struct {
			CurrentPeriodStart int64 `json:"current_period_start"`
			CurrentPeriodEnd   int64 `json:"current_period_end"`
		} `json:"data"`
	} `json:"items"`
}

// stripeInvoiceObject 是 invoice.paid / invoice.payment_failed 载荷。
type stripeInvoiceObject struct {
	ID            string `json:"id"`
	Subscription  string `json:"subscription"`
	BillingReason string `json:"billing_reason"`
	AmountPaid    int64  `json:"amount_paid"`
	AmountDue     int64  `json:"amount_due"`
	Currency      string `json:"currency"`
	PeriodStart   int64  `json:"period_start"`
	PeriodEnd     int64  `json:"period_end"`
	Status        string `json:"status"`
}

// chargeObject 是 charge.refunded 事件载荷。
type chargeObject struct {
	PaymentIntent    string `json:"payment_intent"`
	AmountRefunded   int64  `json:"amount_refunded"`
	Amount           int64  `json:"amount"`
	CurrencyRefunded string `json:"currency"`
}

// paymentIntentObject 是 payment_intent.payment_failed 事件载荷。
type paymentIntentObject struct {
	ID       string            `json:"id"`
	Amount   int64             `json:"amount"`
	Currency string            `json:"currency"`
	Metadata map[string]string `json:"metadata"`
}

// VerifyCallback 验签并归一化 Stripe webhook（设计 §1.4：原始 body 直读，
// 验签失败 ErrSignatureInvalid → 401，不落库）。
func (a *Adapter) VerifyCallback(ctx context.Context, headers http.Header, rawBody []byte) (*payments.CallbackEvent, error) {
	if len(rawBody) > maxCallbackBody {
		return nil, payments.ErrSignatureInvalid
	}
	if err := a.verifySignature(headers, rawBody); err != nil {
		return nil, err
	}
	return a.normalize(rawBody)
}

// verifySignature 校验 Stripe-Signature：v1 = HMAC-SHA256(webhook_secret,
// "t" + "." + rawBody)，时间戳超容忍窗口拒绝（防重放）。
func (a *Adapter) verifySignature(headers http.Header, rawBody []byte) error {
	if a.cfg.WebhookSecret == "" {
		return payments.ErrProviderNotConfigured(providerName)
	}
	sigHeader := headers.Get(signatureHeader)
	if sigHeader == "" {
		return payments.ErrSignatureInvalid
	}
	var timestamp, provided string
	for _, part := range strings.Split(sigHeader, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			timestamp = v
		case "v1":
			provided = v
		}
	}
	if timestamp == "" || provided == "" {
		return payments.ErrSignatureInvalid
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return payments.ErrSignatureInvalid
	}
	if a.tolerance > 0 && a.now().Sub(time.Unix(ts, 0)) > a.tolerance {
		return payments.ErrSignatureInvalid
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.WebhookSecret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	expect := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expect), []byte(provided)) {
		return payments.ErrSignatureInvalid
	}
	return nil
}

// normalize 把验签通过的 Stripe 事件归一化为 CallbackEvent（渠道差异
// 收敛点，设计 §1.1）。
func (a *Adapter) normalize(rawBody []byte) (*payments.CallbackEvent, error) {
	var ev webhookEvent
	if err := json.Unmarshal(rawBody, &ev); err != nil {
		return nil, fmt.Errorf("stripe: decode webhook: %w", err)
	}
	if ev.ID == "" || ev.Type == "" {
		return nil, fmt.Errorf("stripe: webhook missing id/type")
	}
	out := &payments.CallbackEvent{
		Provider:        providerName,
		ProviderEventID: ev.ID,
		Raw:             append([]byte(nil), rawBody...),
		ReceivedAt:      a.now(),
	}
	switch {
	case ev.Type == "checkout.session.completed" || ev.Type == "checkout.session.async_payment_succeeded":
		var obj checkoutSessionObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode checkout session: %w", err)
		}
		out.ProviderSessionID = obj.ID
		out.ProviderOrderID = obj.PaymentIntent
		out.CheckoutMode = obj.Mode
		out.Amount = obj.AmountTotal
		out.Currency = strings.ToUpper(obj.Currency)
		out.MetadataProjectID = obj.Metadata["project_id"]
		if obj.Mode == "subscription" {
			out.ProviderSubID = obj.Subscription
			out.LocalSubscriptionID = firstNonEmpty(obj.Metadata["subscription_id"], obj.ClientReferenceID)
			out.Type = payments.CallbackSubscriptionUpdated
			break
		}
		out.OrderID = firstNonEmpty(obj.ClientReferenceID, obj.Metadata["order_id"])
		if obj.PaymentStatus == "paid" {
			out.Type = payments.CallbackPaid
		} else {
			// async_payment_succeeded 之外的非终态：忽略（不驱动状态机）。
			out.Type = "ignored"
		}
	case ev.Type == "checkout.session.async_payment_failed" || ev.Type == "checkout.session.expired":
		var obj checkoutSessionObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode checkout session: %w", err)
		}
		out.ProviderSessionID = obj.ID
		out.ProviderOrderID = obj.PaymentIntent
		out.OrderID = firstNonEmpty(obj.ClientReferenceID, obj.Metadata["order_id"])
		out.Amount = obj.AmountTotal
		out.Currency = strings.ToUpper(obj.Currency)
		out.MetadataProjectID = obj.Metadata["project_id"]
		out.Type = payments.CallbackFailed
	case ev.Type == "payment_intent.payment_failed":
		var obj paymentIntentObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode payment intent: %w", err)
		}
		out.ProviderOrderID = obj.ID
		out.OrderID = obj.Metadata["order_id"]
		out.Amount = obj.Amount
		out.Currency = strings.ToUpper(obj.Currency)
		out.MetadataProjectID = obj.Metadata["project_id"]
		out.Type = payments.CallbackFailed
	case ev.Type == "charge.refunded":
		var obj chargeObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode charge: %w", err)
		}
		out.ProviderOrderID = obj.PaymentIntent
		out.Amount = obj.AmountRefunded
		out.Currency = strings.ToUpper(obj.CurrencyRefunded)
		out.Type = payments.CallbackRefunded
	case strings.HasPrefix(ev.Type, "customer.subscription."):
		var obj stripeSubscriptionObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode subscription: %w", err)
		}
		fillSubscriptionEvent(out, &obj)
		switch ev.Type {
		case "customer.subscription.deleted":
			out.Type = payments.CallbackSubscriptionCanceled
		default:
			out.Type = mapStripeSubStatus(obj.Status)
		}
	case ev.Type == "invoice.paid":
		var obj stripeInvoiceObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode invoice: %w", err)
		}
		fillInvoiceEvent(out, &obj)
		if obj.BillingReason == "subscription_cycle" {
			out.Type = payments.CallbackSubscriptionRenewed
		} else {
			out.Type = payments.CallbackSubscriptionActivated
		}
	case ev.Type == "invoice.payment_failed":
		var obj stripeInvoiceObject
		if err := json.Unmarshal(ev.Data.Object, &obj); err != nil {
			return nil, fmt.Errorf("stripe: decode invoice: %w", err)
		}
		fillInvoiceEvent(out, &obj)
		out.Type = payments.CallbackSubscriptionPastDue
	default:
		out.Type = "ignored"
	}
	return out, nil
}

func fillSubscriptionEvent(out *payments.CallbackEvent, obj *stripeSubscriptionObject) {
	out.ProviderSubID = obj.ID
	out.LocalSubscriptionID = obj.Metadata["subscription_id"]
	out.MetadataProjectID = obj.Metadata["project_id"]
	out.HostedStatus = obj.Status
	out.CancelAtPeriodEnd = obj.CancelAtPeriodEnd
	start, end := obj.CurrentPeriodStart, obj.CurrentPeriodEnd
	if start == 0 && len(obj.Items.Data) > 0 {
		start = obj.Items.Data[0].CurrentPeriodStart
		end = obj.Items.Data[0].CurrentPeriodEnd
	}
	if start > 0 {
		out.PeriodStart = time.Unix(start, 0).UTC()
	}
	if end > 0 {
		out.PeriodEnd = time.Unix(end, 0).UTC()
	}
}

func fillInvoiceEvent(out *payments.CallbackEvent, obj *stripeInvoiceObject) {
	out.ProviderSubID = obj.Subscription
	out.BillingReason = obj.BillingReason
	out.Amount = obj.AmountPaid
	if out.Amount == 0 {
		out.Amount = obj.AmountDue
	}
	out.Currency = strings.ToUpper(obj.Currency)
	if obj.PeriodStart > 0 {
		out.PeriodStart = time.Unix(obj.PeriodStart, 0).UTC()
	}
	if obj.PeriodEnd > 0 {
		out.PeriodEnd = time.Unix(obj.PeriodEnd, 0).UTC()
	}
}

func mapStripeSubStatus(status string) string {
	switch status {
	case "trialing":
		return payments.CallbackSubscriptionUpdated
	case "active":
		return payments.CallbackSubscriptionUpdated
	case "past_due", "unpaid":
		return payments.CallbackSubscriptionPastDue
	case "canceled":
		return payments.CallbackSubscriptionCanceled
	case "incomplete_expired":
		return payments.CallbackSubscriptionExpired
	default:
		return payments.CallbackSubscriptionUpdated
	}
}

// Refund 对 PaymentIntent 发起退款（POST /v1/refunds）。
func (a *Adapter) Refund(ctx context.Context, in payments.RefundInput) (*payments.RefundResult, error) {
	if a.cfg.SecretKey == "" {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	if in.ProviderOrderID == "" {
		return nil, fmt.Errorf("stripe: refund requires provider order id (payment intent)")
	}
	form := url.Values{}
	form.Set("payment_intent", in.ProviderOrderID)
	if in.Amount > 0 {
		form.Set("amount", strconv.FormatInt(in.Amount, 10))
	}
	var out refundResponse
	if err := a.do(ctx, http.MethodPost, "/v1/refunds", form, in.IdempotencyKey, &out); err != nil {
		return nil, err
	}
	return &payments.RefundResult{
		RefundID:  out.ID,
		Succeeded: out.Status == "succeeded",
	}, nil
}

type refundResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// do 执行一次 Stripe REST 调用（form 编码 + Bearer 认证 + Idempotency-Key 头）。
func (a *Adapter) do(ctx context.Context, method, path string, form url.Values, idempotencyKey string, out any) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIBaseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.SecretKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("stripe: request %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCallbackBody))
	if err != nil {
		return fmt.Errorf("stripe: read response %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 渠道错误统一包装 domain ProviderError（use-case 映射 gRPC 状态，
		// 渠道原文不跨层透出）。
		return &payments.ProviderError{Provider: providerName, Status: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("stripe: decode response %s: %w", path, err)
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// CreateCheckout 创建 Stripe Billing Checkout（mode=subscription）。
func (a *Adapter) CreateCheckout(ctx context.Context, in domainsubs.HostedCheckoutInput) (*domainsubs.HostedCheckout, error) {
	if a.cfg.SecretKey == "" {
		return nil, payments.ErrProviderNotConfigured(providerName)
	}
	if in.PriceID == "" {
		return nil, fmt.Errorf("stripe: hosted checkout requires price_id")
	}
	if in.SuccessURL == "" || in.CancelURL == "" {
		return nil, fmt.Errorf("stripe: hosted checkout requires success_url and cancel_url")
	}
	form := url.Values{}
	form.Set("mode", "subscription")
	form.Set("client_reference_id", in.SubscriptionID)
	form.Set("metadata[subscription_id]", in.SubscriptionID)
	form.Set("metadata[project_id]", in.ProjectID)
	form.Set("subscription_data[metadata][subscription_id]", in.SubscriptionID)
	form.Set("subscription_data[metadata][project_id]", in.ProjectID)
	form.Set("line_items[0][price]", in.PriceID)
	form.Set("line_items[0][quantity]", "1")
	form.Set("success_url", in.SuccessURL)
	form.Set("cancel_url", in.CancelURL)

	var out checkoutSessionResponse
	reqID := in.IdempotencyKey
	if reqID == "" {
		reqID = in.SubscriptionID
	}
	if err := a.do(ctx, http.MethodPost, "/v1/checkout/sessions", form, reqID, &out); err != nil {
		return nil, err
	}
	if out.ID == "" || out.URL == "" {
		return nil, fmt.Errorf("stripe: subscription checkout response missing id/url")
	}
	return &domainsubs.HostedCheckout{SessionID: out.ID, PaymentURL: out.URL}, nil
}

// CancelAtPeriodEnd 通知 Stripe 期末取消（hosted Cancel）。
func (a *Adapter) CancelAtPeriodEnd(ctx context.Context, providerSubID string) error {
	if a.cfg.SecretKey == "" {
		return payments.ErrProviderNotConfigured(providerName)
	}
	if providerSubID == "" {
		return fmt.Errorf("stripe: cancel requires provider subscription id")
	}
	form := url.Values{}
	form.Set("cancel_at_period_end", "true")
	return a.do(ctx, http.MethodPost, "/v1/subscriptions/"+providerSubID, form, "cancel-at-end:"+providerSubID, nil)
}

// CancelNow 立即取消 Stripe 订阅（Server 强制 Cancel）。
func (a *Adapter) CancelNow(ctx context.Context, providerSubID string) error {
	if a.cfg.SecretKey == "" {
		return payments.ErrProviderNotConfigured(providerName)
	}
	if providerSubID == "" {
		return fmt.Errorf("stripe: cancel requires provider subscription id")
	}
	return a.do(ctx, http.MethodDelete, "/v1/subscriptions/"+providerSubID, nil, "cancel-now:"+providerSubID, nil)
}

var _ payments.PaymentProvider = (*Adapter)(nil)
var _ domainsubs.HostedBilling = (*Adapter)(nil)
