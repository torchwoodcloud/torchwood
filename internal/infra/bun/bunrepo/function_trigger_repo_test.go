package bunrepo_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

// seedTrigger 函数行 + 触发器仓储（见各用例内联构造）。
func newTrigger(t *testing.T, ctx context.Context, repo domainfunctions.TriggerRepo, projectID, functionID, id, typ string, cfg domainfunctions.TriggerConfig) *domainfunctions.Trigger {
	t.Helper()
	now := time.Now()
	trg := &domainfunctions.Trigger{
		ID:         id,
		ProjectID:  projectID,
		FunctionID: functionID,
		Type:       typ,
		Config:     cfg,
		Enabled:    true,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if typ == domainfunctions.TriggerTypeHTTP {
		trg.Token = "tok_" + id
	} else {
		due := now.Add(time.Minute)
		trg.NextRunAt = &due
	}
	require.NoError(t, repo.CreateTrigger(ctx, trg))
	return trg
}

// TestTriggerRepository_CRUDAndTokenLookup 触发器 CRUD、token 查找与跨项目
// fail-closed（未命中一律按不存在处理，不泄露存在性）。
func TestTriggerRepository_CRUDAndTokenLookup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	fnRepo := bunrepo.NewFunctionRepository(db)
	trgRepo := bunrepo.NewFunctionTriggerRepository(db)

	now := time.Now()
	require.NoError(t, fnRepo.CreateFunction(ctx, &domainfunctions.Function{
		ID: "fn_trg", ProjectID: projectID, Name: "trg", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}))

	trg := newTrigger(t, ctx, trgRepo, projectID, "fn_trg", "trg_http1", domainfunctions.TriggerTypeHTTP,
		domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeAsyncAck, AckBody: `{"is_valid":true}`, Handshake: domainfunctions.HandshakeEcho})

	// token 命中。
	got, err := trgRepo.GetTriggerByToken(ctx, projectID, "tok_trg_http1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, domainfunctions.TriggerTypeHTTP, got.Type)
	require.Equal(t, domainfunctions.ResponseModeAsyncAck, got.Config.ResponseMode)
	require.Equal(t, "http:trg_http1", got.TriggerSource())

	// 跨项目/错 token：nil（fail-closed）。
	other, err := trgRepo.GetTriggerByToken(ctx, "otherproj", "tok_trg_http1")
	require.NoError(t, err)
	require.Nil(t, other, "跨项目 token 不得命中")
	miss, err := trgRepo.GetTriggerByToken(ctx, projectID, "tok_missing")
	require.NoError(t, err)
	require.Nil(t, miss)

	// UpdateTrigger（轮换 token + 停用）；不可变列（type）由白名单保证不写。
	trg.Token = "tok_rotated"
	trg.Enabled = false
	require.NoError(t, trgRepo.UpdateTrigger(ctx, trg))
	got2, err := trgRepo.GetTrigger(ctx, projectID, "fn_trg", "trg_http1")
	require.NoError(t, err)
	require.NotNil(t, got2)
	require.Equal(t, "tok_rotated", got2.Token)
	require.False(t, got2.Enabled)
	require.Equal(t, domainfunctions.TriggerTypeHTTP, got2.Type, "type 不可变")
	// 停用后 token 查找由 app 层按 enabled 过滤（repo 层返回行本身）。
	// DeleteTrigger。
	require.NoError(t, trgRepo.DeleteTrigger(ctx, projectID, "fn_trg", "trg_http1"))
	gone, err := trgRepo.GetTrigger(ctx, projectID, "fn_trg", "trg_http1")
	require.NoError(t, err)
	require.Nil(t, gone)
}

