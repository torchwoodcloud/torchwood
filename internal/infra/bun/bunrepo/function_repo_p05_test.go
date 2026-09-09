package bunrepo_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

// seedP05Function 构造带池策略的函数（P0.5 列回读断言用）。
func seedP05Function(t *testing.T, ctx context.Context, repo domainfunctions.FunctionRepo, projectID, fnID string) *domainfunctions.Function {
	t.Helper()
	now := time.Now()
	fn := &domainfunctions.Function{
		ID:                     fnID,
		ProjectID:              projectID,
		Name:                   "p05",
		Runtime:                "node-18.0",
		Entrypoint:             "index.main",
		TimeoutSeconds:         15,
		Spec:                   "shared-1x",
		Enabled:                true,
		MinInstances:           1,
		MaxInstances:           4,
		IdleTTLSeconds:         60,
		MaxRequestsPerInstance: 77,
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))
	return fn
}

// TestFunctionRepository_P05PoolPolicyColumns 池策略列持久化与回读（迁移
// 000014；列 DEFAULT 与 domain 零值一致性由 CreateFunction 不传时落库默认）。
func TestFunctionRepository_P05PoolPolicyColumns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewFunctionRepository(db)

	fn := seedP05Function(t, ctx, repo, projectID, "fn_pool")
	got, err := repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, 1, got.MinInstances)
	require.Equal(t, 4, got.MaxInstances)
	require.Equal(t, 60, got.IdleTTLSeconds)
	require.Equal(t, 77, got.MaxRequestsPerInstance)
	require.Empty(t, got.LatestReadyDeploymentID, "指针初始为空")

	// 平台默认（未传池策略 → 列 DEFAULT：0/2/300/1000，CHECK 下限兜底）。
	now := time.Now()
	def := &domainfunctions.Function{
		ID: "fn_def", ProjectID: projectID, Name: "def", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateFunction(ctx, def))
	gotDef, err := repo.GetFunction(ctx, projectID, def.ID)
	require.NoError(t, err)
	require.Equal(t, 0, gotDef.MinInstances)
	require.Equal(t, 2, gotDef.MaxInstances)
	require.Equal(t, 300, gotDef.IdleTTLSeconds)
	require.Equal(t, 1000, gotDef.MaxRequestsPerInstance)

	// CHECK 约束（迁移 000014）：min<0 / max<1 拒绝落库。
	bad := &domainfunctions.Function{
		ID: "fn_bad", ProjectID: projectID, Name: "bad", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		MinInstances: -1, CreatedAt: now, UpdatedAt: now,
	}
	require.Error(t, repo.CreateFunction(ctx, bad), "min_instances >= 0 CHECK")
}

// TestFunctionRepository_TwoWritePrebook 同步快路径两写预占记账（§6 约束①/
// K9）：INSERT (running, timeout 快照) 直接预占 → 预占行执行中可见 → 终态
// UPDATE；全程无 queued/building 中间态。
func TestFunctionRepository_TwoWritePrebook(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewFunctionRepository(db)

	fn := seedP05Function(t, ctx, repo, projectID, "fn_pre")
	dep := seedDeploymentRow(t, ctx, repo, fn, domainfunctions.DeploymentStatusReady)

	timeoutSnapshot := fn.TimeoutSeconds
	rec := &domainfunctions.ExecutionRecord{
		ID:             "exe_pre",
		FunctionID:     fn.ID,
		ProjectID:      projectID,
		DeploymentID:   dep.ID,
		Status:         domainfunctions.ExecutionStatusRunning, // 预占态直插
		TimeoutSeconds: &timeoutSnapshot,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, repo.CreateExecution(ctx, rec))

	// 预占行即刻可见（审计/限频计数依据；执行中崩溃留在 running）。
	visible, err := repo.GetExecution(ctx, projectID, fn.ID, rec.ID)
	require.NoError(t, err)
	require.NotNil(t, visible)
	require.Equal(t, domainfunctions.ExecutionStatusRunning, visible.Status)
	require.NotNil(t, visible.TimeoutSeconds)
	require.Equal(t, 15, *visible.TimeoutSeconds, "timeout 快照随预占行落库")

	// 第二写：终态 UPDATE（completed + outputs + duration_ms）。
	visible.Status = domainfunctions.ExecutionStatusCompleted
	visible.Response = `{"ok":1}`
	visible.DurationMS = 42
	visible.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateExecution(ctx, visible))

	final, err := repo.GetExecution(ctx, projectID, fn.ID, rec.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, final.Status)
	require.Equal(t, int64(42), final.DurationMS)
	// UpdateExecution 白名单不含 timeout_seconds：快照列对终态 UPDATE 只读。
	require.NotNil(t, final.TimeoutSeconds)
	require.Equal(t, 15, *final.TimeoutSeconds)
}

