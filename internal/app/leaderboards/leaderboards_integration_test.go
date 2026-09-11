package leaderboards

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestUC(t *testing.T) (*Leaderboards, string, context.Context, context.Context) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	uc := NewLeaderboards(
		db,
		bunrepo.NewLeaderboardBoardRepository(db),
		bunrepo.NewLeaderboardEntryRepository(db),
		bunrepo.NewIdempotencyStore(db),
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
	return uc, projectID, ctx, admin
}

func userCtx(ctx context.Context, projectID, userID string) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind:      shared.ActorKindEndUser,
		CredentialType: shared.CredentialTypeSession,
		ProjectID:      projectID,
		UserID:         userID,
		ActorID:        idgen.ID("user-" + userID),
	})
}

func guesspicBoard() *domainleaderboards.Board {
	min, max := int64(0), int64(3080)
	return &domainleaderboards.Board{
		ID:              "daily_final",
		Sort:            domainleaderboards.SortDesc,
		TieBreak:        domainleaderboards.TieBreakParallel,
		Policy:          domainleaderboards.PolicyBest,
		PeriodKind:      domainleaderboards.PeriodDaily,
		PeriodTZ:        "Asia/Shanghai",
		ValueMin:        &min,
		ValueMax:        &max,
		ClientSubmit:    true,
		PerSubjectLimit: 20,
		RetentionPeriods: 90,
		SubjectKind:     "user",
	}
}

func currentPeriod(t *testing.T, b *domainleaderboards.Board) string {
	t.Helper()
	loc, err := time.LoadLocation(b.PeriodTZ)
	if err != nil {
		t.Fatal(err)
	}
	return domainleaderboards.PeriodKey(b.PeriodKind, loc, time.Now())
}

func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	if err == nil {
		return codes.OK
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("non-status error: %v", err)
	}
	return st.Code()
}

// 验收 2：中场 submit(900) → 分位；终局 submit(1500) → 同条目更新、名次上升。
func TestIntegration_SubmitMidThenFinal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, projectID, ctx, admin := newTestUC(t)
	if _, err := uc.CreateBoard(admin, guesspicBoard()); err != nil {
		t.Fatal(err)
	}

	s1, _, err := uc.Submit(admin, SubmitCommand{BoardID: "daily_final", SubjectID: "u1", Value: 900})
	if err != nil {
		t.Fatal(err)
	}
	if s1.Stats.Total != 1 || s1.Stats.Rank != 1 {
		t.Fatalf("mid: %+v", s1.Stats)
	}

	// 再来两个较低分的玩家，然后 u1 终局提分。
	for _, s := range []struct{ sub string; val int64 }{{"u2", 500}, {"u3", 1499}} {
		if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: "daily_final", SubjectID: s.sub, Value: s.val}); err != nil {
			t.Fatal(err)
		}
	}
	final, _, err := uc.Submit(admin, SubmitCommand{BoardID: "daily_final", SubjectID: "u1", Value: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if final.Stats.Total != 3 {
		t.Fatalf("final total = %d, want 3（同条目更新不增 total）", final.Stats.Total)
	}
	if final.Entry.Value != 1500 || final.Entry.SubmitCount != 2 {
		t.Fatalf("entry value=%d submit_count=%d, want 1500/2", final.Entry.Value, final.Entry.SubmitCount)
	}
	if final.Stats.Rank != 1 || final.Stats.Below != 2 {
		t.Fatalf("final rank=%d below=%d, want 1/2", final.Stats.Rank, final.Stats.Below)
	}

	// me：分享卡复走。
	me, err := uc.GetMyEntry(userCtx(ctx, projectID, "u1"), "daily_final", "")
	if err != nil {
		t.Fatal(err)
	}
	if me.Entry == nil || me.Stats.Rank != 1 || me.Stats.Total != 3 {
		t.Fatalf("me = %+v", me)
	}
}

// 验收 3：上一期宽限可补传；未来期/已封榜期拒收。
func TestIntegration_PeriodGraceAndSeal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)
	b := guesspicBoard()
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation(b.PeriodTZ)
	now := time.Now()
	cur := domainleaderboards.PeriodKey(b.PeriodKind, loc, now)
	prev := domainleaderboards.PeriodKeyMinus(b.PeriodKind, loc, now, 1)
	sealed := domainleaderboards.PeriodKeyMinus(b.PeriodKind, loc, now, 2)

	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 100, Period: prev}); err != nil {
		t.Fatalf("补传上一期失败: %v", err)
	}
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 100, Period: sealed}); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("已封榜期 = %v, want InvalidArgument", err)
	}
	next := domainleaderboards.PeriodKeyMinus(b.PeriodKind, loc, now.AddDate(0, 0, 2), 0)
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 100, Period: next}); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("未来期 = %v, want InvalidArgument", err)
	}
	_ = cur

	// console 删条目同样受封榜限制。
	if err := uc.DeleteEntry(admin, b.ID, prev, "u1"); err != nil {
		t.Fatalf("宽限期内删除应允许: %v", err)
	}
	if err := uc.DeleteEntry(admin, b.ID, sealed, "u1"); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("封榜期删除 = %v, want InvalidArgument", err)
	}
}