// TestTriggerRepository_ClaimDueCron_CASAndMisfire 领取语义：CAS 幂等（同一
// 到期行只领一次）、misfire catch_up_once 补跑一次且一次推进跨过全部错过
// 周期、skip 只推进不补跑、disabled 不领取。
func TestTriggerRepository_ClaimDueCron_CASAndMisfire(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	fnRepo := bunrepo.NewFunctionRepository(db)
	trgRepo := bunrepo.NewFunctionTriggerRepository(db)

	now := time.Now()
	require.NoError(t, fnRepo.CreateFunction(ctx, &domainfunctions.Function{
		ID: "fn_cron", ProjectID: projectID, Name: "cron", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}))

	// 准时到期（宽限内）。
	due := now.Add(-10 * time.Second)
	trg := newTrigger(t, ctx, trgRepo, projectID, "fn_cron", "trg_cron1", domainfunctions.TriggerTypeCron,
		domainfunctions.TriggerConfig{Expr: "0 10 * * *", Misfire: domainfunctions.MisfireCatchUpOnce})
	_ = trg
	_, err := db.ExecContext(ctx, `UPDATE `+testutil.CatalogQuoted(projectID)+`.function_triggers SET next_run_at = ? WHERE id = 'trg_cron1'`, due)
	require.NoError(t, err)

	scanNow := now
	claims, err := trgRepo.ClaimDueCron(ctx, projectID, scanNow, 100, domainfunctions.DefaultCronNext)
	require.NoError(t, err)
	require.Len(t, claims, 1)
	require.Equal(t, "trg_cron1", claims[0].TriggerID)
	require.Equal(t, "fn_cron", claims[0].FunctionID)
	require.Equal(t, due.Unix(), claims[0].ScheduledFor.Unix())

	// 同一扫描时刻再领：CAS 输家（next_run_at 已推进）→ 空结果（不双跑）。
	claims2, err := trgRepo.ClaimDueCron(ctx, projectID, scanNow, 100, domainfunctions.DefaultCronNext)
	require.NoError(t, err)
	require.Empty(t, claims2)

	// 推进目标 = due 之后的下一计划时刻（10:00 档，跨日）。
	got, err := trgRepo.GetTrigger(ctx, projectID, "fn_cron", "trg_cron1")
	require.NoError(t, err)
	require.NotNil(t, got.NextRunAt)
	require.True(t, got.NextRunAt.After(scanNow), "推进目标 = due 之后的下一计划时刻")

	// misfire=skip：只推进不补跑。
	dueSkip := now.Add(-48 * time.Hour)
	newTrigger(t, ctx, trgRepo, projectID, "fn_cron", "trg_cron_skip", domainfunctions.TriggerTypeCron,
		domainfunctions.TriggerConfig{Expr: "0 10 * * *", Misfire: domainfunctions.MisfireSkip})
	_, err = db.ExecContext(ctx, `UPDATE `+testutil.CatalogQuoted(projectID)+`.function_triggers SET next_run_at = ? WHERE id = 'trg_cron_skip'`, dueSkip)
	require.NoError(t, err)
	claimsSkip, err := trgRepo.ClaimDueCron(ctx, projectID, now, 100, domainfunctions.DefaultCronNext)
	require.NoError(t, err)
	require.Empty(t, claimsSkip, "skip 模式 misfire 不补跑")
	gotSkip, err := trgRepo.GetTrigger(ctx, projectID, "fn_cron", "trg_cron_skip")
	require.NoError(t, err)
	require.NotNil(t, gotSkip.NextRunAt)
	require.True(t, gotSkip.NextRunAt.After(now), "skip 仍推进到 now 之后的下一计划时刻")

	// misfire=catch_up_once：宕机两天只补 1 次（ScheduledFor=原到期值）。
	dueCatch := now.Add(-48 * time.Hour)
	newTrigger(t, ctx, trgRepo, projectID, "fn_cron", "trg_cron_catch", domainfunctions.TriggerTypeCron,
		domainfunctions.TriggerConfig{Expr: "0 10 * * *", Misfire: domainfunctions.MisfireCatchUpOnce})
	_, err = db.ExecContext(ctx, `UPDATE `+testutil.CatalogQuoted(projectID)+`.function_triggers SET next_run_at = ? WHERE id = 'trg_cron_catch'`, dueCatch)
	require.NoError(t, err)
	claimsCatch, err := trgRepo.ClaimDueCron(ctx, projectID, now, 100, domainfunctions.DefaultCronNext)
	require.NoError(t, err)
	require.Len(t, claimsCatch, 1, "catch_up_once 补跑一次")
	require.Equal(t, dueCatch.Unix(), claimsCatch[0].ScheduledFor.Unix())
	gotCatch, err := trgRepo.GetTrigger(ctx, projectID, "fn_cron", "trg_cron_catch")
	require.NoError(t, err)
	require.NotNil(t, gotCatch.NextRunAt)
	require.True(t, gotCatch.NextRunAt.After(now), "一次推进跨过全部错过周期")

	// disabled 不领取。
	newTrigger(t, ctx, trgRepo, projectID, "fn_cron", "trg_cron_off", domainfunctions.TriggerTypeCron,
		domainfunctions.TriggerConfig{Expr: "* * * * *"})
	_, err = db.ExecContext(ctx, `UPDATE `+testutil.CatalogQuoted(projectID)+`.function_triggers SET enabled = false, next_run_at = ? WHERE id = 'trg_cron_off'`, now.Add(-time.Minute))
	require.NoError(t, err)
	claimsOff, err := trgRepo.ClaimDueCron(ctx, projectID, now, 100, domainfunctions.DefaultCronNext)
	require.NoError(t, err)
	for _, c := range claimsOff {
		require.NotEqual(t, "trg_cron_off", c.TriggerID)
	}

	// http 触发器不参与 cron 领取。
	newTrigger(t, ctx, trgRepo, projectID, "fn_cron", "trg_http_mixed", domainfunctions.TriggerTypeHTTP, domainfunctions.TriggerConfig{ResponseMode: domainfunctions.ResponseModeSync})
	claimsHTTP, err := trgRepo.ClaimDueCron(ctx, projectID, now, 100, domainfunctions.DefaultCronNext)
	require.NoError(t, err)
	for _, c := range claimsHTTP {
		require.NotEqual(t, "trg_http_mixed", c.TriggerID)
	}
}

