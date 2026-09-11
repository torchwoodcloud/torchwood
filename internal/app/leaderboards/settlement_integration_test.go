package leaderboards

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	appassets "github.com/torchwoodcloud/torchwood/internal/app/assets"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	infraevents "github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
)

// mockGranter 记录发放调用；failSubjects 模拟单笔失败（重跑再放行）。
type mockGranter struct {
	mu          sync.Mutex
	calls       []string
	failSubject map[string]int // subject → 剩余失败次数
}

func (m *mockGranter) GrantReward(ctx context.Context, projectID, subjectID, assetCode string, amount int64, idempotencyKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, idempotencyKey)
	if m.failSubject != nil {
		if n, ok := m.failSubject[subjectID]; ok && n > 0 {
			m.failSubject[subjectID] = n - 1
			return fmt.Errorf("mock grant failure for %s", subjectID)
		}
	}
	return nil
}

func (m *mockGranter) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func newSettlementUC(t *testing.T, granter domainleaderboards.RewardGranter) (*Leaderboards, context.Context, *clients.Database, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	defs := bunrepo.NewAssetDefRepository(db)
	// 预置奖励引用的资产定义（保存时校验存在性）。
	now := time.Now().UTC()
	for _, code := range []string{"gold", "coin"} {
		if err := defs.Insert(ctx, &domainassets.Def{
			ProjectID: projectID, ID: code + "_def", Code: code, Name: code,
			Class: domainassets.ClassCurrency, Status: domainassets.DefStatus("active"),
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	uc := NewLeaderboards(
		db,
		bunrepo.NewLeaderboardBoardRepository(db),
		bunrepo.NewLeaderboardEntryRepository(db),
		bunrepo.NewLeaderboardSettlementRepository(db),
		granter,
		defs,
		nil,
		nil,
		bunrepo.NewProjectRepository(db),
	)
	admin := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "k1",
		ActorID:        "actor1",
	})
	return uc, admin, db, projectID
}

// seedSealedEntries 直插已封榜期的条目（submit 通道对旧期拒收——封榜即
// 终局；测试以直插模拟封榜前已提交）。
func seedSealedEntries(t *testing.T, db *clients.Database, projectID, boardID, periodKey string, entries map[string]int64) {
	t.Helper()
	repo := bunrepo.NewLeaderboardEntryRepository(db)
	now := time.Now().UTC()
	for sub, val := range entries {
		if err := repo.Insert(context.Background(), &domainleaderboards.Entry{
			ProjectID: projectID, BoardID: boardID, PeriodKey: periodKey,
			SubjectID: sub, Value: val, SubmitCount: 1,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed %s: %v", sub, err)
		}
	}
}

func oldPeriodKey(t *testing.T, days int) string {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return domainleaderboards.PeriodKeyMinus(domainleaderboards.PeriodDaily, loc, time.Now(), days)
}

func rewardBoard(id string, tieBreak domainleaderboards.TieBreak, rules ...domainleaderboards.RewardRule) *domainleaderboards.Board {
	return &domainleaderboards.Board{
		ID:              id,
		Sort:            domainleaderboards.SortDesc,
		TieBreak:        tieBreak,
		Policy:          domainleaderboards.PolicyBest,
		PeriodKind:      domainleaderboards.PeriodDaily,
		PeriodTZ:        "Asia/Shanghai",
		ClientSubmit:    true,
		PerSubjectLimit: 100,
		Rewards:         rules,
	}
}

func rankPtr(v int32) *int32  { return &v }
func valuePtr(v int64) *int64 { return &v }

// 结算生命周期：封榜期扫描 → 规则展开 → 幂等发放 → settled；二次扫描零增量。
func TestIntegration_SettlementLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	granter := &mockGranter{}
	uc, admin, db, projectID := newSettlementUC(t, granter)

	rules := []domainleaderboards.RewardRule{
		{RankMin: rankPtr(1), RankMax: rankPtr(2), AssetCode: "gold", Amount: 100},
		{ValueMin: valuePtr(50), AssetCode: "coin", Amount: 10},
	}
	b := rewardBoard("daily_rewarded", domainleaderboards.TieBreakParallel, rules...)
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	old := oldPeriodKey(t, 3)
	// 值 100,90,90 → rank 1,2,2。规则1（top2 含端点）命中 3 人；规则2
	//（value>=50）命中 3 人 → 共 6 笔发放。
	seedSealedEntries(t, db, projectID, b.ID, old, map[string]int64{"u1": 100, "u2": 90, "u3": 90})

	n, err := uc.SettleDue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("settled = %d, want 1", n)
	}
	if got := granter.count(); got != 6 {
		t.Fatalf("grants = %d, want 6（top2 含端点 3 人 + value>=50 3 人）", got)
	}
	s, grants, err := uc.GetSettlement(admin, b.ID, old)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != domainleaderboards.SettlementStatusSettled || s.EntryCount != 3 || s.GrantCount != 6 {
		t.Fatalf("settlement = %+v", s)
	}
	for _, g := range grants {
		if g.Status != "granted" {
			t.Fatalf("grant %+v 未发放", g)
		}
	}

	// 幂等：再次扫描零增量（已 settled 的期不再认领）。
	n, err = uc.SettleDue(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("second scan = (%d, %v), want 0", n, err)
	}
	if got := granter.count(); got != 6 {
		t.Fatalf("grants after rescan = %d, want 6（不双发）", got)
	}
}

