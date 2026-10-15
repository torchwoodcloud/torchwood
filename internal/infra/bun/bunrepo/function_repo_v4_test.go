package bunrepo_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// Functions v4 仓储语义（迁移 000028：function_invocations 改名 + target 列族
// + 状态机收敛 queued/running/completed/failed）。对标 webhook_repo_test 的
// 真库集成模式。

func seedFunction(t *testing.T, ctx context.Context, repo domainfunctions.FunctionRepo, projectID, id string) *domainfunctions.Function {
	t.Helper()
	fn := &domainfunctions.Function{
		ID: id, ProjectID: projectID, Name: "fn-" + id,
		TargetURL:               "https://" + id + ".example.com",
		TimeoutSeconds:          15,
		Enabled:                 true,
		TargetSecretEnc:         "enc:v1:test",      // #nosec G101 -- 测试夹具伪密文（非凭证）
		TargetSecretFingerprint: "twfnsec_deadbeef", // #nosec G101 -- 测试夹具伪指纹
		DeclaredScopes:          []string{"assets:write"},
		CreatedAt:               time.Now(),
		UpdatedAt:               time.Now(),
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))
	return fn
}

func seedInvocation(t *testing.T, ctx context.Context, repo domainfunctions.FunctionRepo, projectID, functionID, id, status, source string) *domainfunctions.InvocationRecord {
	t.Helper()
	e := &domainfunctions.InvocationRecord{
		ID: id, FunctionID: functionID, ProjectID: projectID,
		Status: status, TriggerSource: source,
		InvokingUserID:       "user-1",
		ClientIdempotencyKey: "idem-" + id,
		CreatedAt:            time.Now(),
		UpdatedAt:            time.Now(),
	}
	require.NoError(t, repo.CreateInvocation(ctx, e))
	return e
}

// TestFunctionRepo_TargetRoundtrip 函数 CRUD + target 列族往返：url/密文/
// 指纹/宽限窗列读回一致；Update 白名单列可变（bun 更新写规范）。
func TestFunctionRepo_TargetRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	fn := seedFunction(t, ctx, repo, projectID, "fn_rt")

	got, err := repo.GetFunction(ctx, projectID, "fn_rt")
	require.NoError(t, err)
	require.Equal(t, fn.TargetURL, got.TargetURL)
	require.Equal(t, fn.TargetSecretEnc, got.TargetSecretEnc)
	require.Equal(t, fn.TargetSecretFingerprint, got.TargetSecretFingerprint)
	require.Equal(t, []string{"assets:write"}, got.DeclaredScopes)

	// 轮换写回（双钥窗列可变）+ url 更新。
	got.TargetPreviousSecretEnc = "enc:v1:prev"
	got.TargetSecretRotatedAt = time.Now()
	got.TargetURL = "https://new.example.com"
	require.NoError(t, repo.UpdateFunction(ctx, got))

	got2, err := repo.GetFunction(ctx, projectID, "fn_rt")
	require.NoError(t, err)
	require.Equal(t, "https://new.example.com", got2.TargetURL)
	require.Equal(t, "enc:v1:prev", got2.TargetPreviousSecretEnc)
	require.False(t, got2.TargetSecretRotatedAt.IsZero())
}

// TestInvocationRepo_Lifecycle 调用记录生命周期：两写（INSERT running 预占 +
// 终态 UPDATE 列白名单）、CAS 领取（queued→running）、终态兜底（仅未终态可
// 标 failed）、幂等唯一索引冲突归一、限频计数、跨项目隔离。
func TestInvocationRepo_Lifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	seedFunction(t, ctx, repo, projectID, "fn_lc")

	// 两写预占：INSERT running + 终态 UPDATE（stdout/stderr 列已退役）。
	seedInvocation(t, ctx, repo, projectID, "fn_lc", "inv_run", domainfunctions.InvocationStatusRunning, "client")
	rec, err := repo.GetInvocation(ctx, projectID, "fn_lc", "inv_run")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusRunning, rec.Status)
	rec.Status = domainfunctions.InvocationStatusCompleted
	rec.Response = `{"ok":true}`
	rec.DurationMS = 42
	require.NoError(t, repo.UpdateInvocation(ctx, rec))
	got, err := repo.GetInvocation(ctx, projectID, "fn_lc", "inv_run")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, got.Status)
	require.Equal(t, `{"ok":true}`, got.Response)
	require.Equal(t, int64(42), got.DurationMS)

	// CAS 领取：queued→running 生效；重复领取（已 running）不生效。
	seedInvocation(t, ctx, repo, projectID, "fn_lc", "inv_queued", domainfunctions.InvocationStatusQueued, "client")
	claimed, err := repo.TransitionInvocationStatus(ctx, projectID, "fn_lc", "inv_queued",
		domainfunctions.InvocationStatusQueued, domainfunctions.InvocationStatusRunning)
	require.NoError(t, err)
	require.True(t, claimed)
	claimed, err = repo.TransitionInvocationStatus(ctx, projectID, "fn_lc", "inv_queued",
		domainfunctions.InvocationStatusQueued, domainfunctions.InvocationStatusRunning)
	require.NoError(t, err)
	require.False(t, claimed, "状态机收敛后无 building；已领取行不可二领")

	// 终态兜底：仅未终态（queued/running）可标 failed。
	require.NoError(t, repo.FailInvocationIfActive(ctx, projectID, "fn_lc", "inv_queued", "worker retries exhausted"))
	got, err = repo.GetInvocation(ctx, projectID, "fn_lc", "inv_queued")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusFailed, got.Status)
	require.NoError(t, repo.FailInvocationIfActive(ctx, projectID, "fn_lc", "inv_run", "should not overwrite"))
	got, err = repo.GetInvocation(ctx, projectID, "fn_lc", "inv_run")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusCompleted, got.Status, "completed 终态行不被覆盖（防重复投递回写）")
	require.Empty(t, got.Error)

	// 幂等唯一索引：并发同键第二 INSERT 冲突归一领域哨兵。
	seedInvocation(t, ctx, repo, projectID, "fn_lc", "inv_idem", domainfunctions.InvocationStatusQueued, "client")
	dup := &domainfunctions.InvocationRecord{
		ID: "inv_idem_dup", FunctionID: "fn_lc", ProjectID: projectID,
		Status: domainfunctions.InvocationStatusQueued, TriggerSource: "client",
		InvokingUserID: "user-1", ClientIdempotencyKey: "idem-inv_idem",
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	err = repo.CreateInvocation(ctx, dup)
	require.True(t, errors.Is(err, domainfunctions.ErrInvocationIdempotencyConflict), "23505 归一 ErrInvocationIdempotencyConflict，got %v", err)

	// 限频计数：窗口内 client 来源行数。
	n, err := repo.CountClientInvocations(ctx, projectID, "fn_lc", "user-1", time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 3, n, "三条 client 来源行（inv_run/inv_queued/inv_idem）")

	// 幂等回读。
	got, err = repo.GetInvocationByIdempotencyKey(ctx, projectID, "fn_lc", "user-1", "idem-inv_idem")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "inv_idem", got.ID)

	// 跨项目隔离：其他项目查不到。
	otherID, _, otherCleanup := testutil.CreateTestProject(ctx, db)
	defer otherCleanup()
	other, err := repo.GetInvocation(ctx, otherID, "fn_lc", "inv_run")
	require.NoError(t, err)
	require.Nil(t, other, "project-scoped 访问按 (project, function) 收窄")
}

