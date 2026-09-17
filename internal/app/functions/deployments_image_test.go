package functions

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件覆盖 BYO 镜像部署源三期阶段 1/4 的 app 层（设计
// docs/design/functions-runtimes-and-sources.md §0「源/运行时互斥」/§3）：
// D7 双向互斥校验、image 分支状态机（首次 ImportImage → INSERT
//（source_ref=digest、template_version=0）→ buildDeployment image 分流复检 →
// ready）、失败清理分流与 worker 补构建分流（ProcessExecution 非 ready 路径
// → 幂等 ImportImage）。

// sampleImageDigest 是测试用钉死 digest 样例（mock executor 默认返回值同形）。
const sampleImageDigest = "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// imageSource 是测试用镜像源样板（一次性凭证随结构体）。
func imageSource() *domainfunctions.ImageSource {
	return &domainfunctions.ImageSource{
		Reference:        "registry.example.com/acme/greet:v1",
		RegistryUsername: "bot",
		RegistryToken:    "one-shot-registry-token",
	}
}

// imageTestUC 组装镜像源分支用例聚合（fn 以给定 runtime 预置到 repo）。
func imageTestUC(t *testing.T, exec *mockExecutor, runtime string) (*Functions, *mockRepo) {
	t.Helper()
	repo := newMockRepo()
	require.NoError(t, repo.CreateFunction(context.Background(), &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Runtime: runtime, TimeoutSeconds: 20, Enabled: true,
	}))
	uc := NewFunctions(&config.AppConfig{}, exec, repo, newMockQueue())
	return uc, repo
}

// TestCreateDeployment_SourceRuntimeMutualExclusion 源/运行时互斥（D7 双向，
// 设计 §0）表驱动：image runtime 只收 image 源；node/go runtime 拒绝 image
// 源；image runtime 拒绝 zip/git 源。错误为 InvalidArgument 且明示两侧。
func TestCreateDeployment_SourceRuntimeMutualExclusion(t *testing.T) {
	cases := []struct {
		name      string
		runtime   string
		source    string // zip | git | image
		wantOK    bool
		notCalled string // 互斥拒绝时不得触达的端口观测点
	}{
		{name: "image runtime + image 源 放行", runtime: "image", source: "image", wantOK: true},
		{name: "image runtime + zip 源 拒绝", runtime: "image", source: "zip", notCalled: "build"},
		{name: "image runtime + git 源 拒绝", runtime: "image", source: "git", notCalled: "pack"},
		{name: "node runtime + image 源 拒绝", runtime: "node-18.0", source: "image", notCalled: "import"},
		{name: "go runtime + image 源 拒绝", runtime: "go-1.26", source: "image", notCalled: "import"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exec := newMockExecutor(nil, nil)
			packer := newFakePacker()
			uc, repo := imageTestUC(t, exec, tc.runtime)
			uc.packer = packer

			cmd := CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1"}
			switch tc.source {
			case "zip":
				cmd.Code = []byte("PK\x03\x04fake-zip")
			case "git":
				cmd.Git = gitSource()
			case "image":
				cmd.Image = imageSource()
			}
			dep, err := uc.CreateDeployment(serverCtx(), cmd)
			if tc.wantOK {
				require.NoError(t, err)
				require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
				t.Cleanup(func() { _ = removeZip("p1", "fn_1", dep.ID) })
				return
			}
			require.Nil(t, dep)
			require.Equal(t, codes.InvalidArgument, status.Code(err))
			// 错误明示两侧（source 与 runtime 取值都在文案中）。
			require.ErrorContains(t, err, `"`+tc.source+`"`)
			require.ErrorContains(t, err, `"`+tc.runtime+`"`)
			require.Empty(t, repo.deployments, "互斥拒绝不得落行")
			switch tc.notCalled {
			case "build":
				require.Zero(t, exec.builds, "互斥拒绝不得进入构建")
			case "pack":
				require.Empty(t, packer.got, "互斥拒绝不得调用 packer")
			case "import":
				require.Zero(t, exec.imports, "互斥拒绝不得调用 ImportImage")
			}
		})
	}
}