// TestFunctionRepository_OrphanRecoveryP05 周期孤儿恢复（P0.5）：扫描范围
// 含 queued；staleAfter 按行判定（timeout 快照 + 120s 宽限；NULL 回退 1h）。
func TestFunctionRepository_OrphanRecoveryP05(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewFunctionRepository(db)

	fn := seedP05Function(t, ctx, repo, projectID, "fn_orph")
	dep := seedDeploymentRow(t, ctx, repo, fn, domainfunctions.DeploymentStatusReady)

	quoted := testutil.CatalogQuoted(projectID)
	seed := func(id, status string, timeout *int, age time.Duration) {
		now := time.Now()
		rec := &domainfunctions.ExecutionRecord{
			ID: id, FunctionID: fn.ID, ProjectID: projectID, DeploymentID: dep.ID,
			Status: status, TimeoutSeconds: timeout,
			CreatedAt: now.Add(-age), UpdatedAt: now.Add(-age),
		}
		require.NoError(t, repo.CreateExecution(ctx, rec))
		// CreateExecution 的 updated_at 取 Now()，人为回拨以模拟滞留时长。
		_, err := db.ExecContext(ctx, fmt.Sprintf(
			`UPDATE %s.function_executions SET created_at = NOW() - INTERVAL '%d seconds', updated_at = NOW() - INTERVAL '%d seconds' WHERE id = '%s'`,
			quoted, int(age.Seconds()), int(age.Seconds()), id))
		require.NoError(t, err)
	}

	snap := 5
	// 超时快照 5s + 120s 宽限 < 滞留 3min → 回收。
	seed("orph_running_snap", domainfunctions.ExecutionStatusRunning, &snap, 3*time.Minute)
	// 快照行但滞留 30s（< 宽限）→ 保留。
	seed("orph_running_fresh", domainfunctions.ExecutionStatusRunning, &snap, 30*time.Second)
	// NULL 快照 + 滞留 2h > 1h 回退 → 回收。
	seed("orph_queued_legacy", domainfunctions.ExecutionStatusQueued, nil, 2*time.Hour)
	// NULL 快照 + 滞留 10min（< 1h）→ 保留。
	seed("orph_building_recent", domainfunctions.ExecutionStatusBuilding, nil, 10*time.Minute)

	recovered, err := repo.RecoverOrphanExecutionsInProject(ctx, projectID, time.Now().Add(-time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, int64(2), recovered, "快照超时行 + NULL 超期行各回收一条")

	for _, id := range []string{"orph_running_snap", "orph_queued_legacy"} {
		got, err := repo.GetExecution(ctx, projectID, fn.ID, id)
		require.NoError(t, err)
		require.Equal(t, domainfunctions.ExecutionStatusFailed, got.Status, id)
		require.Equal(t, "orphaned execution recovered", got.Error)
	}
	for _, id := range []string{"orph_running_fresh", "orph_building_recent"} {
		got, err := repo.GetExecution(ctx, projectID, fn.ID, id)
		require.NoError(t, err)
		require.NotEqual(t, domainfunctions.ExecutionStatusFailed, got.Status, id)
	}
}

// TestFunctionRepository_LatestReadyDeploymentPointer latest_ready_deployment_id
// 的事务维护：ready 激活置指针；删除指向行清指针；删除非指向行不动指针。
func TestFunctionRepository_LatestReadyDeploymentPointer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewFunctionRepository(db)

	fn := seedP05Function(t, ctx, repo, projectID, "fn_ptr")
	now := time.Now()
	mkDep := func(id string) *domainfunctions.Deployment {
		return &domainfunctions.Deployment{
			ID: id, FunctionID: fn.ID, ProjectID: projectID, Size: 1,
			Status: domainfunctions.DeploymentStatusBuilding, CreatedAt: now, UpdatedAt: now,
		}
	}
	depA := mkDep("dep_a")
	depB := mkDep("dep_b")
	require.NoError(t, repo.CreateDeployment(ctx, depA))
	require.NoError(t, repo.CreateDeployment(ctx, depB))

	// 激活 depA → 指针指向 A。
	depA.Status = domainfunctions.DeploymentStatusReady
	require.NoError(t, repo.ActivateDeployment(ctx, depA))
	got, err := repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "dep_a", got.LatestReadyDeploymentID)

	// 激活 depB → 指针切换到 B（最新 ready）。
	depB.Status = domainfunctions.DeploymentStatusReady
	require.NoError(t, repo.ActivateDeployment(ctx, depB))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "dep_b", got.LatestReadyDeploymentID)

	// 删除非指向行（depA）→ 指针不动。
	require.NoError(t, repo.DeleteDeployment(ctx, projectID, fn.ID, "dep_a"))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, "dep_b", got.LatestReadyDeploymentID)

	// 删除指向行（depB）→ 同事务清空指针（NULL 回退全量列表逻辑）。
	require.NoError(t, repo.DeleteDeployment(ctx, projectID, fn.ID, "dep_b"))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Empty(t, got.LatestReadyDeploymentID)
}
