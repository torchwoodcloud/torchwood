package worker

import (
	"context"
	"log/slog"
	"sync"
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
// 周期与日志。两个周期共用一个 actor：各自一条 Periodic 循环，Start 等两条
// 都随 ctx 取消退出。
type AnalyticsMaintenanceWorker struct {
	maintenance   *appanalytics.Maintenance
	logger        *slog.Logger
	partitionLoop *Periodic
	cleanupLoop   *Periodic
}

// NewAnalyticsMaintenanceWorker creates the analytics maintenance service.
func NewAnalyticsMaintenanceWorker(maintenance *appanalytics.Maintenance, logger *slog.Logger) *AnalyticsMaintenanceWorker {
	if logger == nil {
		logger = slog.Default()
	}
	w := &AnalyticsMaintenanceWorker{maintenance: maintenance, logger: logger}
	w.partitionLoop = NewPeriodic("analytics-partitions", analyticsPartitionInterval, analyticsMaintenanceTimeout, w.partitionOnce, logger).RunAtStart()
	w.cleanupLoop = NewPeriodic("analytics-cleanup", analyticsCleanupInterval, analyticsMaintenanceTimeout, w.cleanupOnce, logger).RunAtStart()
	return w
}

func (w *AnalyticsMaintenanceWorker) Name() string { return "analytics-maintenance" }

func (w *AnalyticsMaintenanceWorker) Init(ctx lynx.AppContext) error { return nil }

// Start 双循环并行驱动（启动即各跑一轮——worker 重启补齐错过的窗口，
// 预建/搬运/裁剪/清洗全部幂等）；阻塞到 ctx 取消且两条循环都退出。
func (w *AnalyticsMaintenanceWorker) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { _ = w.partitionLoop.Start(ctx) })
	wg.Go(func() { _ = w.cleanupLoop.Start(ctx) })
	wg.Wait()
	return nil
}

func (w *AnalyticsMaintenanceWorker) Stop(ctx context.Context) error { return nil }

// partitionOnce 与 cleanupOnce 自行记日志并返回 nil：一轮多个步骤各有
// 专属错误消息，不走骨架的统一轮次日志（periodic 包注释的 fn 自记语义）。
func (w *AnalyticsMaintenanceWorker) partitionOnce(ctx context.Context) error {
	now := time.Now()
	if err := w.maintenance.RunPartitionsOnce(ctx, now); err != nil {
		w.logger.Error("analytics partition precreate failed", "error", err)
	}
	if err := w.maintenance.RunPruneOnce(ctx, now); err != nil {
		w.logger.Error("analytics retention prune failed", "error", err)
	}
	return nil
}

func (w *AnalyticsMaintenanceWorker) cleanupOnce(ctx context.Context) error {
	if err := w.maintenance.RunCleanupOnce(ctx, time.Now()); err != nil {
		w.logger.Error("analytics tombstone cleanup failed", "error", err)
	}
	return nil
}