// TestCreateDeployment_ImageSuccess 成功主链路（状态机）：首次 ImportImage
// （带一次性凭证、无预期 digest）→ INSERT（source_ref=digest、
// template_version=0、source_url=原始引用、source_dir 恒空、size=0）→
// buildDeployment image 分流复检（预期 digest = 行内 source_ref、凭证为空）→
// ready。无 zip 落盘、Build 不被调用。
func TestCreateDeployment_ImageSuccess(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.importDigest = sampleImageDigest
	uc, repo := imageTestUC(t, exec, "image")

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Image: imageSource(),
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	require.Zero(t, exec.builds, "image 源不走 Build")

	// 两次 ImportImage：首次（落库前）带凭证无预期 digest；复检（ready 门禁）
	// 预期 digest = 钉死值、凭证为空（一次性凭证不落库）。
	require.Equal(t, 2, exec.imports)
	first, recheck := exec.importSpecs[0], exec.importSpecs[1]
	require.Equal(t, "registry.example.com/acme/greet:v1", first.Reference)
	require.Equal(t, "bot", first.RegistryUsername)
	require.Equal(t, "one-shot-registry-token", first.RegistryToken)
	require.Empty(t, first.ExpectedDigest, "首次导入无预期 digest")
	require.Equal(t, "p1", first.ProjectID)
	require.Equal(t, "fn_1", first.FunctionID)
	require.Equal(t, dep.ID, first.DeploymentID)
	require.Equal(t, int64(20), first.FunctionTimeoutSeconds, "函数超时随 spec（旧池 drain）")
	require.Equal(t, sampleImageDigest, recheck.ExpectedDigest, "复检带预期 digest（幂等锚）")
	require.Equal(t, "registry.example.com/acme/greet:v1", recheck.Reference, "复检引用 = 行内 source_url")
	require.Empty(t, recheck.RegistryUsername, "复检凭证恒空（D8）")
	require.Empty(t, recheck.RegistryToken)

	require.Equal(t, domainfunctions.DeploymentSourceImage, dep.SourceType)
	require.Equal(t, "registry.example.com/acme/greet:v1", dep.SourceURL)
	require.Equal(t, sampleImageDigest, dep.SourceRef, "source_ref = 钉死 digest")
	require.Empty(t, dep.SourceDir, "image 源 source_dir 恒空")
	require.Zero(t, dep.Size, "image 源无 zip 字节流")
	require.Equal(t, int32(0), dep.TemplateVersion, "template_version=0（未知模板，D12 并发降级）")

	require.NoFileExists(t, zipPath("p1", "fn_1", dep.ID), "image 源不写 zipPath")

	stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, stored.Status)
	require.Equal(t, domainfunctions.DeploymentSourceImage, stored.SourceType)
	require.Equal(t, sampleImageDigest, stored.SourceRef)
	require.Equal(t, int32(0), stored.TemplateVersion, "template_version=0 落库")
}

// TestCreateDeployment_ImageFirstImportFailureNoRow 首次 ImportImage 失败
// （INSERT 之前）：错误透传、无行无残留（与 git pack 失败同构）。
func TestCreateDeployment_ImageFirstImportFailureNoRow(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.importErr = status.Error(codes.InvalidArgument, "manifest unknown")
	uc, repo := imageTestUC(t, exec, "image")

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Image: imageSource(),
	})
	require.Nil(t, dep)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, 1, exec.imports, "失败发生在首次导入")
	require.Empty(t, repo.deployments, "导入失败无行（source 列不可变 → INSERT 前移）")
	require.Zero(t, exec.removes)
}

// TestCreateDeployment_ImageInsertFailureCleansImage INSERT 失败：清理已导入
// 的平台镜像（RemoveImage 幂等兜底），无行。
func TestCreateDeployment_ImageInsertFailureCleansImage(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, repo := imageTestUC(t, exec, "image")
	repo.mu.Lock()
	repo.createDeploymentErr = errors.New("db down")
	repo.mu.Unlock()

	_, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Image: imageSource(),
	})
	require.Error(t, err)
	require.Empty(t, repo.deployments, "INSERT 失败无行")
	require.Equal(t, 1, exec.removes, "INSERT 失败清理已导入镜像")
}