// TestInvocationRepo_OrphanRecoverAndPrune 孤儿恢复（行内超时快照 + 120s
// 宽限，queued/running 双态）与保留窗清理（server 条数式 / trigger 时间窗）。
func TestInvocationRepo_OrphanRecoverAndPrune(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	quoted := testutil.CatalogQuoted(projectID)

	repo := bunrepo.NewFunctionRepository(db)
	seedFunction(t, ctx, repo, projectID, "fn_or")

	// 孤儿：running + 超时快照 1s（宽限 120s 已过）。
	stale := 1
	old := time.Now().Add(-time.Hour)
	_, err0 := db.Conn(ctx).ExecContext(ctx, `INSERT INTO `+quoted+`.function_invocations
		(id, function_id, project_id, status, timeout_seconds, trigger_source, created_at, updated_at)
		VALUES ('inv_orphan', 'fn_or', ?, 'running', ?, '', ?, ?)`,
		projectID, stale, old, old)
	require.NoError(t, err0)
	// 非孤儿：running 无快照（updated_at 刚刚）。
	_, err0 = db.Conn(ctx).ExecContext(ctx, `INSERT INTO `+quoted+`.function_invocations
		(id, function_id, project_id, status, trigger_source, created_at, updated_at)
		VALUES ('inv_fresh', 'fn_or', ?, 'running', '', NOW(), NOW())`,
		projectID)
	require.NoError(t, err0)

	n, err := repo.RecoverOrphanInvocationsInProject(ctx, projectID, time.Now().Add(-time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
	got, err := repo.GetInvocation(ctx, projectID, "fn_or", "inv_orphan")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusFailed, got.Status)
	got, err = repo.GetInvocation(ctx, projectID, "fn_or", "inv_fresh")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.InvocationStatusRunning, got.Status, "新鲜行不被误判孤儿")

	// 保留清理：server 面（trigger_source=''）终态行条数式保留；trigger 来源
	// 行按时间窗（48h）——未到期不裁。
	_, err0 = db.Conn(ctx).ExecContext(ctx, `INSERT INTO `+quoted+`.function_invocations
		(id, function_id, project_id, status, trigger_source, created_at, updated_at)
		VALUES ('inv_srv_old', 'fn_or', ?, 'completed', '', ?, ?),
		       ('inv_srv_keep', 'fn_or', ?, 'completed', '', ?, ?),
		       ('inv_cli_old', 'fn_or', ?, 'completed', 'client', ?, ?),
		       ('inv_cli_new', 'fn_or', ?, 'completed', 'client', ?, ?)`,
		projectID, time.Now().Add(-72*time.Hour), time.Now().Add(-72*time.Hour),
		projectID, time.Now(), time.Now(),
		projectID, time.Now().Add(-72*time.Hour), time.Now().Add(-72*time.Hour),
		projectID, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	require.NoError(t, err0)
	require.NoError(t, repo.PruneOldInvocationsInProject(ctx, projectID, "fn_or", 1))
	require.NoError(t, repo.PruneTriggerInvocationsInProject(ctx, projectID, "fn_or", time.Now().Add(-48*time.Hour)))

	var cnt int
	require.NoError(t, db.Conn(ctx).QueryRowContext(ctx,
		`SELECT count(*) FROM `+quoted+`.function_invocations WHERE id IN ('inv_srv_old','inv_srv_keep','inv_cli_old','inv_cli_new')`).Scan(&cnt))
	require.Equal(t, 2, cnt, "server 旧终态行条数裁剪 + 超窗 client 行时间裁剪；keep 行与未到期 client 行保留")
}
