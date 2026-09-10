package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func seedFunctionRow(t *testing.T, ctx context.Context, repo domainfunctions.FunctionRepo, projectID string) *domainfunctions.Function {
	t.Helper()
	now := time.Now()
	fn := &domainfunctions.Function{
		ID:             "fn_1",
		ProjectID:      projectID,
		Name:           "hello",
		Runtime:        "node-18.0",
		Entrypoint:     "index.main",
		TimeoutSeconds: 15,
		Spec:           "shared-1x",
		Enabled:        true,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))
	return fn
}

func seedDeploymentRow(t *testing.T, ctx context.Context, repo domainfunctions.FunctionRepo, fn *domainfunctions.Function, status string) *domainfunctions.Deployment {
	t.Helper()
	now := time.Now()
	dep := &domainfunctions.Deployment{
		ID:         "dep_1",
		FunctionID: fn.ID,
		ProjectID:  fn.ProjectID,
		Size:       1024,
		Status:     status,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, repo.CreateDeployment(ctx, dep))
	return dep
}

func TestFunctionRepository_CRUDAndRelations(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)

	// 空列表。
	list, err := repo.ListFunctions(ctx, projectID)
	require.NoError(t, err)
	require.Empty(t, list)

	fn := seedFunctionRow(t, ctx, repo, projectID)

	got, err := repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "hello", got.Name)

	otherID, _, otherCleanup := testutil.CreateTestProject(ctx, db)
	defer otherCleanup()
	missing, err := repo.GetFunction(ctx, otherID, fn.ID)
	require.NoError(t, err)
	require.Nil(t, missing)

	// 更新。
	fn.Name = "renamed"
	fn.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateFunction(ctx, fn))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)

	// deployment + variables + executions。
	dep := seedDeploymentRow(t, ctx, repo, fn, domainfunctions.DeploymentStatusReady)

	require.NoError(t, repo.SetVariables(ctx, projectID, fn.ID, map[string]string{"A": "1", "B": "2"}))
	vars, err := repo.GetVariables(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"A": "1", "B": "2"}, vars)

	// SetVariables 全量替换。
	require.NoError(t, repo.SetVariables(ctx, projectID, fn.ID, map[string]string{"C": "3"}))
	vars, err = repo.GetVariables(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"C": "3"}, vars)

	rec := &domainfunctions.ExecutionRecord{
		ID:           "exe_1",
		FunctionID:   fn.ID,
		ProjectID:    projectID,
		DeploymentID: dep.ID,
		Status:       domainfunctions.ExecutionStatusQueued,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	require.NoError(t, repo.CreateExecution(ctx, rec))
	gotRec, err := repo.GetExecution(ctx, projectID, fn.ID, "exe_1")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusQueued, gotRec.Status)

	// 删除部署 → 执行记录级联删除。
	require.NoError(t, repo.DeleteDeployment(ctx, projectID, fn.ID, dep.ID))
	gotRec, err = repo.GetExecution(ctx, projectID, fn.ID, "exe_1")
	require.NoError(t, err)
	require.Nil(t, gotRec, "FK 级联删除 execution")

	// 删除函数 → 部署/变量级联删除。
	require.NoError(t, repo.DeleteFunction(ctx, projectID, fn.ID))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Nil(t, got)
	vars, err = repo.GetVariables(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Empty(t, vars)
	deps, err := repo.ListDeployments(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Empty(t, deps)
}

func TestFunctionRepository_PruneOldExecutions(t *testing.T) {
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

	for i := 0; i < 5; i++ {
		require.NoError(t, repo.CreateExecution(ctx, &domainfunctions.ExecutionRecord{
			ID:           "exe_" + string(rune('a'+i)),
			FunctionID:   fn.ID,
			ProjectID:    projectID,
			DeploymentID: dep.ID,
			Status:       domainfunctions.ExecutionStatusCompleted,
			CreatedAt:    time.Now().Add(time.Duration(i) * time.Second),
			UpdatedAt:    time.Now(),
		}))
	}
	require.NoError(t, repo.PruneOldExecutionsInProject(ctx, projectID, fn.ID, 3))
	recs, err := repo.ListExecutions(ctx, projectID, fn.ID, 100)
	require.NoError(t, err)
	require.Len(t, recs, 3, "仅保留最近 3 条")
}

// TestFunctionRepository_WritePathsAreProjectScoped 写路径带 project_id 过滤
// （G6-5/R07-P2-3、R08-P2-3）：跨项目 UpdateFunction / UpdateDeployment /
// SetVariables 不得影响本项目的行（即使 function/deployment id 相同）。
func TestFunctionRepository_WritePathsAreProjectScoped(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	otherID, _, otherCleanup := testutil.CreateTestProject(ctx, db)
	defer otherCleanup()

	repo := bunrepo.NewFunctionRepository(db)
	fn := seedFunctionRow(t, ctx, repo, projectID)
	dep := seedDeploymentRow(t, ctx, repo, fn, domainfunctions.DeploymentStatusReady)
	require.NoError(t, repo.SetVariables(ctx, projectID, fn.ID, map[string]string{"A": "1"}))

	// 跨项目 SetVariables：目标 schema 无该 function 行（FK），必须失败且不得清掉本项目变量。
	err := repo.SetVariables(ctx, otherID, fn.ID, map[string]string{"B": "2"})
	require.Error(t, err)
	vars, err := repo.GetVariables(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"A": "1"}, vars, "跨项目 SetVariables 不得影响本项目变量")

	// 跨项目 UpdateFunction 不得改名/更新。
	crossFn := *fn
	crossFn.ProjectID = otherID
	crossFn.Name = "hijacked"
	crossFn.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateFunction(ctx, &crossFn))
	got, err := repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "hello", got.Name, "跨项目 UpdateFunction 不得生效")

	// 跨项目 UpdateDeployment（同 function id）不得改状态。
	crossDep := *dep
	crossDep.ProjectID = otherID
	crossDep.Status = domainfunctions.DeploymentStatusFailed
	require.NoError(t, repo.UpdateDeployment(ctx, &crossDep))
	gotDep, err := repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, gotDep.Status, "跨项目 UpdateDeployment 不得生效")

	// 同项目内写路径仍正常（对照）。
	fn.Name = "renamed"
	fn.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateFunction(ctx, fn))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name)

	dep.Status = domainfunctions.DeploymentStatusFailed
	dep.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateDeployment(ctx, dep))
	gotDep, err = repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusFailed, gotDep.Status)

	require.NoError(t, repo.SetVariables(ctx, projectID, fn.ID, map[string]string{"C": "3"}))
	vars, err = repo.GetVariables(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"C": "3"}, vars)
}