// TestCreateDeployment_ImageRecheckFailureConvergesFailed 复检（ready 门禁）
// 失败：状态机内收敛 failed（不向上抛），failed 行保留 + 镜像清理。
func TestCreateDeployment_ImageRecheckFailureConvergesFailed(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	exec.importFn = func(spec domainfunctions.ImportImageSpec) (string, error) {
		if spec.ExpectedDigest == "" {
			return sampleImageDigest, nil // 首次导入成功
		}
		return "", context.DeadlineExceeded // 复检超时
	}
	uc, repo := imageTestUC(t, exec, "image")

	dep, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Image: imageSource(),
	})
	require.NoError(t, err, "构建失败在状态机内收敛，不向上抛")
	require.Equal(t, domainfunctions.DeploymentStatusFailed, dep.Status)
	require.NotEmpty(t, dep.Error)
	require.Equal(t, 2, exec.imports)
	require.Equal(t, 1, exec.removes, "失败分支清理构建产物镜像")

	stored, err := repo.GetDeployment(context.Background(), "p1", "fn_1", dep.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusFailed, stored.Status, "failed 行保留")
	require.Equal(t, domainfunctions.DeploymentSourceImage, stored.SourceType, "source 列不可变：failed 后投影仍在")
	require.Equal(t, sampleImageDigest, stored.SourceRef)
}

// TestCreateDeployment_ImageShapeValidation 镜像源形状校验（server 侧纵深
// 复核层）：引用非空 + 长度上限（完整 host guard 在阶段 2 dispatcher 侧）；
// 坏形状不得触达 executor。
func TestCreateDeployment_ImageShapeValidation(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, repo := imageTestUC(t, exec, "image")

	_, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		Image: &domainfunctions.ImageSource{Reference: ""},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "reference is required")

	long := make([]byte, 501)
	for i := range long {
		long[i] = 'a'
	}
	_, err = uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1",
		Image: &domainfunctions.ImageSource{Reference: string(long)},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "exceeds 500 bytes")

	require.Zero(t, exec.imports, "坏形状不得调用 ImportImage")
	require.Empty(t, repo.deployments)
}

