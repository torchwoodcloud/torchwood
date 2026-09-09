package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appbilling "github.com/torchwoodcloud/torchwood/internal/app/billing"
)

// usageRollupInterval 是小时 bucket 落表扫描间隔（设计 §4.2：每 5min）。
const usageRollupInterval = 5 * time.Minute

// UsageRollupWorker 周期把 Redis 上一完整小时 bucket 幂等 upsert 到
// usage_rollups，并月聚合 billing_statements。
type UsageRollupWorker struct {
	billing  *appbilling.Billing
	logger   *slog.Logger
	interval time.Duration
}

// NewUsageRollupWorker creates the usage rollup service.
func NewUsageRollupWorker(billing *appbilling.Billing, logger *slog.Logger) *UsageRollupWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &UsageRollupWorker{billing: billing, logger: logger, interval: usageRollupInterval}
}

func (w *UsageRollupWorker) Name() string { return "usage-rollup" }

func (w *UsageRollupWorker) Init(ctx lynx.AppContext) error { return nil }

// Start 周期 rollup：失败仅记日志；阻塞到 ctx 取消。
func (w *UsageRollupWorker) Start(ctx context.Context) error {
	w.runOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("usage rollup worker stopped")
			return nil
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *UsageRollupWorker) Stop(ctx context.Context) error { return nil }

func (w *UsageRollupWorker) runOnce(ctx context.Context) {
	if err := w.billing.RunWorkerOnce(ctx, time.Now()); err != nil {
		w.logger.Error("usage rollup failed", "error", err)
	}
}
