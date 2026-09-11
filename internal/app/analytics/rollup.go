// rollup.go 是 rollup worker 用例（PR5，docs/design/analytics.md §7）：
// 每小时对全部 active 项目执行「昨日终算 + 当日部分聚合」的幂等重算（D7），
// 并覆盖式刷新字典 total_30d。worker（worker/analytics_rollup.go）只做周期
// 与日志；本项目失败仅记日志继续（单项目故障不放大为全项目停摆）。
// 红线：不进 outbox、不发 realtime、不触发函数（D1）。
package analytics

import (
	"context"
	"log/slog"
	"time"

	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
)

// Rollup 是 rollup worker 用例聚合。
type Rollup struct {
	repo     domainanalytics.RollupRepository
	projects projects.Repository
	logger   *slog.Logger
}

// NewRollup 构造用例（Wire）：时钟经 RunWorkerOnce(ctx, now) 显式传入
// （worker 传 time.Now()，测试注入固定时钟）。
func NewRollup(repo domainanalytics.RollupRepository, projectsRepo projects.Repository, logger *slog.Logger) *Rollup {
	if logger == nil {
		logger = slog.Default()
	}
	return &Rollup{repo: repo, projects: projectsRepo, logger: logger}
}

// RunWorkerOnce 单轮 rollup：对每个 active 项目重算 [昨日, 今日] 两个 UTC 日
// （昨日终算 + 当日部分聚合——摄入钳制窗 ±24h 保证两天窗口是新增事件的完备
// 覆盖，D4/D7；worker 停摆 ≤1 天由「昨日终算」覆盖式补算自愈）。幂等性由
// 覆盖式 upsert 保证：同窗口重跑不翻倍。
func (u *Rollup) RunWorkerOnce(ctx context.Context, now time.Time) error {
	if u.repo == nil || u.projects == nil {
		return nil
	}
	now = now.UTC()
	today := dayStart(now)
	yesterday := today.AddDate(0, 0, -1)
	since := now.Add(-domainanalytics.DefTotalsWindow)

	list, err := u.projects.ListProjects(ctx)
	if err != nil {
		return err
	}
	for i := range list {
		p := &list[i]
		if p.Status != "active" {
			continue
		}
		for _, day := range []time.Time{yesterday, today} {
			if err := u.repo.RollupDay(ctx, p.ID, day); err != nil {
				u.logger.ErrorContext(ctx, "analytics rollup day failed",
					slog.String("project_id", p.ID),
					slog.String("day", day.Format("2006-01-02")),
					slog.String("error", err.Error()))
				break // 同项目后续日的重算依赖前序健康，下一轮整体重试
			}
		}
		if err := u.repo.RefreshDefinitionTotals30d(ctx, p.ID, since); err != nil {
			u.logger.ErrorContext(ctx, "analytics refresh definition totals failed",
				slog.String("project_id", p.ID),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// dayStart 归一到 UTC 零点。
func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