// TestCreateDeployment_ImageImportSpecPayload ImportImageSpec 载荷断言：Env
// 与执行链同源组装（sanitizeEnv + TW_API_BASE_URL 注入、无执行身份 token）、
// EgressUntrusted 同执行分类。
func TestCreateDeployment_ImageImportSpecPayload(t *testing.T) {
	exec := newMockExecutor(nil, nil)
	uc, repo := imageTestUC(t, exec, "image")
	require.NoError(t, repo.SetVariables(context.Background(), "p1", "fn_1",
		map[string]string{"FOO": "bar", "BAD\nKEY": "dropped"}))
	uc.cfg = &config.AppConfig{Functions: &config.Functions{
		Execution: &config.Functions_Execution{ApiBaseUrl: "http://torchwood-server:9080"},
	}}

	_, err := uc.CreateDeployment(serverCtx(), CreateDeploymentCommand{
		ProjectID: "p1", FunctionID: "fn_1", Image: imageSource(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, exec.importSpecs)
	spec := exec.importSpecs[0]
	require.Equal(t, map[string]string{
		"FOO":           "bar",
		twAPIBaseURLEnv: "http://torchwood-server:9080",
	}, spec.Env, "Env 与执行链同源组装（验证 spawn 携带 variables）")
	require.NotContains(t, spec.Env, twExecutionTokenEnv, "构建/验证期无执行身份 token")
	require.False(t, spec.EgressUntrusted, "无 client_callable/触发器 → 可信")
}

// TestProcessExecution_ImageDeploymentRebuildViaImport worker 补构建分流
// （设计 §3「worker 补构建分流」）：image 源非 ready deployment → 幂等
// ImportImage（预期 digest = 行内 source_ref、凭证为空）→ ready → 执行完成。
func TestProcessExecution_ImageDeploymentRebuildViaImport(t *testing.T) {
	repo := newMockRepo()
	fn := &domainfunctions.Function{
		ID: "fn_1", ProjectID: "p1", Runtime: "image", TimeoutSeconds: 20, Enabled: true,
	}
	require.NoError(t, repo.CreateFunction(context.Background(), fn))
	require.NoError(t, repo.CreateDeployment(context.Background(), &domainfunctions.Deployment{
		ID: "dep_img", FunctionID: "fn_1", ProjectID: "p1",
		Status:          domainfunctions.DeploymentStatusPending,
		SourceType:      domainfunctions.DeploymentSourceImage,
		SourceURL:       "registry.example.com/acme/greet:v1",
		SourceRef:       sampleImageDigest,
		TemplateVersion: 0,
	}))

	exec := newMockExecutor(&domainfunctions.ExecutionResult{StatusCode: 0, Stdout: "ok"}, nil)
	uc := NewFunctions(&config.AppConfig{}, exec, repo, newMockQueue())

	rec := &domainfunctions.ExecutionRecord{
		ID: "e1", FunctionID: "fn_1", ProjectID: "p1", DeploymentID: "dep_img",
		Status: domainfunctions.ExecutionStatusQueued, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	require.NoError(t, repo.CreateExecution(context.Background(), rec))

	err := uc.ProcessExecutionPayload(context.Background(), []byte(
		`{"execution_id":"e1","function_id":"fn_1","project_id":"p1","data":"{}"}`))
	require.NoError(t, err)
	require.Zero(t, exec.builds, "image 源补构建不走 Build")
	require.Equal(t, 1, exec.imports, "image 源补构建 = 幂等 ImportImage")
	spec := exec.importSpecs[0]
	require.Equal(t, sampleImageDigest, spec.ExpectedDigest, "预期 digest = 行内 source_ref（本地命中零 pull 的幂等锚）")
	require.Equal(t, "registry.example.com/acme/greet:v1", spec.Reference)
	require.Empty(t, spec.RegistryUsername, "补构建凭证恒空（一次性凭证不落库，D8）")
	require.Empty(t, spec.RegistryToken)

	dep, _ := repo.GetDeployment(context.Background(), "p1", "fn_1", "dep_img")
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	got, _ := repo.GetExecution(context.Background(), "p1", "fn_1", "e1")
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, got.Status)
}

// TestListRuntimes_IncludesImageRuntime runtimes 表含 image 专用项（三期
// 阶段 1，设计 §3）：CreateFunction 由此放行 runtime=image。
func TestListRuntimes_IncludesImageRuntime(t *testing.T) {
	uc := NewFunctions(&config.AppConfig{}, newMockExecutor(nil, nil), newMockRepo(), newMockQueue())
	rts := uc.ListRuntimes()
	var image *domainfunctions.RuntimeInfo
	for i := range rts {
		if rts[i].ID == "image" {
			image = &rts[i]
		}
	}
	require.NotNil(t, image, "runtimes 表必须含 image 项")
	require.Equal(t, "Bring your own image", image.Name)
	require.Equal(t, "-", image.Entrypoint, "entrypoint 无意义占位")
}

// TestCreateDeployment_ImageEndToEndSmoke 进程内全链路冒烟（真实 DB；DSN 未
// 配置时跳过——带 DSN 验收跑法覆盖）：mock executor（ImportImage 成功）→
// bunrepo INSERT（image 源投影落真实 Postgres）→ buildDeployment 复检 →
// ready；DB 行 source 列与 template_version=0 读回断言。
func TestCreateDeployment_ImageEndToEndSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if testutil.AdminDSN() == "" || testutil.TestDSN() == "" {
		t.Skip("integration DSN not configured (TORCHWOOD_TEST_*_DATABASE_SOURCE)")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	now := time.Now()
	fn := &domainfunctions.Function{
		ID: "fn_img_e2e", ProjectID: projectID, Name: "e2e-img",
		Runtime: "image", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))

	exec := newMockExecutor(nil, nil)
	exec.importDigest = sampleImageDigest
	uc := NewFunctions(&config.AppConfig{}, exec, repo, newMockQueue())

	dep, err := uc.CreateDeployment(contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID: "svc-e2e", ActorKind: shared.ActorKindService, ProjectID: projectID,
	}), CreateDeploymentCommand{
		ProjectID:  projectID,
		FunctionID: fn.ID,
		Image:      imageSource(),
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	require.Equal(t, 2, exec.imports, "首次导入 + ready 门禁复检")

	// DB 行 source 列正确（真实 Postgres 读回）。
	stored, err := repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, domainfunctions.DeploymentSourceImage, stored.SourceType)
	require.Equal(t, "registry.example.com/acme/greet:v1", stored.SourceURL, "source_url = 原始引用")
	require.Equal(t, sampleImageDigest, stored.SourceRef, "source_ref = 钉死 digest")
	require.Empty(t, stored.SourceDir, "image 源 source_dir 恒空")
	require.Equal(t, int32(0), stored.TemplateVersion, "template_version=0 落库（并发降级 fail-safe）")
	require.Zero(t, stored.Size)
	require.Equal(t, domainfunctions.DeploymentStatusReady, stored.Status)
}
