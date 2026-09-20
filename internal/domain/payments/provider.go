// Package payments 定义 v3 支付子域的领域模型与端口：订单状态机、
// 归一化回调事件与 PaymentProvider 渠道端口（设计 §1）。
package payments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// ProviderName 是受支持渠道名（设计 §1.2 渠道差异矩阵）。
const (
	ProviderStripe = "stripe"
	ProviderWeChat = "wechat"
	ProviderAlipay = "alipay"
	ProviderIOSIAP = "ios_iap"
)

// ErrUnsupported 表示渠道不支持该操作（如 iOS IAP 无 CreatePayment）。
var ErrUnsupported = errors.New("payments: operation unsupported by provider")

// ErrLegacyReceiptUnsupported 表示 legacy verifyReceipt 收据路径已显式下线：
// 该路径解析出的收据不携带价格（归一化 Amount 恒 0，应用层 fail-closed 下
// 永远无法结算，功能死路）且 BundleID 从未与配置比对。JWS transaction ID
// 是唯一受支持的验票入口。
var ErrLegacyReceiptUnsupported = errors.New("payments: legacy verifyReceipt receipts are not supported; use JWS transaction IDs")

// ErrSignatureInvalid 表示回调验签失败：调用方必须 401 且不落任何行
// （设计 §Security：验签是唯一信任根，不返回区分性错误）。
var ErrSignatureInvalid = errors.New("payments: callback signature invalid")

// ErrProviderIndexMiss 表示验签成功、携带本平台会写入的 ref，但
// public.provider_resource_index 未命中（早到 webhook）。HTTP 映射 503。
var ErrProviderIndexMiss = errors.New("payments: provider resource index miss")

// ErrReceiptBoundToOtherUser 表示 iOS receipt / transactionId 已绑定其他用户
// （设计 §Security 5：一份 receipt 绑一个 user，跨用户领取拒绝）。
var ErrReceiptBoundToOtherUser = errors.New("payments: receipt already bound to another user")

// ErrMissingProjectMetadata 表示渠道下单未携带 project_id（调用方缺陷）：
// metadata[project_id] 必写（设计 §9.2 / K21），缺失会使早到 webhook 被
// 误判为无关噪音吞掉。
var ErrMissingProjectMetadata = errors.New("payments: project_id metadata is required for payment creation")

// errNotConfigured 是渠道凭据未配置的统一错误（服务可启动，
// 相关操作 fail-closed）。
var errNotConfigured = errors.New("payments: provider not configured")

// ErrNotConfigured 报告 err 是否为「渠道未配置」。
func ErrNotConfigured(err error) bool { return errors.Is(err, errNotConfigured) }

// ErrProviderNotConfigured 构造「渠道未配置」错误（sentinel 未导出，
// 供 adapter 构造）。
func ErrProviderNotConfigured(provider string) error {
	return fmt.Errorf("%w: %s", errNotConfigured, provider)
}

// ProviderError 携带渠道侧调用失败（HTTP 状态等）；adapter 把非 2xx
// 渠道响应包装为本类型，use-case 据此映射 gRPC 状态（不透出渠道原文）。
type ProviderError struct {
	Provider string
	Status   int // 渠道 HTTP 状态码（0 = 未知）
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("payments: provider %s error: http %d", e.Provider, e.Status)
}

