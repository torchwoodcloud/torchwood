package functions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
)

// 运行时指定（docs/design/functions-runtime-selection.md）app 层测试：
// eol 状态门（CreateFunction / UpdateFunction / CreateDeployment 新构建
// 入口）与 deployment.runtime 快照列的写全语义。

// TestCreateFunction_EOLRuntimeRejected eol runtime 拒绝新建函数：错误
// 说明 EOL 并列出可选 active runtime（node-18.0 自 2025-04 上游 EOL）。
func TestCreateFunction_EOLRuntimeRejected(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateFunction(platformAdminCtx(), CreateFunctionCommand{
		ID: "fn_eol", ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "end-of-life")
	require.ErrorContains(t, err, "node-24.0", "错误文案须列出可选 runtime")

	// active runtime 照常创建；deprecated（如有）同样放行。
	fn, err := uc.CreateFunction(platformAdminCtx(), CreateFunctionCommand{
		ID: "fn_ok", ProjectID: "p1", Name: "f", Runtime: "node-24.0", TimeoutSeconds: timeoutPtr(15),
	})
	require.NoError(t, err)
	require.Equal(t, "node-24.0", fn.Runtime)
}

// TestUpdateFunction_Runtime 运行时可变更（§3）：eol 拒绝、合法变更落
// repo——生效语义 = 只影响后续新 deployment（存量 ready 部署不受影响）。
func TestUpdateFunction_Runtime(t *testing.T) {
	repo := newMockRepo()
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := platformAdminCtx()

	_, err := uc.CreateFunction(ctx, CreateFunctionCommand{
		ID: "fn_rt", ProjectID: "p1", Name: "f", Runtime: "node-22.0", TimeoutSeconds: timeoutPtr(15),
	})
	require.NoError(t, err)

	// eol runtime 拒绝。
	eol := "node-18.0"
	_, err = uc.UpdateFunction(ctx, UpdateFunctionCommand{ProjectID: "p1", FunctionID: "fn_rt", Runtime: &eol})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "end-of-life")

	// 未知 runtime 拒绝。
	bogus := "node-99.0"
	_, err = uc.UpdateFunction(ctx, UpdateFunctionCommand{ProjectID: "p1", FunctionID: "fn_rt", Runtime: &bogus})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "unsupported runtime")

	// 合法变更（含"从旧版本迁出"路径）落 repo。
	updated := "node-24.0"
	fn, err := uc.UpdateFunction(ctx, UpdateFunctionCommand{ProjectID: "p1", FunctionID: "fn_rt", Runtime: &updated})
	require.NoError(t, err)
	require.Equal(t, "node-24.0", fn.Runtime)

	stored, err := repo.GetFunction(ctx, "p1", "fn_rt")
	require.NoError(t, err)
	require.Equal(t, "node-24.0", stored.Runtime)
}

// TestCreateDeployment_EOLRuntimeRejected eol runtime 函数拒绝新的部署
// 构建（§6 状态门；存量 ready 部署执行面不受影响——本门只在新的构建
// 入口）。
func TestCreateDeployment_EOLRuntimeRejected(t *testing.T) {
	repo := newMockRepo()
	require.NoError(t, repo.CreateFunction(context.Background(), &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Runtime: "node-18.0", TimeoutSeconds: 10, Enabled: true,
	}))
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	_, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Code: []byte("PK\x03\x04zip"),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "end-of-life")
	require.Empty(t, repo.deployments, "状态门在落库前拒绝")
}

// TestCreateDeployment_RuntimeSnapshot 三个部署源分支都写全 runtime 快照
// （§4：INSERT 期写全、之后不可变——zip/git = fn.runtime，image = image）。
func TestCreateDeployment_RuntimeSnapshot(t *testing.T) {
	t.Run("zip", func(t *testing.T) {
		exec := newMockExecutor(nil, nil)
		repo := newMockRepo()
		require.NoError(t, repo.CreateFunction(context.Background(), &domainfunctions.Function{
			ID: "fn_1", ProjectID: "p1", Runtime: "node-24.0", TimeoutSeconds: 10, Enabled: true,
		}))
		uc := newTestUC(exec, repo, newMockQueue())

		dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
			ProjectID: "p1", FunctionID: "fn_1", Code: []byte("PK\x03\x04zip"),
		})
		require.NoError(t, err)
		require.Equal(t, "node-24.0", dep.Runtime)
		t.Cleanup(func() { _ = removeZip("p1", "fn_1", dep.ID) })

		stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
		require.NoError(t, err)
		require.Equal(t, "node-24.0", stored.Runtime)
	})

	t.Run("git", func(t *testing.T) {
		exec := newMockExecutor(nil, nil)
		uc, repo := gitTestUC(t, exec, newFakePacker())

		dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
			ProjectID: "p1", FunctionID: "fn_1", Git: gitSource(),
		})
		require.NoError(t, err)
		require.Equal(t, "node-24.0", dep.Runtime)

		stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
		require.NoError(t, err)
		require.Equal(t, "node-24.0", stored.Runtime)
	})

	t.Run("image", func(t *testing.T) {
		exec := newMockExecutor(nil, nil)
		exec.importDigest = sampleImageDigest
		uc, repo := imageTestUC(t, exec, "image")

		dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
			ProjectID: "p1", FunctionID: "fn_1", Image: imageSource(),
		})
		require.NoError(t, err)
		require.Equal(t, "image", dep.Runtime, "image 源快照 = image runtime ID")

		stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
		require.NoError(t, err)
		require.Equal(t, "image", stored.Runtime)
	})
}
