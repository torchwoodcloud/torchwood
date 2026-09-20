package worker

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	appleaderboards "github.com/torchwoodcloud/torchwood/internal/app/leaderboards"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	pkgtestutil "github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// ——settler 集成测试（真 Postgres）：rewards SQL 预过滤只扫有奖励榜 +
// settle 指标（backlog gauge / failures counter）——

// recordingGranter 记录发放调用（结算路径冒烟，不真发资产）。
type recordingGranter struct{ calls int }

func (g *recordingGranter) GrantReward(context.Context, string, string, string, int64, string) error {
	g.calls++
	return nil
}

var _ domainleaderboards.RewardGranter = (*recordingGranter)(nil)

func settleRankPtr(v int32) *int32 { return &v }

func settleTestBoard(id string, rules []domainleaderboards.RewardRule) *domainleaderboards.Board {
	return &domainleaderboards.Board{
		ID:              id,
		Sort:            domainleaderboards.SortDesc,
		TieBreak:        domainleaderboards.TieBreakParallel,
		Policy:          domainleaderboards.PolicyBest,
		PeriodKind:      domainleaderboards.PeriodDaily,
		PeriodTZ:        "Asia/Shanghai",
		ClientSubmit:    true,
		PerSubjectLimit: 100,
		Rewards:         rules,
	}
}

// settleOldPeriodKey 是 days 天前的期 key（已封榜，可被结算扫描认领）。
func settleOldPeriodKey(t *testing.T, days int) string {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	return domainleaderboards.PeriodKeyMinus(domainleaderboards.PeriodDaily, loc, time.Now(), days)
}

func settleSeedEntry(t *testing.T, db *clients.Database, projectID, boardID, periodKey, subject string) {
	t.Helper()
	repo := bunrepo.NewLeaderboardEntryRepository(db)
	now := time.Now().UTC()
	require.NoError(t, repo.Insert(context.Background(), &domainleaderboards.Entry{
		ProjectID: projectID, BoardID: boardID, PeriodKey: periodKey,
		SubjectID: subject, Value: 100, SubmitCount: 1,
		CreatedAt: now, UpdatedAt: now,
	}))
}

// TestLeaderboardsSettlerIntegration_RewardedBoardsOnly（S12 项2）：有/无
// rewards 两榜各有已封榜旧期——SQL 预过滤后只结算有奖励榜；backlog gauge
// 只累计被扫描榜的待结算期；二轮扫描 backlog 归零（幂等不重复发）。
func TestLeaderboardsSettlerIntegration_RewardedBoardsOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := pkgtestutil.SetupTestDB(t)
	projectID, _, cleanup := pkgtestutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	// 奖励引用的资产定义（建榜校验存在性）。
	defs := bunrepo.NewAssetDefRepository(db)
	now := time.Now().UTC()
	require.NoError(t, defs.Insert(ctx, &domainassets.Def{
		ProjectID: projectID, ID: "gold_def", Code: "gold", Name: "gold",
		Class: domainassets.ClassCurrency, Status: domainassets.DefStatus("active"),
		CreatedAt: now, UpdatedAt: now,
	}))

	granter := &recordingGranter{}
	uc := appleaderboards.NewLeaderboards(
		db,
		bunrepo.NewLeaderboardBoardRepository(db),
		bunrepo.NewLeaderboardEntryRepository(db),
		bunrepo.NewLeaderboardSettlementRepository(db),
		granter,
		defs,
		nil, nil,
		bunrepo.NewProjectRepository(db),
	)
	admin := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "k1",
	})

	rewarded := settleTestBoard("rewarded", []domainleaderboards.RewardRule{
		{RankMin: settleRankPtr(1), RankMax: settleRankPtr(1), AssetCode: "gold", Amount: 1},
	})
	plain := settleTestBoard("plain", nil)
	for _, b := range []*domainleaderboards.Board{rewarded, plain} {
		_, err := uc.CreateBoard(admin, b)
		require.NoError(t, err)
	}

	// 两榜各播一个已封榜旧期（无奖励榜若被扫描本也会枚举出该期——过滤生效
	// 与否体现在 backlog 与结算行上）。
	period := settleOldPeriodKey(t, 5)
	settleSeedEntry(t, db, projectID, rewarded.ID, period, "u1")
	settleSeedEntry(t, db, projectID, plain.ID, period, "u1")

	// SQL 过滤单元证据：List 全量两榜；ListWithRewards 只剩有奖励榜。
	boardRepo := bunrepo.NewLeaderboardBoardRepository(db)
	all, err := boardRepo.List(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, all, 2, "List 保持全量语义（console/清理面不受影响）")
	rewardedOnly, err := boardRepo.ListWithRewards(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, rewardedOnly, 1, "SQL 侧 jsonb_array_length 过滤生效")
	require.Equal(t, rewarded.ID, rewardedOnly[0].ID)

	settler := NewLeaderboardsSettler(uc, nil)
	failuresBefore := testutil.ToFloat64(leaderboardsSettleFailuresTotal)
	settler.runOnce(ctx)

	require.InDelta(t, failuresBefore, testutil.ToFloat64(leaderboardsSettleFailuresTotal), 0.001,
		"健康轮次失败计数不动")
	require.InDelta(t, 1.0, testutil.ToFloat64(leaderboardsSettleBacklogPeriods), 0.001,
		"backlog 只累计有奖励榜的待结算期（无奖励榜不被扫描）")
	require.Equal(t, 1, granter.calls, "只有有奖励榜触发发放")

	// 结算行证据：有奖励榜 settled；无奖励榜无结算行。
	s, grants, err := uc.GetSettlement(admin, rewarded.ID, period)
	require.NoError(t, err)
	require.Equal(t, domainleaderboards.SettlementStatusSettled, s.Status)
	require.Len(t, grants, 1)
	_, _, err = uc.GetSettlement(admin, plain.ID, period)
	require.Error(t, err, "无奖励榜不产生结算行")

	// 二轮扫描：已结算期不再枚举，backlog 归零，不双发。
	settler.runOnce(ctx)
	require.InDelta(t, 0.0, testutil.ToFloat64(leaderboardsSettleBacklogPeriods), 0.001)
	require.Equal(t, 1, granter.calls, "重扫不双发")
}

// TestLeaderboardsSettleInterval_PeriodGranularity（S12 项2b）：结算是期粒度
// 动作，扫描周期分钟级无必要——钉在 10 分钟量级。
func TestLeaderboardsSettleInterval_PeriodGranularity(t *testing.T) {
	require.Equal(t, 10*time.Minute, leaderboardsSettleInterval)
	require.Greater(t, leaderboardsSettleInterval, 5*time.Minute,
		"扫描预算（runOnce 5min）必须短于扫描周期")
}
