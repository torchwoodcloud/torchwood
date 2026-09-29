package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appsubs "github.com/torchwoodcloud/torchwood/internal/app/subscriptions"
)

const (
	subscriptionBillerInterval = time.Minute
	// subscriptionBillerTimeout 是单轮扣款周期的预算上限（有界批量；短于
	// 扫描周期，对齐 cron 领取的 50s/1min 比例；WithoutCancel 使停机不腰斩
	// 在途轮次）。
	subscriptionBillerTimeout = 50 * time.Second
)

// SubscriptionBiller 周期扫描 platform 到期订阅：扣款续期 / past_due / expired
// （v3 设计 §3.1）。
type SubscriptionBiller struct {
	subs   *appsubs.Subscriptions
	logger *slog.Logger
	loop   *Periodic
}

// NewSubscriptionBiller creates the platform subscription billing worker.
func NewSubscriptionBiller(subs *appsubs.Subscriptions, logger *slog.Logger) *SubscriptionBiller {
	if logger == nil {
		logger = slog.Default()
	}
	c := &SubscriptionBiller{subs: subs, logger: logger}
	c.loop = NewPeriodic("subscription-biller", subscriptionBillerInterval, subscriptionBillerTimeout, c.runOnce, logger)
	return c
}

func (c *SubscriptionBiller) Name() string { return "subscription-biller" }

func (c *SubscriptionBiller) Init(ctx lynx.AppContext) error { return nil }

func (c *SubscriptionBiller) Start(ctx context.Context) error { return c.loop.Start(ctx) }

func (c *SubscriptionBiller) Stop(ctx context.Context) error { return nil }

func (c *SubscriptionBiller) runOnce(ctx context.Context) error {
	n, err := c.subs.RunBillingCycle(ctx, time.Now().UTC())
	if err != nil {
		return err
	}
	if n > 0 {
		c.logger.Info("subscription billing cycle processed", "count", n)
	}
	return nil
}
