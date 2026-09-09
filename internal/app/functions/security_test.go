package functions

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/pkg/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCreateFunction_RejectsMaliciousIDs(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	for _, id := range []string{
		"../../etc/passwd", // 路径穿越
		"..",
		"a/b",
		`a\b`,
		"a:b",
		"a b",
		"fn.x",
		"-bad",                  // 必须以字母数字开头
		"_bad",                  // 下划线不能开头
		"Fn-1",                  // 大写非法（Docker 镜像名只允许小写，G6-3）
		strings.Repeat("a", 65), // 超长
	} {
		_, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: id, ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15),
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "id %q 应被拒绝", id)
	}

	// 合法 ID 仍可创建（全小写）；runtimes/specifications 在 REST 自定义动词
	// 迁移（R10-P1-3/B3）后不再是保留字，可作 function id 合法创建。
	for _, id := range []string{"fn_1", "fn-1", "a", "0", "runtimes", "specifications", strings.Repeat("a", 64)} {
		fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: id, ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15),
		})
		require.NoError(t, err, "id %q 应被接受", id)
		require.Equal(t, id, fn.ID)
	}
}

// TestCreateFunction_FormerReservedIDsAreRegularIDs：REST 自定义动词迁移
// （R10-P1-3/B3）后，旧字面量路由保留字（runtimes/specifications）成为合法
// function_id，创建后可按普通函数读取。
func TestCreateFunction_FormerReservedIDsAreRegularIDs(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	for _, id := range []string{"runtimes", "specifications"} {
		fn, err := uc.CreateFunction(ctx, CreateFunctionCommand{
			ID: id, ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15),
		})
		require.NoError(t, err, "function id %q 应可正常创建", id)
		require.Equal(t, id, fn.ID)

		got, err := uc.GetFunction(ctx, "p1", id)
		require.NoError(t, err)
		require.Equal(t, id, got.ID)
	}
}

func TestZipPath_SanitizesTraversalComponents(t *testing.T) {
	cases := []struct {
		projectID, functionID, deploymentID string
		wantSub                             string
	}{
		{"p1", "../../etc", "d1", filepath.Join("p1", "etc", "d1.zip")},
		{"p1", "a/b", "d2", filepath.Join("p1", "b", "d2.zip")},
		{"../evil", "..", "d3", filepath.Join("evil", "..", "d3.zip")},
	}
	for _, c := range cases {
		got := zipPath(c.projectID, c.functionID, c.deploymentID)
		want := filepath.Join(zipRoot(), filepath.FromSlash(c.wantSub))
		require.Equal(t, filepath.Clean(want), filepath.Clean(got))
		require.NoError(t, assertZipDir(got), "消毒后的路径必须在 zip 根目录内")
	}
}

func TestWriteZip_RejectsEscapingPath(t *testing.T) {
	// 绕过 zipPath 直接构造逃逸路径 → 必须拒绝且不落盘。
	escaped := filepath.Join(os.TempDir(), "torchwood-escape-test", "x.zip")
	err := writeZip(escaped, []byte("PK\x03\x04payload"))
	require.Error(t, err)
	require.ErrorContains(t, err, "escapes functions root")
	_, statErr := os.Stat(escaped)
	require.Error(t, statErr, "逃逸路径不得写入文件")
}

func TestWriteZipRemoveZip_RoundTripInRoot(t *testing.T) {
	path := zipPath("p1", "fn_1", "d1")
	defer func() { _ = os.RemoveAll(filepath.Join(zipRoot(), "p1")) }()
	require.NoError(t, writeZip(path, []byte("PK\x03\x04payload")))
	content, err := os.ReadFile(path) // #nosec G304 -- 路径来自仓库内测试数据
	require.NoError(t, err)
	require.Equal(t, "PK\x03\x04payload", string(content))
	require.NoError(t, removeZip("p1", "fn_1", "d1"))
	_, statErr := os.Stat(path)
	require.Error(t, statErr, "删除后文件不存在")
}

func TestGetDeployment_RejectsCrossProject(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	seedReadyFunction(repo, "p2", "fn_2", true, 15)
	seedReadyFunction(repo, "p1", "fn_1", true, 15)

	// 函数不在 project p1 → NotFound。
	_, err := uc.GetDeployment(ctx, "p1", "fn_2", "dep_ready")
	require.Equal(t, codes.NotFound, status.Code(err))

	// 部署属于其他项目（同 functionID）→ NotFound。
	require.NoError(t, repo.CreateDeployment(ctx, &domainfunctions.Deployment{
		ID: "dep_cross", FunctionID: "fn_1", ProjectID: "p2", Status: domainfunctions.DeploymentStatusReady,
	}))
	_, err = uc.GetDeployment(ctx, "p1", "fn_1", "dep_cross")
	require.Equal(t, codes.NotFound, status.Code(err))

	// 同项目内正常访问。
	dep, err := uc.GetDeployment(ctx, "p1", "fn_1", "dep_ready")
	require.NoError(t, err)
	require.Equal(t, "dep_ready", dep.ID)
}

