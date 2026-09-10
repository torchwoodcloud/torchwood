// Package payments 提供支付渠道适配器装配：PaymentProvider 注册表
// （Stripe / 微信 / 支付宝 / iOS IAP）。
package payments

import (
	"fmt"
	"strings"

	"github.com/google/wire"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/alipay"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/iosiap"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/stripe"
	"github.com/torchwoodcloud/torchwood/internal/infra/payments/wechat"
	"github.com/torchwoodcloud/torchwood/pkg/config"
)

// ProviderSet 装配渠道注册表。
var ProviderSet = wire.NewSet(
	NewStripeAdapter,
	NewWeChatAdapter,
	NewAlipayAdapter,
	NewIosIapAdapter,
	ProvideRegistry,
	wire.Bind(new(domainpayments.ProviderRegistry), new(*Registry)),
	wire.Bind(new(domainsubs.HostedBilling), new(*stripe.Adapter)),
)

// NewStripeAdapter 从配置构造 Stripe 适配器（secret 走环境变量，
// 未配置时构造成功但操作 fail-closed）。
func NewStripeAdapter(cfg *config.AppConfig) *stripe.Adapter {
	stripeCfg := cfg.GetPayments().GetStripe()
	return stripe.New(stripe.Config{
		SecretKey:     stripeCfg.GetSecretKey(),
		WebhookSecret: stripeCfg.GetWebhookSecret(),
		APIBaseURL:    stripeCfg.GetApiBaseUrl(),
	})
}

// NewWeChatAdapter 从配置构造微信支付适配器。
func NewWeChatAdapter(cfg *config.AppConfig) *wechat.Adapter {
	w := cfg.GetPayments().GetWechat()
	return wechat.New(wechat.Config{
		MchID:              w.GetMchId(),
		AppID:              w.GetAppId(),
		APIV3Key:           w.GetApiV3Key(),
		MerchantSerialNo:   w.GetMerchantSerialNo(),
		MerchantPrivateKey: w.GetMerchantPrivateKey(),
		PlatformCert:       w.GetPlatformCert(),
		NotifyURL:          firstNonEmpty(w.GetNotifyUrl(), callbackURL(cfg, domainpayments.ProviderWeChat)),
		APIBaseURL:         w.GetApiBaseUrl(),
	})
}

// NewAlipayAdapter 从配置构造支付宝适配器。
func NewAlipayAdapter(cfg *config.AppConfig) *alipay.Adapter {
	a := cfg.GetPayments().GetAlipay()
	return alipay.New(alipay.Config{
		AppID:           a.GetAppId(),
		AppPrivateKey:   a.GetAppPrivateKey(),
		AlipayPublicKey: a.GetAlipayPublicKey(),
		NotifyURL:       firstNonEmpty(a.GetNotifyUrl(), callbackURL(cfg, domainpayments.ProviderAlipay)),
		GatewayURL:      a.GetGatewayUrl(),
	})
}

// NewIosIapAdapter 从配置构造 iOS IAP 适配器。
func NewIosIapAdapter(cfg *config.AppConfig) *iosiap.Adapter {
	i := cfg.GetPayments().GetIosIap()
	return iosiap.New(iosiap.Config{
		BundleID:         i.GetBundleId(),
		SharedSecret:     i.GetSharedSecret(),
		AppleRootCert:    i.GetAppleRootCert(),
		VerifyReceiptURL: i.GetVerifyReceiptUrl(),
		SandboxVerifyURL: i.GetSandboxVerifyUrl(),
	})
}

func callbackURL(cfg *config.AppConfig, provider string) string {
	base := strings.TrimRight(cfg.GetServer().GetHttp().GetPublicUrl(), "/")
	if base == "" {
		return ""
	}
	return base + "/v1/payments/callbacks/" + provider
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Registry 是渠道名 → PaymentProvider 的注册表。
type Registry struct {
	providers map[string]domainpayments.PaymentProvider
}

// NewRegistry 登记任意一组渠道（单测 / 组装）。
func NewRegistry(providers ...domainpayments.PaymentProvider) *Registry {
	m := make(map[string]domainpayments.PaymentProvider, len(providers))
	for _, p := range providers {
		if p == nil {
			continue
		}
		m[p.Name()] = p
	}
	return &Registry{providers: m}
}

// ProvideRegistry 是 Wire 入口（四渠道全部登记，未配置时 adapter 仍 fail-closed）。
func ProvideRegistry(
	stripeAdapter *stripe.Adapter,
	wechatAdapter *wechat.Adapter,
	alipayAdapter *alipay.Adapter,
	iosAdapter *iosiap.Adapter,
) *Registry {
	return NewRegistry(stripeAdapter, wechatAdapter, alipayAdapter, iosAdapter)
}

// Get 按渠道名取 provider；未注册渠道返回错误。
func (r *Registry) Get(name string) (domainpayments.PaymentProvider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("payments: unknown provider %q", name)
	}
	return p, nil
}

// Names 返回已注册渠道名。
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for name := range r.providers {
		out = append(out, name)
	}
	return out
}