// 验收 4+5：分数边界拒收；同主体重复提交不增 total、超限 RESOURCE_EXHAUSTED。
func TestIntegration_ValueBoundsAndQuota(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)
	b := guesspicBoard()
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}

	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 3080}); err != nil {
		t.Fatalf("3080 应接受: %v", err)
	}
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 3081}); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("3081 = %v, want InvalidArgument", err)
	}

	for i := 0; i < 19; i++ {
		if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 1000}); err != nil {
			t.Fatalf("第 %d 次提交失败: %v", i+2, err)
		}
	}
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 1000}); codeOf(t, err) != codes.ResourceExhausted {
		t.Fatalf("第 21 次 = %v, want ResourceExhausted", err)
	}
	total, err := uc.ListTop(admin, b.ID, "", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if total.Total != 1 {
		t.Fatalf("total = %d, want 1（重复提交不增 total）", total.Total)
	}
}

// 验收 6：N 个不同主体并发提交后 total == N（写时聚合做不到的那一条）。
func TestIntegration_ConcurrentSubmitTotal(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)
	b := guesspicBoard()
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	const n = 12
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, errs[i] = uc.Submit(admin, SubmitCommand{
				BoardID:   b.ID,
				SubjectID: fmt.Sprintf("u%02d", i),
				Value:     int64(1000 + i),
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("并发提交 %d 失败: %v", i, err)
		}
	}
	res, err := uc.ListTop(admin, b.ID, "", 100, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != n {
		t.Fatalf("total = %d, want %d", res.Total, n)
	}
	// 全序严格、rank 为 competition。
	for i, e := range res.Entries {
		if e.Position != int64(i+1) {
			t.Errorf("entries[%d].position = %d", i, e.Position)
		}
	}
}

// 验收 7 的结构性替代：client 面 submit 无 subject 字段（从 session 派生）；
// 未开 client_submit 的榜 PERMISSION_DENIED。
func TestIntegration_ClientFaceSubmitSelf(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, projectID, ctx, admin := newTestUC(t)
	if _, err := uc.CreateBoard(admin, guesspicBoard()); err != nil {
		t.Fatal(err)
	}
	closed := guesspicBoard()
	closed.ID = "server_only"
	closed.ClientSubmit = false
	if _, err := uc.CreateBoard(admin, closed); err != nil {
		t.Fatal(err)
	}

	user := userCtx(ctx, projectID, "player1")
	snap, _, err := uc.SubmitSelf(user, SubmitCommand{BoardID: "daily_final", Value: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Entry.SubjectID != "player1" {
		t.Fatalf("subject = %q, want session 派生的 player1", snap.Entry.SubjectID)
	}
	if _, _, err := uc.SubmitSelf(user, SubmitCommand{BoardID: "server_only", Value: 1}); codeOf(t, err) != codes.PermissionDenied {
		t.Fatalf("client_submit=false = %v, want PermissionDenied", err)
	}
	// server 面仍可代提交。
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: "server_only", SubjectID: "player1", Value: 1}); err != nil {
		t.Fatal(err)
	}
}