// 并列边界：parallel 按 rank 含端点（3 人）；earliest 按 position 截断（2 人）。
func TestIntegration_SettlementTieBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	for _, tc := range []struct {
		tie  domainleaderboards.TieBreak
		want int
	}{
		{domainleaderboards.TieBreakParallel, 3},
		{domainleaderboards.TieBreakEarliest, 2},
	} {
		granter := &mockGranter{}
		uc, admin, db, projectID := newSettlementUC(t, granter)
		rank := int32(2)
		b := rewardBoard(fmt.Sprintf("tie_%s", tc.tie), tc.tie,
			domainleaderboards.RewardRule{RankMax: &rank, AssetCode: "gold", Amount: 1})
		if _, err := uc.CreateBoard(admin, b); err != nil {
			t.Fatal(err)
		}
		old := oldPeriodKey(t, 4)
		seedSealedEntries(t, db, projectID, b.ID, old, map[string]int64{"a": 100, "b": 90, "c": 90})
		if _, err := uc.SettleDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := granter.count(); got != tc.want {
			t.Fatalf("tie=%s grants = %d, want %d", tc.tie, got, tc.want)
		}
	}
}

// 单笔失败不阻断整期：failed 记录在案，重跑补发且不重复已发放。
func TestIntegration_SettlementRerunAfterFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	granter := &mockGranter{failSubject: map[string]int{"u2": 1}}
	uc, admin, db, projectID := newSettlementUC(t, granter)
	rank := int32(3)
	b := rewardBoard("rerun_board", domainleaderboards.TieBreakParallel,
		domainleaderboards.RewardRule{RankMax: &rank, AssetCode: "gold", Amount: 5})
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	old := oldPeriodKey(t, 5)
	seedSealedEntries(t, db, projectID, b.ID, old, map[string]int64{"u1": 100, "u2": 100, "u3": 100})
	if _, err := uc.SettleDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, grants, err := uc.GetSettlement(admin, b.ID, old)
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != domainleaderboards.SettlementStatusSettled {
		t.Fatalf("单笔失败也应 settled（不阻断整期）: %+v", s)
	}
	failed := 0
	for _, g := range grants {
		if g.Status == "failed" {
			failed++
		}
	}
	if failed != 1 || s.GrantCount != 2 {
		t.Fatalf("failed=%d granted=%d, want 1/2", failed, s.GrantCount)
	}

	// 重跑：失败笔补发，已发放笔不重复调用。
	if err := uc.RerunSettlement(admin, b.ID, old); err != nil {
		t.Fatal(err)
	}
	if got := granter.count(); got != 4 { // 首轮 3 次（1 失败）+ 重跑 1 次补发
		t.Fatalf("grants total = %d, want 4（重跑只补 failed）", got)
	}
	_, grants, err = uc.GetSettlement(admin, b.ID, old)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if g.Status != "granted" {
			t.Fatalf("重跑后仍有未发放: %+v", g)
		}
	}
	// 全部已发放时重跑为幂等空操作（不重复调用 granter）。
	if err := uc.RerunSettlement(admin, b.ID, old); err != nil {
		t.Fatalf("settled 重跑应为幂等空操作: %v", err)
	}
	if got := granter.count(); got != 4 {
		t.Fatalf("重跑后再次重跑 grants = %d, want 4", got)
	}
}

