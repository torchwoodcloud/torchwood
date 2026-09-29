package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appbilling "github.com/torchwoodcloud/torchwood/internal/app/billing"
)

const (
	// usageRollupInterval 是小时 bucket 落表扫描间隔（设计 §4.2：每 5min）。
	usageRollupInterval = 5 * time.Minute
	// usageRollupTimeout 是单轮 rollup 的预算上限（短于扫描周期；WithoutCancel
	// 使停机不腰斩在途轮次——Redis bucket 幂等 upsert，超时由下一轮收敛）。
	usageRollupTimeout = 4 * time.Minute
)

// UsageRollupWorker 周期把 Redis 上一完整小时 bucket 幂等 upsert 到
// usage_rollups，并月聚合 billing_statements。
type UsageRollupWorker struct {
	billing *appbilling.Billing
	logger  *slog.Logger
	loop    *Periodic
}

// NewUsageRollupWorker creates the usage rollup service.
func NewUsageRollupWorker(billing *appbilling.Billing, logger *slog.Logger) *UsageRollupWorker {
	if logger == nil {
		logger = slog.Default()
	}
	w := &UsageRollupWorker{billing: billing, logger: logger}
	w.loop = NewPeriodic("usage-rollup", usageRollupInterval, usageRollupTimeout, w.runOnce, logger).RunAtStart()
	return w
}

func (w *UsageRollupWorker) Name() string { return "usage-rollup" }

func (w *UsageRollupWorker) Init(ctx lynx.AppContext) error { return nil }

func (w *UsageRollupWorker) Start(ctx context.Context) error { return w.loop.Start(ctx) }

func (w *UsageRollupWorker) Stop(ctx context.Context) error { return nil }

func (w *UsageRollupWorker) runOnce(ctx context.Context) error {
	return w.billing.RunWorkerOnce(ctx, time.Now())
}
