package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/prometheus/client_golang/prometheus"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
)

// analyticsRollupInterval 是 rollup 周期（设计 §7：每小时——昨日终算 +
// 当日部分聚合，新鲜度 ≤ 1h）。
const analyticsRollupInterval = time.Hour

// analyticsRollupTimeout 是单轮 rollup 的预算上限（对齐
// analyticsMaintenanceTimeout 样板：全 active 项目 × 两日重算是有界操作，
// 超时后剩余项目顺延到下一轮——覆盖式重算幂等，天然重试安全； WithoutCancel
// 使停机不腰斩在途轮次，轮次在预算边界自然收束）。事务内另有逐语句
// statement_timeout 兜底（bunrepo analyticsRollupPreamble）。
const analyticsRollupTimeout = 10 * time.Minute

// ——worker 侧 Prometheus 指标（P2 观测补全；billing usageRollupLag 同款：
// rollup 是全局周期作业，指标不带 project 维度 label——时间序列基数随项目数
// 爆炸，单项目归因走结构化日志）——
var (
	analyticsRollupDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "torchwood_analytics_rollup_duration_seconds",
		Help:    "Duration of one full analytics rollup round (all active projects).",
		Buckets: prometheus.ExponentialBuckets(0.5, 2, 12), // 0.5s .. ~34min
	})
	analyticsRollupFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_analytics_rollup_failures_total",
		Help: "Failed rollup operations: per project-day / definition-refresh errors, plus 1 for a round-level listing failure.",
	})
)

func init() {
	prometheus.MustRegister(analyticsRollupDurationSeconds, analyticsRollupFailuresTotal)
}

// AnalyticsRollupWorker 周期执行 analytics rollup（D7 幂等重算式：同窗口
// 重跑覆盖不翻倍）。业务逻辑在 app/analytics.Rollup（RunWorkerOnce 模式），
// 本作业只做周期、日志与指标。
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
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), analyticsRollupTimeout)
	defer cancel()
	started := time.Now()
	stats, err := w.rollup.RunWorkerOnceStats(runCtx, time.Now())
	analyticsRollupDurationSeconds.Observe(time.Since(started).Seconds())
	if err != nil {
		analyticsRollupFailuresTotal.Inc()
		w.logger.Error("analytics rollup failed", "error", err)
		return
	}
	analyticsRollupFailuresTotal.Add(float64(stats.Failures))
}
