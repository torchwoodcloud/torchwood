package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
)

// leaderboardsPruneInterval 是排行榜 retention 清理周期（低频足够：
// 清理按期粒度整期删除，幂等，慢一轮无影响）。
const leaderboardsPruneInterval = 30 * time.Minute

// LeaderboardsCleaner 周期清理超过 retention_periods 的排行榜期
// （dogfooding 提案 2026-09-11：默认 0 = 永久保留，只有显式配置的榜被清）。
type LeaderboardsCleaner struct {
	leaderboards *appleaderboards.Leaderboards
	logger       *slog.Logger
	interval     time.Duration
}

// NewLeaderboardsCleaner creates the leaderboard retention pruner.
func NewLeaderboardsCleaner(lb *appleaderboards.Leaderboards, logger *slog.Logger) *LeaderboardsCleaner {
	if logger == nil {
		logger = slog.Default()
	}
	return &LeaderboardsCleaner{leaderboards: lb, logger: logger, interval: leaderboardsPruneInterval}
}

func (c *LeaderboardsCleaner) Name() string { return "leaderboards-cleaner" }

func (c *LeaderboardsCleaner) Init(ctx lynx.AppContext) error { return nil }

func (c *LeaderboardsCleaner) Start(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.logger.Info("leaderboards cleaner stopped")
			return nil
		case <-ticker.C:
			c.runOnce(ctx)
		}
	}
}

func (c *LeaderboardsCleaner) Stop(ctx context.Context) error { return nil }

func (c *LeaderboardsCleaner) runOnce(ctx context.Context) {
	pruneCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	n, err := c.leaderboards.PruneExpiredPeriods(pruneCtx)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.Warn("leaderboards prune failed", "error", err)
		}
		return
	}
	if n > 0 {
		c.logger.Info("leaderboards pruned expired boards", "count", n)
	}
}