func TestFunctionRepository_RecoverOrphanExecutions(t *testing.T) {
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

	stale := &domainfunctions.ExecutionRecord{
		ID:           "exe_stale",
		FunctionID:   fn.ID,
		ProjectID:    projectID,
		DeploymentID: dep.ID,
		Status:       domainfunctions.ExecutionStatusRunning,
		CreatedAt:    time.Now().Add(-2 * time.Hour),
		UpdatedAt:    time.Now().Add(-2 * time.Hour),
	}
	require.NoError(t, repo.CreateExecution(ctx, stale))
	fresh := &domainfunctions.ExecutionRecord{
		ID:           "exe_fresh",
		FunctionID:   fn.ID,
		ProjectID:    projectID,
		DeploymentID: dep.ID,
		Status:       domainfunctions.ExecutionStatusQueued,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
	require.NoError(t, repo.CreateExecution(ctx, fresh))

	n, err := repo.RecoverOrphanExecutionsInProject(ctx, projectID, time.Now().Add(-time.Hour), 500)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	got, err := repo.GetExecution(ctx, projectID, fn.ID, "exe_stale")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusFailed, got.Status)
	// P0.5：恢复错误文案统一为孤儿恢复语义（v1 曾为 "worker restarted"——
	// 同步快路径孤儿由 server 侧产生，文案不再绑定 worker）。
	require.Equal(t, "orphaned execution recovered", got.Error)

	got, err = repo.GetExecution(ctx, projectID, fn.ID, "exe_fresh")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusQueued, got.Status, "未过期的记录不受影响")

	otherID, _, otherCleanup := testutil.CreateTestProject(ctx, db)
	defer otherCleanup()
	otherFn := &domainfunctions.Function{
		ID: "fn_other", ProjectID: otherID, Name: "other", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateFunction(ctx, otherFn))
	otherDep := seedDeploymentRow(t, ctx, repo, otherFn, domainfunctions.DeploymentStatusReady)
	require.NoError(t, repo.CreateExecution(ctx, &domainfunctions.ExecutionRecord{
		ID: "exe_other", FunctionID: otherFn.ID, ProjectID: otherID, DeploymentID: otherDep.ID,
		Status:    domainfunctions.ExecutionStatusRunning,
		CreatedAt: time.Now().Add(-2 * time.Hour), UpdatedAt: time.Now().Add(-2 * time.Hour),
	}))
	n, err = repo.RecoverOrphanExecutionsInProject(ctx, projectID, time.Now().Add(-time.Hour), 500)
	require.NoError(t, err)
	require.Zero(t, n, "本项目已无孤儿")
	got, err = repo.GetExecution(ctx, otherID, otherFn.ID, "exe_other")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusRunning, got.Status, "不得扫到其它项目")

	// PR5 的回归守卫（查询 public.functions 计数为 0，断言"functions 不得再写
	// public"）已随基线重定退役：public.functions 幽灵表在 db/migrations 基线
	// 重定（2026-09-06）时整体移除，运行时唯一落点即项目 schema。
}

func TestFunctionRepository_InvalidProjectID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := bunrepo.NewFunctionRepository(nil)

	_, err := repo.GetFunction(ctx, "Bad-ID", "fn")
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	err = repo.SetVariables(ctx, "Bad-ID", "fn", map[string]string{"A": "1"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = repo.RecoverOrphanExecutionsInProject(ctx, "Bad-ID", time.Now(), 1)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	err = repo.PruneOldExecutionsInProject(ctx, "Bad-ID", "fn", 1)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
