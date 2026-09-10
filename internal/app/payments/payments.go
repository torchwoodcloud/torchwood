// Package payments 是 v3 支付子域的 use-case 聚合（设计 §1）：
// 建单 / 查单（Client 面）、订单与回调管理（Server 面）、回调处理
// （serverhttp 面）、退款与人工履约、超时关单（worker 面）。
package payments

import (
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
)

const (
	// defaultOrderTTL 是建单默认有效期（Stripe Checkout Session 上限 24h）。
	defaultOrderTTL = 24 * time.Hour
	// maxOrderTTL 是允许的最大有效期。
	maxOrderTTL = 24 * time.Hour
	// minOrderTTL 是允许的最小有效期。
	minOrderTTL = time.Minute
	// defaultListLimit / maxListLimit 是订单列表分页约束。
	defaultListLimit = 25
	maxListLimit     = 100
	// closeExpiredBatch 是关单 worker 单轮处理上限。
	closeExpiredBatch = 500
)

// 指标（前缀 torchwood_，设计 §Observability；履约 lag 直方图待 PR2
// 真实发放落地后补充——本期履约与翻转同事务同步完成）。
var (
	paymentOrdersTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_payment_orders_total",
		Help: "Total payment order status transitions.",
	}, []string{"provider", "status"})
	paymentCallbackVerifyFailTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "torchwood_payment_callback_verify_fail_total",
		Help: "Total payment callback signature verification failures.",
	}, []string{"provider"})
)

func init() {
	prometheus.MustRegister(paymentOrdersTotal, paymentCallbackVerifyFailTotal)
}

// Payments 是支付子域 use-case 聚合。
type Payments struct {
	cfg          *config.AppConfig
	db           uow.Runner // 仅工作单元
	orders       domainpayments.OrderRepo
	callbacks    domainpayments.CallbackEventRepo
	fulfillments domainpayments.FulfillmentRepo
	fulfiller    domainpayments.Fulfiller
	providers    domainpayments.ProviderRegistry
	events       shared.EventPublisher
	subs         domainpayments.SubscriptionCallbackHandler // hosted 订阅 webhook（可空）
	projects     projects.Repository
	index        domainpayments.ProviderIndexRepo
	logger       *slog.Logger
	scanCursor   appshared.ProjectRotation // CloseExpiredOrders 轮转游标（tick 串行）
}

// NewPayments 构造 use-case 聚合（db 注入 uow.Runner 端口）。
func NewPayments(
	cfg *config.AppConfig,
	db uow.Runner,
	orders domainpayments.OrderRepo,
	callbacks domainpayments.CallbackEventRepo,
	fulfillments domainpayments.FulfillmentRepo,
	fulfiller domainpayments.Fulfiller,
	providers domainpayments.ProviderRegistry,
	events shared.EventPublisher,
	logger *slog.Logger,
	subs domainpayments.SubscriptionCallbackHandler,
	projectRepo projects.Repository,
	index domainpayments.ProviderIndexRepo,
) *Payments {
	return newPayments(cfg, db, orders, callbacks, fulfillments, fulfiller, providers, events, logger, subs, projectRepo, index)
}

// newPayments 允许单测注入内存 uow.Runner。
func newPayments(
	cfg *config.AppConfig,
	db uow.Runner,
	orders domainpayments.OrderRepo,
	callbacks domainpayments.CallbackEventRepo,
	fulfillments domainpayments.FulfillmentRepo,
	fulfiller domainpayments.Fulfiller,
	providers domainpayments.ProviderRegistry,
	events shared.EventPublisher,
	logger *slog.Logger,
	subs domainpayments.SubscriptionCallbackHandler,
	projectRepo projects.Repository,
	index domainpayments.ProviderIndexRepo,
) *Payments {
	if logger == nil {
		logger = slog.Default()
	}
	return &Payments{
		cfg:          cfg,
		db:           db,
		orders:       orders,
		callbacks:    callbacks,
		fulfillments: fulfillments,
		fulfiller:    fulfiller,
		providers:    providers,
		events:       events,
		subs:         subs,
		projects:     projectRepo,
		index:        index,
		logger:       logger,
	}
}

// newOrderID 生成订单 id（ULID，字典序即时间序，便于游标分页）。
func newOrderID() string {
	return idgen.ULID().String()
}
