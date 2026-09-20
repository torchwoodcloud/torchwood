package worker

import (
	"context"
	"log/slog"
	"time"

	"github.com/lynx-go/lynx"
	"github.com/prometheus/client_golang/prometheus"
	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
)

// leaderboardsSettleInterval 是结榜发奖扫描周期。结算以期为粒度（日/周），
// 分钟级扫描没有必要——10 分钟与 retention 清理同一量级；扫描基于状态
//（已封榜 && 无结算行）而非定时投放，停机恢复自动补算，慢一轮无影响。
const leaderboardsSettleInterval = 10 * time.Minute

// ——worker 侧 Prometheus 指标（P2 观测补全；全局无 label，理由同
// analytics_rollup.go——项目/榜维度归因走结构化日志，避免时序基数爆炸）——
var (
	leaderboardsSettleBacklogPeriods = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "torchwood_leaderboards_settle_backlog_periods",
		Help: "Settlable leaderboard periods found by the latest settle scan (lower bound when a board hits the scan limit).",
	})
	leaderboardsSettleFailuresTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "torchwood_leaderboards_settle_failures_total",
		Help: "Failed leaderboard period settlements: per-period errors, plus 1 for a round-level scan failure.",
	})
)

func init() {
	prometheus.MustRegister(leaderboardsSettleBacklogPeriods, leaderboardsSettleFailuresTotal)
}

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
	stats, err := s.leaderboards.SettleDueStats(settleCtx)
	if err != nil {
		if ctx.Err() == nil {
			leaderboardsSettleFailuresTotal.Inc()
			s.logger.Warn("leaderboards settle scan failed", "error", err)
		}
		// 扫描失败时 backlog 指标保持上一轮值（最近一次已知状态，不归零）。
		return
	}
	leaderboardsSettleBacklogPeriods.Set(float64(stats.Backlog))
	leaderboardsSettleFailuresTotal.Add(float64(stats.Failed))
	if stats.Settled > 0 {
		s.logger.Info("leaderboards settled periods", "count", stats.Settled)
	}
}
