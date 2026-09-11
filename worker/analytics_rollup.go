package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
)

// analyticsRollupInterval 是 rollup 周期（设计 §7：每小时——昨日终算 +
// 当日部分聚合，新鲜度 ≤ 1h）。
const analyticsRollupInterval = time.Hour

// AnalyticsRollupWorker 周期执行 analytics rollup（D7 幂等重算式：同窗口
// 重跑覆盖不翻倍）。业务逻辑在 app/analytics.Rollup（RunWorkerOnce 模式），
// 本作业只做周期与日志。
type AnalyticsRollupWorker struct {
	rollup   *appanalytics.Rollup
	logger   *slog.Logger
	interval time.Duration
}

// NewAnalyticsRollupWorker creates the analytics rollup service.
func NewAnalyticsRollupWorker(rollup *appanalytics.Rollup, logger *slog.Logger) *AnalyticsRollupWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &AnalyticsRollupWorker{rollup: rollup, logger: logger, interval: analyticsRollupInterval}
}

func (w *AnalyticsRollupWorker) Name() string { return "analytics-rollup" }

func (w *AnalyticsRollupWorker) Init(ctx lynx.AppContext) error { return nil }

// Start 周期 rollup：失败仅记日志（覆盖式重算天然重试安全）；阻塞到 ctx 取消。
func (w *AnalyticsRollupWorker) Start(ctx context.Context) error {
	w.runOnce(ctx)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("analytics rollup worker stopped")
			return nil
		case <-ticker.C:
			w.runOnce(ctx)
		}
	}
}

func (w *AnalyticsRollupWorker) Stop(ctx context.Context) error { return nil }

func (w *AnalyticsRollupWorker) runOnce(ctx context.Context) {
	if err := w.rollup.RunWorkerOnce(ctx, time.Now()); err != nil {
		w.logger.Error("analytics rollup failed", "error", err)
	}
}
