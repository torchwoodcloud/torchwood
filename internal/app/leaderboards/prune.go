package leaderboards

import (
	"context"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
)

// pruneBatchLimit 是单项目单轮清理的批删上限（DELETE 单语句，量级护栏）。
const pruneBatchLimit = 5000

// PruneExpiredPeriods 是 retention 清理入口（worker 低频 ticker 驱动）：
// 遍历 active 项目 → retention > 0 的榜 → 比保留截止期更早的期整期删除。
// 单榜失败不影响其他榜（错误吞掉记日志，下一轮重试——清理是幂等的）。
func (a *Leaderboards) PruneExpiredPeriods(ctx context.Context) (int, error) {
	if a.projects == nil {
		return 0, nil
	}
	all, err := a.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	now := a.ts()
	pruned := 0
	for i := range all {
		if all[i].Status != "active" {
			continue
		}
		boards, err := a.boards.List(ctx, all[i].ID)
		if err != nil {
			a.logger.Warn("leaderboards prune: list boards failed", "project_id", all[i].ID, "error", err)
			continue
		}
		for j := range boards {
			b := &boards[j]
			cutoff, ok := domainleaderboards.RetentionCutoffKey(b, now)
			if !ok {
				continue
			}
			n, err := a.entries.PruneOlderThan(ctx, all[i].ID, b.ID, cutoff)
			if err != nil {
				a.logger.Warn("leaderboards prune: delete failed", "project_id", all[i].ID, "board_id", b.ID, "error", err)
				continue
			}
			if n > 0 {
				pruned++
				a.logger.Info("leaderboards pruned expired periods",
					"project_id", all[i].ID, "board_id", b.ID, "rows", n, "cutoff", cutoff)
			}
		}
	}
	return pruned, nil
}
