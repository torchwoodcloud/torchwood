package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// seedExecutionRow 构造一条执行记录（P2 测试辅助）。
func seedExecutionRow(t *testing.T, ctx context.Context, repo domainfunctions.FunctionRepo,
	projectID, functionID, deploymentID, id, status, triggerSource, userID, idemKey string, createdAt time.Time,
) {
	t.Helper()
	require.NoError(t, repo.CreateExecution(ctx, &domainfunctions.ExecutionRecord{
		ID: id, FunctionID: functionID, ProjectID: projectID, DeploymentID: deploymentID,
		Status: status, TriggerSource: triggerSource, InvokingUserID: userID,
		ClientIdempotencyKey: idemKey, CreatedAt: createdAt, UpdatedAt: createdAt,
	}))
}

// idsOf 把执行记录列表折叠为 id 集合。
func idsOf(recs []domainfunctions.ExecutionRecord) map[string]bool {
	out := map[string]bool{}
	for i := range recs {
		out[recs[i].ID] = true
	}
	return out
}

// TestFunctionRepository_RetentionTiering（Q7 保留分级）：server 面条数式
// prune 不裁剪 client/http 来源行；trigger 来源时间窗 prune 不裁剪 server
// 面行——两条删除路径互不误伤。
func TestFunctionRepository_RetentionTiering(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	fn := seedFunctionRow(t, ctx, repo, projectID)
	dep := seedDeploymentRow(t, ctx, repo, fn, domainfunctions.DeploymentStatusReady)

	now := time.Now()
	old := now.Add(-72 * time.Hour) // 48h 之外

	// server 面：5 条旧（created_at 逐条递增）+ 1 条新（共 6 条，条数式
	// keep=2 应裁 4 条旧）。
	for i := 0; i < 5; i++ {
		seedExecutionRow(t, ctx, repo, projectID, fn.ID, dep.ID,
			"srv_old_"+string(rune('a'+i)), domainfunctions.ExecutionStatusCompleted, "", "", "", old.Add(time.Duration(i)*time.Second))
	}
	seedExecutionRow(t, ctx, repo, projectID, fn.ID, dep.ID,
		"srv_new", domainfunctions.ExecutionStatusCompleted, "", "", "", now)

	// client 面：1 条 72h 前 + 1 条窗口内（限频计数行形态）。
	seedExecutionRow(t, ctx, repo, projectID, fn.ID, dep.ID,
		"cli_old", domainfunctions.ExecutionStatusCompleted, domainfunctions.TriggerSourceClient, "user-1", "k1", old)
	seedExecutionRow(t, ctx, repo, projectID, fn.ID, dep.ID,
		"cli_new", domainfunctions.ExecutionStatusCompleted, domainfunctions.TriggerSourceClient, "user-1", "", now)

	// http 面：1 条 72h 前。
	seedExecutionRow(t, ctx, repo, projectID, fn.ID, dep.ID,
		"http_old", domainfunctions.ExecutionStatusCompleted, "http:trg-1", "", "", old)

	// 路径一：server 面条数式（keep 2）——只裁 server 旧行（4 条），
	// client/http 行不受影响。
	require.NoError(t, repo.PruneOldExecutionsInProject(ctx, projectID, fn.ID, 2))
	recs, err := repo.ListExecutions(ctx, projectID, fn.ID, 100)
	require.NoError(t, err)
	ids := idsOf(recs)
	require.Len(t, recs, 5, "6 server（keep 2）+ 2 client + 1 http = 5")
	for _, id := range []string{"cli_old", "cli_new", "http_old"} {
		require.True(t, ids[id], "%s 不应被 server 条数式 prune 删除", id)
	}
	require.NotContains(t, ids, "srv_old_a", "最旧的 server 行应被裁掉")

	// 路径二：trigger 面时间窗（48h）——删 cli_old / http_old，
	// server 行与窗口内 client 行不受影响。
	require.NoError(t, repo.PruneTriggerExecutionsInProject(ctx, projectID, fn.ID, now.Add(-48*time.Hour)))
	recs, err = repo.ListExecutions(ctx, projectID, fn.ID, 100)
	require.NoError(t, err)
	ids = idsOf(recs)
	for _, id := range []string{"srv_new", "srv_old_e", "cli_new"} {
		require.True(t, ids[id], "%s 不应被 trigger 时间窗 prune 删除", id)
	}
	require.NotContains(t, ids, "cli_old", "48h 前的 client 行应被时间窗 prune 删除")
	require.NotContains(t, ids, "http_old", "48h 前的 http 行应被时间窗 prune 删除")
	require.Len(t, recs, 3)
}

