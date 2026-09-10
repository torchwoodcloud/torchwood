package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appsubs "github.com/torchwoodcloud/torchwood/internal/app/subscriptions"
)

const subscriptionBillerInterval = time.Minute

// SubscriptionBiller 周期扫描 platform 到期订阅：扣款续期 / past_due / expired
// （v3 设计 §3.1）。
type SubscriptionBiller struct {
	subs     *appsubs.Subscriptions
	logger   *slog.Logger
	interval time.Duration
}

// NewSubscriptionBiller creates the platform subscription billing worker.
func NewSubscriptionBiller(subs *appsubs.Subscriptions, logger *slog.Logger) *SubscriptionBiller {
	if logger == nil {
		logger = slog.Default()
	}
	return &SubscriptionBiller{subs: subs, logger: logger, interval: subscriptionBillerInterval}
}

func (c *SubscriptionBiller) Name() string { return "subscription-biller" }

func (c *SubscriptionBiller) Init(ctx lynx.AppContext) error { return nil }

func (c *SubscriptionBiller) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("subscription biller stopped")
			return nil
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *SubscriptionBiller) Stop(ctx context.Context) error { return nil }

func (c *SubscriptionBiller) runOnce(ctx context.Context) {
	n, err := c.subs.RunBillingCycle(ctx, time.Now().UTC())
	if err != nil {
		c.logger.Error("subscription billing cycle failed", "error", err)
		return
	}
	if n > 0 {
		c.logger.Info("subscription billing cycle processed", "count", n)
	}
}
