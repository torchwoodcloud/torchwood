package functions

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
)

// 本文件覆盖构建链一期接线（阶段 2/4，设计
// docs/design/functions-runtimes-and-sources.md §0/§1）：BuildSpec 载荷
// 组装、verify_build presence 语义、build_timeout 解析与构建 ctx 解耦
// （WithoutCancel，D11）。

// buildTestUC 组装可直接驱动 buildDeployment 的用例聚合（fn 预置到 repo）。
func buildTestUC(t *testing.T, exec *mockExecutor, fn *domainfunctions.Function, vars map[string]string, cfg *config.AppConfig) (*Functions, *mockRepo, *domainfunctions.Deployment) {
	t.Helper()
	repo := newMockRepo()
	require.NoError(t, repo.CreateFunction(context.Background(), fn))
	require.NoError(t, repo.SetVariables(context.Background(), fn.ProjectID, fn.ID, vars))
	dep := &domainfunctions.Deployment{
		ID:         "dep_1",
		FunctionID: fn.ID,
		ProjectID:  fn.ProjectID,
		Status:     domainfunctions.DeploymentStatusPending,
	}
	require.NoError(t, repo.CreateDeployment(context.Background(), dep))
	uc := NewFunctions(cfg, exec, repo, newMockQueue())
	return uc, repo, dep
}

// TestBuildDeployment_BuildSpecPayload 构建载荷断言：runtime/timeout 取
// fn 原值；Env 与执行链同源（sanitizeEnv + TW_API_BASE_URL 注入、无执行
// 身份 token）；EgressUntrusted 同执行分类；verify_build 未配置默认开启。
func TestBuildDeployment_BuildSpecPayload(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	fn := &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Runtime: "go-1.26",
		TimeoutSeconds: 30, ClientCallable: true, Enabled: true,
	}
	uc, _, dep := buildTestUC(t, exec, fn,
		map[string]string{"FOO": "bar", "BAD\nKEY": "dropped"},
		&config.AppConfig{Functions: &config.Functions{
			Execution: &config.Functions_Execution{ApiBaseUrl: "http://torchwood-server:9080"},
		}})

	require.NoError(t, uc.buildDeployment(context.Background(), fn, dep, t.TempDir()+"/code.zip"))
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	require.Len(t, exec.specs, 1)
	spec := exec.specs[0]
	require.Equal(t, "p1", spec.ProjectID)
	require.Equal(t, "fn_1", spec.FunctionID)
	require.Equal(t, "dep_1", spec.DeploymentID)
	require.Equal(t, "go-1.26", spec.Runtime, "runtime = fn.runtime 原值（D7 对账基准）")
	require.Equal(t, int64(30), spec.FunctionTimeoutSeconds, "函数超时原值（旧池 drain 宽限，D14）")
	require.Equal(t, map[string]string{
		"FOO":           "bar",
		twAPIBaseURLEnv: "http://torchwood-server:9080",
	}, spec.Env, "Env 与执行链同源组装（sanitizeEnv + api_base_url 注入）")
	require.NotContains(t, spec.Env, twExecutionTokenEnv, "构建/验证期无执行身份 token")
	require.True(t, spec.EgressUntrusted, "client_callable → untrusted（验证实例与执行同网，A1）")
	require.True(t, spec.Verify, "verify_build 未配置 = 默认开启（D10 presence 语义）")
}

// TestBuildDeployment_VerifyBuildPresence verify_build presence 表驱动：
// 段缺失 / 字段未设置 → 默认开启；显式 false → 关闭；显式 true → 开启
// （先例 security.rate_limit.enabled 的 presence 归一）。
func TestBuildDeployment_VerifyBuildPresence(t *testing.T) {
	cases := []struct {
		name       string
		dispatcher *config.Functions_Dispatcher
		want       bool
	}{
		{name: "dispatcher 段缺失", dispatcher: nil, want: true},
		{name: "字段未设置", dispatcher: &config.Functions_Dispatcher{}, want: true},
		{name: "显式 true", dispatcher: &config.Functions_Dispatcher{VerifyBuild: boolPtr(true)}, want: true},
		{name: "显式 false 关闭", dispatcher: &config.Functions_Dispatcher{VerifyBuild: boolPtr(false)}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := newMockExecutor(nil, nil)
			fn := &domainfunctions.Function{ID: "fn_1", ProjectID: "p1", Runtime: "node-18.0", TimeoutSeconds: 10, Enabled: true}
			uc, _, dep := buildTestUC(t, exec, fn, nil,
				&config.AppConfig{Functions: &config.Functions{Dispatcher: tc.dispatcher}})
			require.NoError(t, uc.buildDeployment(context.Background(), fn, dep, t.TempDir()+"/code.zip"))
			require.Len(t, exec.specs, 1)
			require.Equal(t, tc.want, exec.specs[0].Verify)
		})
	}
}

