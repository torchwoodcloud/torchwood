package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
)

const (
	// paymentCloserInterval 是超时未付关单的扫描间隔（每分钟一次）。
	paymentCloserInterval = time.Minute
	// paymentCloserTimeout 是单轮关单扫描的预算上限（有界批量 UPDATE；
	// 短于扫描周期，对齐 cron 领取的 50s/1min 比例；WithoutCancel 使停机
	// 不腰斩在途轮次）。
	paymentCloserTimeout = 50 * time.Second
)

// PaymentCloser 周期把 created/paying 且超过 expires_at 的订单翻 closed
// （v3 设计 §1.3；closed 不在 §5.1 事件目录，不发 outbox 事件）。
type PaymentCloser struct {
	payments *apppayments.Payments
	logger   *slog.Logger
	loop     *Periodic
}

// NewPaymentCloser creates the expired-order closing service.
func NewPaymentCloser(payments *apppayments.Payments, logger *slog.Logger) *PaymentCloser {
	if logger == nil {
		logger = slog.Default()
	}
	c := &PaymentCloser{payments: payments, logger: logger}
	c.loop = NewPeriodic("payment-closer", paymentCloserInterval, paymentCloserTimeout, c.runOnce, logger)
	return c
}

func (c *PaymentCloser) Name() string { return "payment-closer" }

func (c *PaymentCloser) Init(ctx lynx.AppContext) error { return nil }

func (c *PaymentCloser) Start(ctx context.Context) error { return c.loop.Start(ctx) }

func (c *PaymentCloser) Stop(ctx context.Context) error { return nil }

func (c *PaymentCloser) runOnce(ctx context.Context) error {
	closed, err := c.payments.CloseExpiredOrders(ctx, time.Now())
	if err != nil {
		return err
	}
	if closed > 0 {
		c.logger.Info("closed expired payment orders", "count", closed)
	}
	return nil
}