func TestDeleteDeployment_RejectsCrossProject(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p2", "fn_2", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	err := uc.DeleteDeployment(ctx, "p1", "fn_2", "dep_ready")
	require.Equal(t, codes.NotFound, status.Code(err))

	got, err := repo.GetDeployment(ctx, "p2", "fn_2", "dep_ready")
	require.NoError(t, err)
	require.NotNil(t, got, "跨项目删除不得生效")

	require.NoError(t, uc.DeleteDeployment(ctx, "p2", "fn_2", "dep_ready"))
	got, err = repo.GetDeployment(ctx, "p2", "fn_2", "dep_ready")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestGetVariables_MasksSecrets(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	require.NoError(t, repo.SetVariables(context.Background(), fn.ProjectID, fn.ID, map[string]string{
		"SECRET": "s3cr3t",
		"EMPTY":  "",
	}))
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	vars, err := uc.GetVariables(context.Background(), "p1", "fn_1")
	require.NoError(t, err)
	require.Equal(t, "******", vars["SECRET"], "secret 值必须脱敏")
	require.Equal(t, "", vars["EMPTY"], "空值保持空串")

	// 内部真实值不受影响（execution 路径直接走 repo）。
	raw, err := repo.GetVariables(context.Background(), "p1", "fn_1")
	require.NoError(t, err)
	require.Equal(t, "s3cr3t", raw["SECRET"])
}

func TestSetVariables_MissingFunctionNotFound(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.SetVariables(platformAdminCtx(), "p1", "nope", map[string]string{"A": "1"})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestGetVariables_MissingFunctionNotFound(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.GetVariables(context.Background(), "p1", "nope")
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestCreateExecution_CombinedDataAndEnvBudget(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	big := strings.Repeat("v", 20<<10)
	require.NoError(t, repo.SetVariables(context.Background(), fn.ProjectID, fn.ID, map[string]string{"BIG": big}))
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	// 各自单独都未超限（20KB env + 20KB data），合并后超 32KB → 拒绝。
	_, err := uc.CreateExecution(platformAdminCtx(), CreateExecutionCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		Data: `{"v":"` + strings.Repeat("v", 20<<10) + `"}`,
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "combined maximum")
}

func TestCreateDeployment_BuildSemaphoreFullCleansUp(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	// 占满构建信号量（W-F 全局配额改为实例级，需通过注入的 semaphore 模拟）。
	sem, releases := newFullSemaphore(maxConcurrentBuilds)
	defer func() {
		for _, r := range releases {
			r()
		}
	}()
	uc.WithSemaphores(sem, nil)

	_, err := uc.CreateDeployment(platformAdminCtx(), CreateDeploymentCommand{
		ProjectID:  "p1",
		FunctionID: "fn_1",
		Code:       []byte("PK\x03\x04code"),
	})
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.Len(t, repo.deployments, 1, "仅剩 seed 的 ready 部署，pending 行必须已删除")
	for _, d := range repo.deployments {
		require.Equal(t, domainfunctions.DeploymentStatusReady, d.Status)
	}

	zips, globErr := filepath.Glob(filepath.Join(zipRoot(), "p1", "fn_1", "*.zip"))
	require.NoError(t, globErr)
	require.Empty(t, zips, "信号量满时必须清理本地 zip")
	_ = os.RemoveAll(filepath.Join(zipRoot(), "p1"))
}

func TestProcessExecutionPayload_RejectsMalformed(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	err := uc.ProcessExecutionPayload(context.Background(), []byte("not json"))
	require.ErrorIs(t, err, ErrInvalidQueuePayload)

	err = uc.ProcessExecutionPayload(context.Background(), []byte(`{"execution_id":"e1"}`))
	require.ErrorIs(t, err, ErrInvalidQueuePayload)
}

func TestMarkExecutionFailed(t *testing.T) {
	repo := newMockRepo()
	fn := seedReadyFunction(repo, "p1", "fn_1", true, 15)
	rec := &domainfunctions.ExecutionRecord{
		ID: "e1", FunctionID: fn.ID, ProjectID: fn.ProjectID, DeploymentID: "dep_ready",
		Status: domainfunctions.ExecutionStatusQueued,
	}
	require.NoError(t, repo.CreateExecution(context.Background(), rec))
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	require.NoError(t, uc.MarkExecutionFailed(context.Background(), "p1", "fn_1", "e1", "worker retries exhausted"))
	got, err := repo.GetExecution(context.Background(), "p1", "fn_1", "e1")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusFailed, got.Status)
	require.Equal(t, "worker retries exhausted", got.Error)
}

func newFullSemaphore(max int) (*semaphore.InMemorySemaphore, []func()) {
	sem := semaphore.NewInMemory(max)
	var releases []func()
	for i := 0; i < max; i++ {
		ok, rel, _ := sem.TryAcquire(context.Background())
		if ok {
			releases = append(releases, rel)
		}
	}
	return sem, releases
}