// TestFunctionRepository_ClientIdempotency（P2 幂等）：partial 唯一索引
// (project, function, user, key) WHERE key <> ” ——同键 INSERT 冲突归一为
// ErrExecutionIdempotencyConflict；无键行不受约束；跨用户同键合法。
func TestFunctionRepository_ClientIdempotency(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	fn := seedFunctionRow(t, ctx, repo, projectID)
	dep := seedDeploymentRow(t, ctx, repo, fn, domainfunctions.DeploymentStatusReady)

	now := time.Now()
	seedExecutionRow(t, ctx, repo, projectID, fn.ID, dep.ID,
		"exe_1", domainfunctions.ExecutionStatusRunning, domainfunctions.TriggerSourceClient, "user-1", "idem-1", now)

	// 同 (project, function, user, key)：冲突。
	err := repo.CreateExecution(ctx, &domainfunctions.ExecutionRecord{
		ID: "exe_2", FunctionID: fn.ID, ProjectID: projectID, DeploymentID: dep.ID,
		Status: domainfunctions.ExecutionStatusRunning, TriggerSource: domainfunctions.TriggerSourceClient,
		InvokingUserID: "user-1", ClientIdempotencyKey: "idem-1",
		CreatedAt: now, UpdatedAt: now,
	})
	require.ErrorIs(t, err, domainfunctions.ErrExecutionIdempotencyConflict)

	// 同键不同用户：合法。
	require.NoError(t, repo.CreateExecution(ctx, &domainfunctions.ExecutionRecord{
		ID: "exe_3", FunctionID: fn.ID, ProjectID: projectID, DeploymentID: dep.ID,
		Status: domainfunctions.ExecutionStatusRunning, TriggerSource: domainfunctions.TriggerSourceClient,
		InvokingUserID: "user-2", ClientIdempotencyKey: "idem-1",
		CreatedAt: now, UpdatedAt: now,
	}))

	// 无键（server 面）：不受约束。
	require.NoError(t, repo.CreateExecution(ctx, &domainfunctions.ExecutionRecord{
		ID: "exe_4", FunctionID: fn.ID, ProjectID: projectID, DeploymentID: dep.ID,
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: now, UpdatedAt: now,
	}))

	// 回读原样返回（running 态幂等语义）。
	got, err := repo.GetExecutionByIdempotencyKey(ctx, projectID, fn.ID, "user-1", "idem-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "exe_1", got.ID)
	require.Equal(t, domainfunctions.ExecutionStatusRunning, got.Status)
	missing, err := repo.GetExecutionByIdempotencyKey(ctx, projectID, fn.ID, "user-1", "nope")
	require.NoError(t, err)
	require.Nil(t, missing)

	// 限频 DB 降级计数：user-1 窗口内 1 条（user-2 不计）。
	n, err := repo.CountClientInvocations(ctx, projectID, fn.ID, "user-1", now.Add(-time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

// TestFunctionRepository_ClientPolicyRoundTrip：client 四列经 Create/Update
// 往返保真（更新白名单覆盖）。
func TestFunctionRepository_ClientPolicyRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	fn := seedFunctionRow(t, ctx, repo, projectID)
	require.False(t, fn.ClientCallable, "存量默认 FALSE（fail-closed）")

	fn.ClientCallable = true
	fn.ClientPerUserLimit = 3
	fn.ClientLimitWindow = domainfunctions.ClientLimitWindowHour
	fn.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateFunction(ctx, fn))

	got, err := repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.True(t, got.ClientCallable)
	require.Equal(t, 3, got.ClientPerUserLimit)
	require.Equal(t, domainfunctions.ClientLimitWindowHour, got.ClientLimitWindow)
}
