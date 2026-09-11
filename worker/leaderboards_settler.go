package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
)

// leaderboardsSettleInterval 是结榜发奖扫描周期（与 cronLoop 同节拍）。
// 扫描基于状态（已封榜 && 无结算行）而非定时投放，停机恢复自动补算。
const leaderboardsSettleInterval = time.Minute

// LeaderboardsSettler 周期结算已封榜的期（Phase 2：声明式结榜发奖）。
type LeaderboardsSettler struct {
	leaderboards *appleaderboards.Leaderboards
	logger       *slog.Logger
	interval     time.Duration
}

// NewLeaderboardsSettler creates the leaderboard settlement scanner.
func NewLeaderboardsSettler(lb *appleaderboards.Leaderboards, logger *slog.Logger) *LeaderboardsSettler {
	if logger == nil {
		logger = slog.Default()
	}
	return &LeaderboardsSettler{leaderboards: lb, logger: logger, interval: leaderboardsSettleInterval}
}

func (s *LeaderboardsSettler) Name() string { return "leaderboards-settler" }

func (s *LeaderboardsSettler) Init(ctx lynx.AppContext) error { return nil }

func (s *LeaderboardsSettler) Start(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("leaderboards settler stopped")
			return nil
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *LeaderboardsSettler) Stop(ctx context.Context) error { return nil }

func (s *LeaderboardsSettler) runOnce(ctx context.Context) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	n, err := s.leaderboards.SettleDue(settleCtx)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("leaderboards settle scan failed", "error", err)
		}
		return
	}
	if n > 0 {
		s.logger.Info("leaderboards settled periods", "count", n)
	}
}
