// maintenance.go 是 maintenance worker 用例（PR5，docs/design/analytics.md
// §5/§7）：① 每日分区预建（未来 2 月，幂等）+ DEFAULT 非空搬运 + 保留期裁剪
// （DROP 整月过期分区，user_days 同窗口修剪）；② 低频 tombstone 清洗
// （D10：批删三表身份关联行后标记 done_at）。worker 只做周期与日志；单项目
// 失败仅记日志继续。红线：不进 outbox、不发 realtime、不触发函数（D1）。
package analytics

import (
	"context"
	"log/slog"
	"time"

	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// ensurePartitionMonths 是分区预建窗口（设计 §5：预建未来 2 月——含当月共
// 3 个月；当月分区恒在位 = 迁移静态分区退役后仍覆盖滚动推进）。
const ensurePartitionMonths = 3

// Maintenance 是 maintenance worker 用例聚合。
type Maintenance struct {
	repo          domainanalytics.MaintenanceRepository
	cleaner       domainanalytics.TombstoneCleaner
	projects      projects.Repository
	retentionDays int // 已归一（NormalizeRetentionDays）
	logger        *slog.Logger
}

// NewMaintenance 构造用例（retentionDays 必须已归一；组合根经
// NewMaintenanceFromConfig 注入）。时钟经各 Run*Once(ctx, now) 显式传入
// （worker 传 time.Now()，测试注入固定时钟）。
func NewMaintenance(
	repo domainanalytics.MaintenanceRepository,
	cleaner domainanalytics.TombstoneCleaner,
	projectsRepo projects.Repository,
	retentionDays int,
	logger *slog.Logger,
) *Maintenance {
	if logger == nil {
		logger = slog.Default()
	}
	return &Maintenance{
		repo:          repo,
		cleaner:       cleaner,
		projects:      projectsRepo,
		retentionDays: retentionDays,
		logger:        logger,
	}
}

// NewMaintenanceFromConfig 组合根入口：analytics.retention_days 配置归一后
// 注入（裁剪与查询 raw 回退窗口护栏共用同一配置口径）。
func NewMaintenanceFromConfig(
	cfg *config.AppConfig,
	repo domainanalytics.MaintenanceRepository,
	cleaner domainanalytics.TombstoneCleaner,
	projectsRepo projects.Repository,
	logger *slog.Logger,
) *Maintenance {
	days := int32(0)
	if cfg != nil {
		days = cfg.GetAnalytics().GetRetentionDays()
	}
	return NewMaintenance(repo, cleaner, projectsRepo, domainanalytics.NormalizeRetentionDays(days), logger)
}

// RunPartitionsOnce 单轮分区预建：未来 2 月（含当月）分区幂等就位；目标月与
// DEFAULT 重叠时由仓储在父表锁内搬运（新事件落正确分区）。
func (u *Maintenance) RunPartitionsOnce(ctx context.Context, now time.Time) error {
	if u.repo == nil || u.projects == nil {
		return nil
	}
	list, err := u.projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		p := &list[i]
		if p.Status != "active" {
			continue
		}
		if err := u.repo.EnsureMonthlyPartitions(ctx, p.ID, now, ensurePartitionMonths); err != nil {
			u.logger.ErrorContext(ctx, "analytics partition precreate failed",
				slog.String("project_id", p.ID),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// RunPruneOnce 单轮保留期裁剪：DROP 整月早于保留期的 analytics_events 分区
// （不足整月的部分留待下月）；user_days 与 raw 同窗口修剪（设计 §5，UV 精确
// 口径随之收敛到保留期）。
func (u *Maintenance) RunPruneOnce(ctx context.Context, now time.Time) error {
	if u.repo == nil || u.projects == nil {
		return nil
	}
	cutoff := dayStart(now).AddDate(0, 0, -u.retentionDays)
	list, err := u.projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		p := &list[i]
		if p.Status != "active" {
			continue
		}
		u.pruneProject(ctx, p.ID, cutoff)
	}
	return nil
}

func (u *Maintenance) pruneProject(ctx context.Context, projectID string, cutoff time.Time) {
	partitions, err := u.repo.ListExpiredMonthlyPartitions(ctx, projectID, cutoff)
	if err != nil {
		u.logger.ErrorContext(ctx, "analytics list expired partitions failed",
			slog.String("project_id", projectID),
			slog.String("error", err.Error()))
		return
	}
	for _, name := range partitions {
		if err := u.repo.DropPartition(ctx, projectID, name); err != nil {
			u.logger.ErrorContext(ctx, "analytics drop expired partition failed",
				slog.String("project_id", projectID),
				slog.String("partition", name),
				slog.String("error", err.Error()))
			return // 分区枚举有序，失败即停避免噪音；下一轮重试
		}
		u.logger.InfoContext(ctx, "analytics expired partition dropped",
			slog.String("project_id", projectID),
			slog.String("partition", name))
	}
	u.pruneUserDays(ctx, projectID, cutoff)
}

// pruneUserDays 批删保留期外 user_days 行（分批有界；行数已随 raw 分区裁剪
// 失去对应 raw，保留只会虚增跨日 UV 基数）。
func (u *Maintenance) pruneUserDays(ctx context.Context, projectID string, cutoff time.Time) {
	for batch := 0; batch < domainanalytics.MaxPruneBatches; batch++ {
		n, err := u.repo.PruneUserDaysBefore(ctx, projectID, cutoff, domainanalytics.MaxDeleteBatch)
		if err != nil {
			u.logger.ErrorContext(ctx, "analytics prune user_days failed",
				slog.String("project_id", projectID),
				slog.String("error", err.Error()))
			return
		}
		if n < domainanalytics.MaxDeleteBatch {
			return
		}
	}
	u.logger.WarnContext(ctx, "analytics user_days prune budget exhausted, continue next round",
		slog.String("project_id", projectID))
}

// RunCleanupOnce 单轮 tombstone 清洗（D10）：逐用户分批硬删 raw
// （(user_id, occurred_at) 索引点删，批 ≤ MaxDeleteBatch）→ user_days →
// first_seen → 标记 done_at。raw 未清空（预算耗尽）时不删用户粒度表、不标
// done——避免 rollup 从残存 raw 复活 first_seen/user_days 行。幂等：重删
// 安全（行已删 = 0 行），done_at 只在清洗完成后落值。
func (u *Maintenance) RunCleanupOnce(ctx context.Context, now time.Time) error {
	if u.cleaner == nil || u.projects == nil {
		return nil
	}
	list, err := u.projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	doneAt := now.UTC()
	for i := range list {
		p := &list[i]
		if p.Status != "active" {
			continue
		}
		users, err := u.cleaner.ListPendingDeletionUsers(ctx, p.ID, domainanalytics.MaxCleanupUsers)
		if err != nil {
			u.logger.ErrorContext(ctx, "analytics list pending deletions failed",
				slog.String("project_id", p.ID),
				slog.String("error", err.Error()))
			continue
		}
		for _, userID := range users {
			if err := u.cleanupUser(ctx, p.ID, userID, doneAt); err != nil {
				u.logger.ErrorContext(ctx, "analytics tombstone cleanup failed",
					slog.String("project_id", p.ID),
					slog.String("user_id", userID),
					slog.String("error", err.Error()))
				continue
			}
		}
	}
	return nil
}

func (u *Maintenance) cleanupUser(ctx context.Context, projectID, userID string, doneAt time.Time) error {
	var deleted int64
	for batch := 0; batch < domainanalytics.MaxCleanupUserBatches; batch++ {
		n, err := u.cleaner.DeleteEventsByUserBatch(ctx, projectID, userID, domainanalytics.MaxDeleteBatch)
		if err != nil {
			return err
		}
		deleted += n
		if n < domainanalytics.MaxDeleteBatch {
			// raw 已清空：用户粒度表随之删除并标记 done（行数有界 = 活跃天数
			// ≤ 保留期，单语句安全）。
			if _, err := u.cleaner.DeleteUserDaysByUser(ctx, projectID, userID); err != nil {
				return err
			}
			if _, err := u.cleaner.DeleteFirstSeenByUser(ctx, projectID, userID); err != nil {
				return err
			}
			return u.cleaner.MarkDeletionDone(ctx, projectID, userID, doneAt)
		}
	}
	u.logger.WarnContext(ctx, "analytics tombstone cleanup budget exhausted, continue next round",
		slog.String("project_id", projectID),
		slog.String("user_id", userID),
		slog.Int64("deleted_this_round", deleted))
	return nil
}