// TestBuildDeployment_BuildTimeoutParsing build_timeout 解析：空/非法回落
// 5m，配置值生效（以构建 ctx 的 deadline 断言）。
func TestBuildDeployment_BuildTimeoutParsing(t *testing.T) {
	cases := []struct {
		name    string
		timeout string
		wantMin time.Duration // 预算下界（ scheduling 余量）
		wantMax time.Duration // 预算上界
	}{
		{name: "未配置回落 5m", timeout: "", wantMin: 4 * time.Minute, wantMax: 5 * time.Minute},
		{name: "非法值回落 5m", timeout: "not-a-duration", wantMin: 4 * time.Minute, wantMax: 5 * time.Minute},
		{name: "配置值生效", timeout: "90s", wantMin: 80 * time.Second, wantMax: 90 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := newMockExecutor(nil, nil)
			exec.buildFn = func(ctx context.Context, _ domainfunctions.BuildSpec) error {
				dl, ok := ctx.Deadline()
				require.True(t, ok, "构建 ctx 必须带 build_timeout 封顶 deadline")
				remaining := time.Until(dl)
				require.LessOrEqual(t, remaining, tc.wantMax)
				require.Greater(t, remaining, tc.wantMin)
				return nil
			}
			fn := &domainfunctions.Function{ID: "fn_1", ProjectID: "p1", Runtime: "go-1.26", TimeoutSeconds: 10, Enabled: true}
			uc, _, dep := buildTestUC(t, exec, fn, nil,
				&config.AppConfig{Functions: &config.Functions{
					Dispatcher: &config.Functions_Dispatcher{BuildTimeout: tc.timeout},
				}})
			require.NoError(t, uc.buildDeployment(context.Background(), fn, dep, t.TempDir()+"/code.zip"))
			require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
		})
	}
}

// TestBuildDeployment_SurvivesParentCancel ctx 解耦（D11）：请求 ctx 在构建
// 途中被取消，构建在 WithoutCancel 预算上继续完成、状态照常落库 ready。
func TestBuildDeployment_SurvivesParentCancel(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exec.buildFn = func(ctx context.Context, _ domainfunctions.BuildSpec) error {
		cancel() // 模拟客户端断开
		time.Sleep(10 * time.Millisecond)
		require.NoError(t, ctx.Err(), "构建 ctx 必须脱离请求 ctx 生命周期（WithoutCancel）")
		dl, ok := ctx.Deadline()
		require.True(t, ok, "WithoutCancel 之上仍须有 build_timeout 封顶")
		require.Less(t, time.Until(dl), 5*time.Minute+time.Second)
		return nil
	}
	fn := &domainfunctions.Function{ID: "fn_1", ProjectID: "p1", Runtime: "go-1.26", TimeoutSeconds: 10, Enabled: true}
	uc, repo, dep := buildTestUC(t, exec, fn, nil, &config.AppConfig{})
	require.NoError(t, uc.buildDeployment(parentCtx, fn, dep, t.TempDir()+"/code.zip"))
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)

	// 状态照常落库（父 ctx 已取消，落库走 buildCtx）。
	stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_1")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, stored.Status, "客户端断开后状态照常落库")
	require.Equal(t, "dep_1", fn.LatestReadyDeploymentID, "ActivateDeployment 照常维护 latest 指针")
}

// TestBuildDeployment_FailureCleanup 构建失败分支：failed 落库 + zip 清理 +
// 镜像清理（RemoveImage 走 buildCtx），返回 nil（状态机内收敛，不向上抛）。
func TestBuildDeployment_FailureCleanup(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.buildErr = context.DeadlineExceeded
	fn := &domainfunctions.Function{ID: "fn_1", ProjectID: "p1", Runtime: "go-1.26", TimeoutSeconds: 10, Enabled: true}
	uc, repo, dep := buildTestUC(t, exec, fn, nil, &config.AppConfig{})
	// zip 落真实 zip 根（writeZip/assertZipDir 只放行 torchwood-functions 前缀）。
	zipPath := zipPath("p1", "fn_1", "dep_1")
	t.Cleanup(func() { _ = removeZip("p1", "fn_1", "dep_1") })
	require.NoError(t, writeZip(zipPath, []byte("PK\x03\x04")))
	require.NoError(t, uc.buildDeployment(context.Background(), fn, dep, zipPath))
	require.Equal(t, domainfunctions.DeploymentStatusFailed, dep.Status)
	require.NotEmpty(t, dep.Error)

	stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_1")
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusFailed, stored.Status)
	require.Equal(t, 1, exec.removes, "失败分支清理构建产物镜像")
	require.NoFileExists(t, zipPath, "zip 源维持构建失败即删（D13 一期形态）")
}