// void：预弃奖（无行直接落 voided，SettleDue 跳过）；settled 拒绝 void。
func TestIntegration_SettlementVoid(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	granter := &mockGranter{}
	uc, admin, db, projectID := newSettlementUC(t, granter)
	rank := int32(1)
	b := rewardBoard("void_board", domainleaderboards.TieBreakParallel,
		domainleaderboards.RewardRule{RankMax: &rank, AssetCode: "gold", Amount: 1})
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	voided := oldPeriodKey(t, 6)
	settled := oldPeriodKey(t, 7)
	seedSealedEntries(t, db, projectID, b.ID, voided, map[string]int64{"u1": 100})
	seedSealedEntries(t, db, projectID, b.ID, settled, map[string]int64{"u1": 100})

	// 预弃奖：voided 期落 voided 行。
	if err := uc.VoidSettlement(admin, b.ID, voided); err != nil {
		t.Fatal(err)
	}
	if _, err := uc.SettleDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := granter.count(); got != 1 {
		t.Fatalf("grants = %d, want 1（voided 期被跳过）", got)
	}
	s, _, err := uc.GetSettlement(admin, b.ID, voided)
	if err != nil || s == nil || s.Status != domainleaderboards.SettlementStatusVoided {
		t.Fatalf("voided settlement = (%+v, %v)", s, err)
	}
	// settled 期拒绝 void（已发放只能手工追回）。
	if err := uc.VoidSettlement(admin, b.ID, settled); codeOf(t, err) != codes.FailedPrecondition {
		t.Fatalf("settled void = %v, want FailedPrecondition", err)
	}
}

// 端到端（真 Assets）：发放落 holdings + ledger（ref=leaderboard_settlement），
// 规则校验（未知 asset / none 期 rewards）拒收。
func TestIntegration_SettlementRealAssetsGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	defs := bunrepo.NewAssetDefRepository(db)
	now := time.Now().UTC()
	if err := defs.Insert(ctx, &domainassets.Def{
		ProjectID: projectID, ID: "gold_def", Code: "gold", Name: "Gold",
		Class: domainassets.ClassCurrency, Status: domainassets.DefStatus("active"),
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	assets := appassets.NewAssets(db, defs,
		bunrepo.NewAssetHoldingRepository(db),
		bunrepo.NewAssetLedgerRepository(db),
		infraevents.NewEventOutbox(db), nil, bunrepo.NewProjectRepository(db))
	granter := NewAssetsRewardGranter(assets)

	uc := NewLeaderboards(db,
		bunrepo.NewLeaderboardBoardRepository(db),
		bunrepo.NewLeaderboardEntryRepository(db),
		bunrepo.NewLeaderboardSettlementRepository(db),
		granter,
		defs,
		nil,
		nil,
		bunrepo.NewProjectRepository(db),
	)
	admin := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		ProjectID:      projectID,
		APIKeyID:       "k1",
	})
	rank := int32(1)
	b := rewardBoard("real_assets", domainleaderboards.TieBreakParallel,
		domainleaderboards.RewardRule{RankMax: &rank, AssetCode: "gold", Amount: 42})
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	old := oldPeriodKey(t, 8)
	seedSealedEntries(t, db, projectID, b.ID, old, map[string]int64{"u1": 100})
	if _, err := uc.SettleDue(ctx); err != nil {
		t.Fatal(err)
	}

	holdings, err := assets.ListUserAssets(admin, "u1", 10, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range holdings {
		if h.DefCode == "gold" && h.Holding.Quantity == 42 {
			found = true
		}
	}
	if !found {
		t.Fatalf("gold holdings = %+v, want 42", holdings)
	}
	ledger, err := assets.ListUserLedger(admin, "u1", "", 10, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	refOK := false
	for _, l := range ledger {
		if l.Entry.RefType == "leaderboard_settlement" {
			refOK = true
		}
	}
	if !refOK {
		t.Fatal("ledger 缺 leaderboard_settlement 引用")
	}

	// 规则校验：未知 asset code 建榜拒收。
	unknown := rewardBoard("bad_asset", domainleaderboards.TieBreakParallel,
		domainleaderboards.RewardRule{RankMax: &rank, AssetCode: "nope", Amount: 1})
	if _, err := uc.CreateBoard(admin, unknown); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("未知 asset = %v, want InvalidArgument", err)
	}
	// none 期配置 rewards 拒收。
	noneBoard := rewardBoard("none_board", domainleaderboards.TieBreakParallel,
		domainleaderboards.RewardRule{ValueMin: valuePtr(1), AssetCode: "gold", Amount: 1})
	noneBoard.PeriodKind = domainleaderboards.PeriodNone
	if _, err := uc.CreateBoard(admin, noneBoard); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("none 期 rewards = %v, want InvalidArgument", err)
	}
}