// AsProviderError 取出 *ProviderError（非该类型返回 nil）。
func AsProviderError(err error) *ProviderError {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// CreatePaymentInput 是下单入参（use-case 从订单实体组装）。
type CreatePaymentInput struct {
	OrderID     string // 本地订单 id，回填渠道 metadata / client_reference_id
	ProjectID   string // Stripe metadata[project_id]（K21 区分早到 vs 噪音）
	Amount      int64  // 最小货币单位（bigint，禁止 float）
	Currency    string // ISO-4217 三字母
	Description string // 收银台展示名
	ExpiresAt   time.Time
	SuccessURL  string
	CancelURL   string
	// IdempotencyKey 透传渠道幂等键（Stripe Idempotency-Key header）。
	IdempotencyKey string
}

// PaymentSession 是下单结果：客户端完成支付所需的载荷
// （Stripe → checkout URL；微信/支付宝 → 预下单参数/二维码串）。
type PaymentSession struct {
	SessionID       string // 渠道会话 id（Stripe Checkout Session cs_...）
	PaymentURL      string // 客户端跳转/拉起地址
	ProviderOrderID string // 渠道支付单（Stripe pi_，若下单时已知）
}

// CallbackEventType 是归一化回调类型（设计 §1.1）。subscription_* 由 PR3 消费。
const (
	CallbackPaid     = "paid"
	CallbackFailed   = "failed"
	CallbackRefunded = "refunded"

	CallbackSubscriptionUpdated   = "subscription_updated"
	CallbackSubscriptionActivated = "subscription_activated"
	CallbackSubscriptionRenewed   = "subscription_renewed"
	CallbackSubscriptionPastDue   = "subscription_past_due"
	CallbackSubscriptionCanceled  = "subscription_canceled"
	CallbackSubscriptionExpired   = "subscription_expired"
)

// IsSubscriptionEvent 报告归一化类型是否为订阅镜像事件（hosted webhook）。
func IsSubscriptionEvent(t string) bool {
	switch t {
	case CallbackSubscriptionUpdated, CallbackSubscriptionActivated,
		CallbackSubscriptionRenewed, CallbackSubscriptionPastDue,
		CallbackSubscriptionCanceled, CallbackSubscriptionExpired:
		return true
	}
	return false
}

// CallbackEvent 是验签后的归一化渠道异步通知：
// {Provider, ProviderEventID, ProviderOrderID, Type, Amount, Currency, Raw}
// （设计 §1.1）。ProviderSessionID 为 Stripe Checkout Session 等
// 「我们下单时拿到的会话标识」，用于回调定位本地订单。
type CallbackEvent struct {
	Provider          string
	ProviderEventID   string // 幂等锚点二：(provider, provider_event_id) 唯一
	ProviderSessionID string
	ProviderOrderID   string
	Type              string // paid | failed | refunded | subscription_*
	Amount            int64
	Currency          string
	OrderID           string // 渠道 metadata 回传的本地订单 id（若有）
	Raw               []byte
	ReceivedAt        time.Time

	// hosted 订阅镜像字段（一次性支付事件保持零值）。
	ProviderSubID       string
	LocalSubscriptionID string
	PeriodStart         time.Time
	PeriodEnd           time.Time
	CancelAtPeriodEnd   bool
	HostedStatus        string
	BillingReason       string
	CheckoutMode        string

	// MetadataProjectID 是 Stripe metadata[project_id]（K21 hasPlatformRef）。
	MetadataProjectID string
}

// SubscriptionCallbackHandler 处理 hosted 订阅 webhook（PR3）：
// 与 payment_callback_events 插入同一工作单元（调用方 uow.Run）。
// nil 时订阅事件仅登记不驱动。
type SubscriptionCallbackHandler interface {
	HandleHostedCallback(ctx context.Context, event *CallbackEvent) error
}

// RefundInput 是退款入参。
type RefundInput struct {
	OrderID         string
	ProviderOrderID string // 渠道支付单（Stripe PaymentIntent / 微信 transaction_id）
	Amount          int64  // 0 = 全额
	OrderAmount     int64  // 原单金额（微信/支付宝退款报文需要 total；bigint 最小单位）
	IdempotencyKey  string
}

// RefundResult 是退款结果：Succeeded=true 时订单可直接翻 refunded，
// 否则订单停在 refunding，等渠道回调确认（设计 §1.3）。
type RefundResult struct {
	RefundID  string
	Succeeded bool
}

// PaymentProvider 是支付渠道端口（domain）：use-case 只见归一化接口，
// 渠道差异全部收敛在 infra adapter（设计 §1.1）。
type PaymentProvider interface {
	// Name 返回 ProviderName 常量之一。
	Name() string
	// CreatePayment 下单。iOS IAP 不实现（返回 ErrUnsupported，走验票路径）。
	CreatePayment(ctx context.Context, in CreatePaymentInput) (*PaymentSession, error)
	// VerifyCallback 验签并归一化渠道异步通知；验签失败返回 ErrSignatureInvalid。
	VerifyCallback(ctx context.Context, headers http.Header, rawBody []byte) (*CallbackEvent, error)
	// Refund 发起退款。
	Refund(ctx context.Context, in RefundInput) (*RefundResult, error)
}

// ReceiptVerifier 是 iOS IAP 专用端口（PR4 实现）。
type ReceiptVerifier interface {
	VerifyReceipt(ctx context.Context, in VerifyReceiptInput) (*VerifiedPurchase, error)
}

// CallbackAcker 是渠道回调 HTTP 回执（可选接口，adapter 实现）：
// 验签失败不走本方法（handler 固定 401 空 body）；处理成功/可重试失败
// 才按渠道约定写 JSON / XML / 纯文本，避免渠道重复推（设计 §1.4）。
type CallbackAcker interface {
	CallbackAck(success bool) (status int, contentType string, body []byte)
}

// ProviderRegistry 按渠道名解析 PaymentProvider（infra 装配实现；
// 回调入口与建单路径经它路由，PR4 泛化多渠道）。
type ProviderRegistry interface {
	Get(name string) (PaymentProvider, error)
}

// VerifyReceiptInput 是 iOS 验票入参（Client VerifyReceipt）。
type VerifyReceiptInput struct {
	Receipt   []byte // StoreKit receipt（base64 PKCS7）或 StoreKit 2 JWS
	UserID    string
	ProjectID string
	OrderID   string // 本地订单 id（created 态 ios_iap 单）
}

// VerifiedPurchase 是 Apple 验票通过后的归一化购买。
type VerifiedPurchase struct {
	TransactionID         string
	OriginalTransactionID string
	ProductID             string
	Amount                int64 // 最小货币单位
	Currency              string
	PaidAt                time.Time
	Environment           string // Sandbox | Production
	BundleID              string
}
