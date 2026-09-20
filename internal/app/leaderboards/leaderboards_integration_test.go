package leaderboards

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
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
		bunrepo.NewLeaderboardSettlementRepository(db),
		nil,
		nil,
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
		ID:               "daily_final",
		Sort:             domainleaderboards.SortDesc,
		TieBreak:         domainleaderboards.TieBreakParallel,
		Policy:           domainleaderboards.PolicyBest,
		PeriodKind:       domainleaderboards.PeriodDaily,
		PeriodTZ:         "Asia/Shanghai",
		ValueMin:         &min,
		ValueMax:         &max,
		ClientSubmit:     true,
		PerSubjectLimit:  20,
		RetentionPeriods: 90,
		SubjectKind:      "user",
	}
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
	for _, s := range []struct {
		sub string
		val int64
	}{{"u2", 500}, {"u3", 1499}} {
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

// server 面 provisioning 幂等创建：同配置重放 200 返还现状；异配置
// ALREADY_EXISTS 附字段 diff（漂移信号）。
func TestIntegration_ProvisionBoardIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)

	created, err := uc.CreateBoardProvisioning(admin, &domainleaderboards.Board{
		ID:         "daily_first",
		PeriodKind: domainleaderboards.PeriodDaily,
		PeriodTZ:   "Asia/Shanghai",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Sort != domainleaderboards.SortDesc || created.PerSubjectLimit != domainleaderboards.DefaultPerSubjectLimit {
		t.Fatalf("defaults not applied: %+v", created)
	}

	// 同配置重放（缺省归一后逐字段相等）→ 返回现状。
	replayed, err := uc.CreateBoardProvisioning(admin, &domainleaderboards.Board{
		ID:         "daily_first",
		PeriodKind: domainleaderboards.PeriodDaily,
		PeriodTZ:   "Asia/Shanghai",
	})
	if err != nil {
		t.Fatalf("replay should succeed: %v", err)
	}
	// created 是插入时的内存值（纳秒精度），replayed 从 PG 读出（微秒精度）。
	if replayed.CreatedAt.Sub(created.CreatedAt).Abs() > time.Microsecond {
		t.Fatalf("replay returned a different row: %v vs %v", replayed.CreatedAt, created.CreatedAt)
	}

	// 异配置重放 → ALREADY_EXISTS + diff。
	_, err = uc.CreateBoardProvisioning(admin, &domainleaderboards.Board{
		ID:              "daily_first",
		PeriodKind:      domainleaderboards.PeriodDaily,
		PeriodTZ:        "Asia/Shanghai",
		PerSubjectLimit: 50,
	})
	if codeOf(t, err) != codes.AlreadyExists {
		t.Fatalf("drift replay code = %v, want AlreadyExists: %v", codeOf(t, err), err)
	}
	if !strings.Contains(err.Error(), "per_subject_submit_limit") {
		t.Fatalf("diff missing field: %v", err)
	}
}

// 同 (board, period, subject) 并发首插的确定性交错回归（S8 缺陷 1）：手工
// 事务持有胜者未提交行，令 doSubmit 的 INSERT 阻塞在唯一仲裁（transactionid
// 锁等待）上；胜者提交后败者收到 23505——其事务已 aborted，事务内重试必然
// 25P02（修复前的失败形态），补偿必须在事务外整体重开并走合并路径。
func TestIntegration_ConcurrentFirstInsertSameSubject(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	uc := NewLeaderboards(
		db,
		bunrepo.NewLeaderboardBoardRepository(db),
		bunrepo.NewLeaderboardEntryRepository(db),
		bunrepo.NewLeaderboardSettlementRepository(db),
		nil,
		nil,
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

	b := &domainleaderboards.Board{
		ID:              "sum_board",
		Sort:            domainleaderboards.SortDesc,
		TieBreak:        domainleaderboards.TieBreakParallel,
		Policy:          domainleaderboards.PolicySum,
		PeriodKind:      domainleaderboards.PeriodNone,
		PerSubjectLimit: 10,
	}
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	period := domainleaderboards.PeriodKey(domainleaderboards.PeriodNone, nil, time.Now())

	// 胜者：手工事务内首插，保持未提交（持有唯一键仲裁权）。
	entries := bunrepo.NewLeaderboardEntryRepository(db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	txCtx := clients.WithTx(ctx, tx)
	now := time.Now().UTC()
	if err := entries.Insert(txCtx, &domainleaderboards.Entry{
		ProjectID:   projectID,
		BoardID:     b.ID,
		PeriodKey:   period,
		SubjectID:   "u1",
		Value:       10,
		SubmitCount: 1,
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}

	// 败者：同键提交，其 INSERT 阻塞至胜者提交后以 23505 落败。
	type submitResult struct {
		snap *domainleaderboards.Snapshot
		err  error
	}
	done := make(chan submitResult, 1)
	go func() {
		snap, _, err := uc.Submit(admin, SubmitCommand{BoardID: b.ID, SubjectID: "u1", Value: 7})
		done <- submitResult{snap, err}
	}()

	// 等待败者真正进入 transactionid 锁等待，保证交错必然形成。
	deadline := time.Now().Add(5 * time.Second)
	blocked := false
	for time.Now().Before(deadline) {
		var n int
		qerr := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND pid <> pg_backend_pid()
			   AND wait_event_type = 'Lock' AND wait_event = 'transactionid'`).Scan(&n)
		if qerr != nil {
			_ = tx.Rollback()
			t.Fatal(qerr)
		}
		if n > 0 {
			blocked = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blocked {
		_ = tx.Rollback()
		t.Fatal("败者 INSERT 未进入唯一仲裁等待（交错未形成，测试前提失效）")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if r.err != nil {
		t.Fatalf("败者应经重开事务的合并路径成功: %v", r.err)
	}
	if r.snap.Entry.Value != 17 || r.snap.Entry.SubmitCount != 2 {
		t.Fatalf("merged value=%d count=%d, want 17/2（胜者 10 + 败者 7 sum 合并）", r.snap.Entry.Value, r.snap.Entry.SubmitCount)
	}
	// 落库终态复核（提交链路以外的持久化证据）。
	got, err := entries.Get(ctx, projectID, b.ID, period, "u1")
	if err != nil || got == nil {
		t.Fatalf("read back: %v %v", got, err)
	}
	if got.Value != 17 || got.SubmitCount != 2 {
		t.Fatalf("final row value=%d count=%d, want 17/2", got.Value, got.SubmitCount)
	}
}

// 同 subject 多轮并发首插压力（S8 缺陷 1 补充）：每轮两个 goroutine 同时对
// 新鲜 (board, period, subject) 首插，双方都必须成功（一方 insert、一方经
// 补偿重开事务合并）；sum 合并值 201 校验更新不丢。
func TestIntegration_ConcurrentFirstInsertRounds(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	ctx := context.Background()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)
	uc := NewLeaderboards(
		db,
		bunrepo.NewLeaderboardBoardRepository(db),
		bunrepo.NewLeaderboardEntryRepository(db),
		bunrepo.NewLeaderboardSettlementRepository(db),
		nil,
		nil,
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
	b := &domainleaderboards.Board{
		ID:              "sum_rounds",
		Sort:            domainleaderboards.SortDesc,
		TieBreak:        domainleaderboards.TieBreakParallel,
		Policy:          domainleaderboards.PolicySum,
		PeriodKind:      domainleaderboards.PeriodNone,
		PerSubjectLimit: 10,
	}
	if _, err := uc.CreateBoard(admin, b); err != nil {
		t.Fatal(err)
	}
	period := domainleaderboards.PeriodKey(domainleaderboards.PeriodNone, nil, time.Now())
	entries := bunrepo.NewLeaderboardEntryRepository(db)

	const rounds = 10
	for i := 0; i < rounds; i++ {
		subject := fmt.Sprintf("round%02d", i)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for g := 0; g < 2; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				<-start
				_, _, errs[g] = uc.Submit(admin, SubmitCommand{
					BoardID:   b.ID,
					SubjectID: subject,
					Value:     int64(100 + g),
				})
			}(g)
		}
		close(start)
		wg.Wait()
		for g, err := range errs {
			if err != nil {
				t.Fatalf("round %d goroutine %d: %v", i, g, err)
			}
		}
		got, err := entries.Get(ctx, projectID, b.ID, period, subject)
		if err != nil || got == nil {
			t.Fatalf("round %d read back: %v %v", i, got, err)
		}
		if got.Value != 201 || got.SubmitCount != 2 {
			t.Fatalf("round %d value=%d count=%d, want 201/2（100+101 sum 合并）", i, got.Value, got.SubmitCount)
		}
	}
}

// 每项目榜配置上限（MaxBoardsPerProject）强制执行；满员时重放既有榜仍成功
// （重放路径先于上限判定——预置脚本的重复执行零 diff 不因满员劣化）。
func TestIntegration_BoardCap(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, _, _, admin := newTestUC(t)

	fill := &domainleaderboards.Board{ID: "fill", PeriodKind: domainleaderboards.PeriodNone}
	for i := 0; i < domainleaderboards.MaxBoardsPerProject; i++ {
		fill.ID = fmt.Sprintf("fill_%03d", i)
		if _, err := uc.CreateBoard(admin, fill); err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
	}
	_, err := uc.CreateBoardProvisioning(admin, &domainleaderboards.Board{ID: "overflow", PeriodKind: domainleaderboards.PeriodNone})
	if codeOf(t, err) != codes.ResourceExhausted {
		t.Fatalf("overflow code = %v, want ResourceExhausted: %v", codeOf(t, err), err)
	}
	if _, err := uc.CreateBoardProvisioning(admin, &domainleaderboards.Board{ID: "fill_000", PeriodKind: domainleaderboards.PeriodNone}); err != nil {
		t.Fatalf("replay at cap should succeed: %v", err)
	}
}
