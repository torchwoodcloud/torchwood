package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
)

const (
	// leaderboardsPruneInterval 是排行榜 retention 清理周期（低频足够：
	// 清理按期粒度整期删除，幂等，慢一轮无影响）。
	leaderboardsPruneInterval = 30 * time.Minute
	// leaderboardsPruneTimeout 是单轮清理的预算上限（整期删除是有界操作；
	// 超时由下一轮收敛；WithoutCancel 使停机不腰斩在途轮次）。
	leaderboardsPruneTimeout = 5 * time.Minute
)

// LeaderboardsCleaner 周期清理超过 retention_periods 的排行榜期
// （dogfooding 提案 2026-09-11：默认 0 = 永久保留，只有显式配置的榜被清）。
type LeaderboardsCleaner struct {
	leaderboards *appleaderboards.Leaderboards
	logger       *slog.Logger
	loop         *Periodic
}

// NewLeaderboardsCleaner creates the leaderboard retention pruner.
func NewLeaderboardsCleaner(lb *appleaderboards.Leaderboards, logger *slog.Logger) *LeaderboardsCleaner {
	if logger == nil {
		logger = slog.Default()
	}
	c := &LeaderboardsCleaner{leaderboards: lb, logger: logger}
	c.loop = NewPeriodic("leaderboards-cleaner", leaderboardsPruneInterval, leaderboardsPruneTimeout, c.runOnce, logger)
	return c
}

func (c *LeaderboardsCleaner) Name() string { return "leaderboards-cleaner" }

func (c *LeaderboardsCleaner) Init(ctx lynx.AppContext) error { return nil }

func (c *LeaderboardsCleaner) Start(ctx context.Context) error { return c.loop.Start(ctx) }

func (c *LeaderboardsCleaner) Stop(ctx context.Context) error { return nil }

func (c *LeaderboardsCleaner) runOnce(ctx context.Context) error {
	n, err := c.leaderboards.PruneExpiredPeriods(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		c.logger.Info("leaderboards pruned expired boards", "count", n)
	}
	return nil
}
