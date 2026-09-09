package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	apppayments "github.com/torchwoodcloud/torchwood/internal/app/payments"
)

// paymentCloserInterval 是超时未付关单的扫描间隔（每分钟一次）。
const paymentCloserInterval = time.Minute

// PaymentCloser 周期把 created/paying 且超过 expires_at 的订单翻 closed
// （v3 设计 §1.3；closed 不在 §5.1 事件目录，不发 outbox 事件）。
type PaymentCloser struct {
	payments *apppayments.Payments
	logger   *slog.Logger
	interval time.Duration
}

// NewPaymentCloser creates the expired-order closing service.
func NewPaymentCloser(payments *apppayments.Payments, logger *slog.Logger) *PaymentCloser {
	if logger == nil {
		logger = slog.Default()
	}
	return &PaymentCloser{payments: payments, logger: logger, interval: paymentCloserInterval}
}

func (c *PaymentCloser) Name() string { return "payment-closer" }

func (c *PaymentCloser) Init(ctx lynx.AppContext) error { return nil }

// Start 周期关单：失败仅记日志；阻塞到 ctx 取消。
func (c *PaymentCloser) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("payment closer stopped")
			return nil
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *PaymentCloser) Stop(ctx context.Context) error { return nil }

func (c *PaymentCloser) runOnce(ctx context.Context) {
	closed, err := c.payments.CloseExpiredOrders(ctx, time.Now())
	if err != nil {
		c.logger.Error("close expired payment orders failed", "error", err)
		return
	}
	if closed > 0 {
		c.logger.Info("closed expired payment orders", "count", closed)
	}
}