// request_id 幂等：同 key 重放返回首次快照，不重复烧 quota。
func TestIntegration_RequestIdIdempotency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)
	b := guesspicBoard()
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	cmd := SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 900, RequestID: "req-1"}
	first, replayed, err := uc.Submit(admin, cmd)
	if err != nil || replayed {
		t.Fatalf("first = err:%v replayed:%v", err, replayed)
	}
	second, replayed, err := uc.Submit(admin, cmd)
	if err != nil || !replayed {
		t.Fatalf("replay = err:%v replayed:%v", err, replayed)
	}
	if second.Entry.SubmitCount != first.Entry.SubmitCount {
		t.Fatalf("replay submit_count = %d, want %d（重放不烧额度）", second.Entry.SubmitCount, first.Entry.SubmitCount)
	}
	// 同 key 不同载荷 → KEY_CONFLICT。
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 1000, RequestID: "req-1"}); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("key conflict = %v, want InvalidArgument", err)
	}
}

// tie-break：同分时 tiebreak 值优者 position 靠前；rank 仍并列。
func TestIntegration_TiebreakOrdering(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)
	b := guesspicBoard()
	b.ID = "quiz"
	tb := domainleaderboards.SortAsc
	b.TiebreakOrder = &tb
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	// 同分 900，用时 300/100/200 —— position 应按用时升序。
	for _, s := range []struct {
		sub string
		val int64
		tbv int64
	}{{"slow", 900, 300}, {"fast", 900, 100}, {"mid", 900, 200}} {
		v := s.tbv
		if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: s.sub, Value: s.val, Tiebreak: &v}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := uc.ListTop(admin, b.ID, "", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 {
		t.Fatalf("total = %d", res.Total)
	}
	wantOrder := []string{"fast", "mid", "slow"}
	for i, e := range res.Entries {
		if e.SubjectID != wantOrder[i] {
			t.Fatalf("entries[%d] = %s, want %s", i, e.SubjectID, wantOrder[i])
		}
		if e.Rank != 1 {
			t.Fatalf("并列同分 rank = %d, want 1（competition）", e.Rank)
		}
	}
	// board 声明 tiebreak 后缺 tiebreak_value 的提交拒收。
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u9", Value: 100}); codeOf(t, err) != codes.InvalidArgument {
		t.Fatalf("缺 tiebreak = %v, want InvalidArgument", err)
	}
}

// 有条目后不可变字段拒绝修改；可变字段照常。
func TestIntegration_BoardImmutability(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)
	b := guesspicBoard()
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 100}); err != nil {
		t.Fatal(err)
	}
	newSort := domainleaderboards.SortAsc
	if _, err := uc.UpdateBoard(admin, UpdateBoardCommand{BoardID: b.ID, Sort: &newSort}); codeOf(t, err) != codes.FailedPrecondition {
		t.Fatalf("有条目改 sort = %v, want FailedPrecondition", err)
	}
	flag := true
	if _, err := uc.UpdateBoard(admin, UpdateBoardCommand{BoardID: b.ID, ClientSubmit: &flag}); err != nil {
		t.Fatalf("有条目改可变字段应允许: %v", err)
	}
	// 无条目的榜允许改期配置。
	fresh := guesspicBoard()
	fresh.ID = "fresh"
	if _, err := uc.CreateBoard(admin, fresh); err != nil {
		t.Fatal(err)
	}
	kind := domainleaderboards.PeriodWeekly
	if _, err := uc.UpdateBoard(admin, UpdateBoardCommand{BoardID: "fresh", PeriodKind: &kind}); err != nil {
		t.Fatalf("无条目改 period_kind 应允许: %v", err)
	}
}

// me 无条目：entry 缺省、total 照常返回（冷启动场景）。
func TestIntegration_MeWithoutEntry(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, projectID, ctx, admin := newTestUC(t)
	b := guesspicBoard()
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	if _, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 100}); err != nil {
		t.Fatal(err)
	}
	me, err := uc.GetMyEntry(userCtx(ctx, projectID, "nobody"), b.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if me.Entry != nil || me.Stats.Total != 1 {
		t.Fatalf("me = %+v, want entry=nil total=1", me)
	}
}
