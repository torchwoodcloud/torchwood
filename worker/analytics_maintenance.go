package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
)

const (
	// analyticsPartitionInterval 是分区治理周期（设计 §7：每日——预建未来
	// 2 月分区 + DEFAULT 非空搬运 + 保留期裁剪 DROP 过期月分区）。
	analyticsPartitionInterval = 24 * time.Hour
	// analyticsCleanupInterval 是 tombstone 清洗周期（设计 §7：每 6h）。
	analyticsCleanupInterval = 6 * time.Hour
	// analyticsMaintenanceTimeout 是单轮作业的预算上限（分区 DDL 与批量删除
	// 都是有界操作；超时由下一轮重试收敛——全部作业幂等）。
	analyticsMaintenanceTimeout = 10 * time.Minute
)

// AnalyticsMaintenanceWorker 周期执行 analytics 运维作业（设计 §7）：
//   - 每日：分区预建（未来 2 月，幂等）+ DEFAULT 非空搬运 + 保留期裁剪；
//   - 每 6h：tombstone 清洗（注销用户三表身份关联行硬删）。
//
// 业务逻辑在 app/analytics.Maintenance（RunWorkerOnce 模式），本作业只做
// 周期与日志。
type AnalyticsMaintenanceWorker struct {
	maintenance     *appanalytics.Maintenance
	logger          *slog.Logger
	partitionInt    time.Duration
	cleanupInterval time.Duration
}

// NewAnalyticsMaintenanceWorker creates the analytics maintenance service.
func NewAnalyticsMaintenanceWorker(maintenance *appanalytics.Maintenance, logger *slog.Logger) *AnalyticsMaintenanceWorker {
	if logger == nil {
		logger = slog.Default()
	}
	return &AnalyticsMaintenanceWorker{
		maintenance:     maintenance,
		logger:          logger,
		partitionInt:    analyticsPartitionInterval,
		cleanupInterval: analyticsCleanupInterval,
	}
}

func (w *AnalyticsMaintenanceWorker) Name() string { return "analytics-maintenance" }

func (w *AnalyticsMaintenanceWorker) Init(ctx lynx.AppContext) error { return nil }

// Start 启动即各跑一轮（worker 重启补齐错过的窗口——预建/搬运/裁剪/清洗全部
// 幂等），随后双 ticker 周期驱动；阻塞到 ctx 取消。
func (w *AnalyticsMaintenanceWorker) Start(ctx context.Context) error {
	w.partitionOnce(ctx)
	w.cleanupOnce(ctx)

	partitions := time.NewTicker(w.partitionInt)
	defer partitions.Stop()
	cleanups := time.NewTicker(w.cleanupInterval)
	defer cleanups.Stop()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("analytics maintenance worker stopped")
			return nil
		case <-partitions.C:
			w.partitionOnce(ctx)
		case <-cleanups.C:
			w.cleanupOnce(ctx)
		}
	}
}

func (w *AnalyticsMaintenanceWorker) Stop(ctx context.Context) error { return nil }

func (w *AnalyticsMaintenanceWorker) partitionOnce(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), analyticsMaintenanceTimeout)
	defer cancel()
	now := time.Now()
	if err := w.maintenance.RunPartitionsOnce(runCtx, now); err != nil {
		w.logger.Error("analytics partition precreate failed", "error", err)
	}
	if err := w.maintenance.RunPruneOnce(runCtx, now); err != nil {
		w.logger.Error("analytics retention prune failed", "error", err)
	}
}

func (w *AnalyticsMaintenanceWorker) cleanupOnce(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), analyticsMaintenanceTimeout)
	defer cancel()
	if err := w.maintenance.RunCleanupOnce(runCtx, time.Now()); err != nil {
		w.logger.Error("analytics tombstone cleanup failed", "error", err)
	}
}