// TestTriggerRepository_ClaimDueCron_ConcurrentNoDoubleClaim CAS 并发领取
// 不双跑（多实例扫描同一到期行）：N 个并发 ClaimDueCron 中恰有一个赢家，
// 全局领取总数 = 1。
func TestTriggerRepository_ClaimDueCron_ConcurrentNoDoubleClaim(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	fnRepo := bunrepo.NewFunctionRepository(db)
	trgRepo := bunrepo.NewFunctionTriggerRepository(db)

	now := time.Now()
	require.NoError(t, fnRepo.CreateFunction(ctx, &domainfunctions.Function{
		ID: "fn_race", ProjectID: projectID, Name: "race", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}))
	newTrigger(t, ctx, trgRepo, projectID, "fn_race", "trg_race", domainfunctions.TriggerTypeCron,
		domainfunctions.TriggerConfig{Expr: "* * * * *"})
	due := now.Add(-time.Minute)
	_, err := db.ExecContext(ctx, `UPDATE `+testutil.CatalogQuoted(projectID)+`.function_triggers SET next_run_at = ? WHERE id = 'trg_race'`, due)
	require.NoError(t, err)

	const workers = 8
	var (
		wg          sync.WaitGroup
		totalClaims atomic.Int64
		firstErr    atomic.Value
	)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claims, err := trgRepo.ClaimDueCron(ctx, projectID, now, 100, domainfunctions.DefaultCronNext)
			if err != nil {
				firstErr.CompareAndSwap(nil, err)
				return
			}
			totalClaims.Add(int64(len(claims)))
		}()
	}
	close(start)
	wg.Wait()

	require.Nil(t, firstErr.Load())
	require.Equal(t, int64(1), totalClaims.Load(), "并发领取必须恰有一个赢家（不双跑）")
}
