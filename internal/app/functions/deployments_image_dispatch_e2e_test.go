package functions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	infrafunctions "github.com/torchwoodcloud/torchwood/internal/infra/functions"
	config "github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// 进程内端到端冒烟（三期阶段 3，镜像源 BYO；真实 DB；DSN 未配置时跳过——
// 集成链路在 `task test`/带 DSN 的验收跑法覆盖）：与 git 源冒烟
// （deployments_git_e2e_test.go）同风格，差异在 executor 侧——httptest 起
// fake functions-dispatcher（实现 /v1/dispatch/images/import 返回固定
// digest、/v1/dispatch/executions 返回固定 ok 封套），经**真实**
// DispatcherExecutor HTTP 适配贯通整条链路；packer 为 nil（image 源不需要）。
//
// 覆盖面：create-from-image 语义（cmd.Image）→ 首次 ImportImage（INSERT
// 之前）→ bunrepo INSERT（source 投影落真实 Postgres）→ buildDeployment
// image 分流复检（expected_digest = 钉死 digest）→ ready → CreateExecution
// 同步执行走 fake dispatcher 的 executions 端点。

// TestCreateDeployment_ImageDispatcherEndToEndSmoke 镜像源全链路冒烟：
// 形状校验 → 真实 DispatcherExecutor → fake dispatcher（import ×2）→
// INSERT（source_ref=digest、template_version=0、size=0）→ ready →
// 执行（executions 端点）→ completed。
func TestCreateDeployment_ImageDispatcherEndToEndSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if testutil.AdminDSN() == "" || testutil.TestDSN() == "" {
		t.Skip("integration DSN not configured (TORCHWOOD_TEST_*_DATABASE_SOURCE)")
	}

	const (
		imageRef      = "registry.example.com/acme/greet:v1"
		pinnedDigest  = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
		sharedToken   = "e2e-dispatcher-token"
		oneShotToken  = "one-shot-registry-token"
		executionData = `{"hello":"world"}`
	)

	// importBodies / execBodies 收集各端点请求载荷（JSON 解码后断言）。
	var importCalls, execCalls int
	var importBodies []map[string]any
	dispatcherSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method %q", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if got := r.Header.Get("X-Tw-Dispatcher-Token"); got != sharedToken {
			t.Errorf("shared token header = %q, want %q", got, sharedToken)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/dispatch/images/import":
			importCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			importBodies = append(importBodies, body)
			_ = json.NewEncoder(w).Encode(map[string]string{"digest": pinnedDigest})
		case "/v1/dispatch/executions":
			execCalls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "fn_img_dispatch_e2e", body["function_id"], "执行载荷定向到函数")
			// 固定 ok 封套（main 风格）：status=ok + result 响应体。
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":      "ok",
				"response":    `{"ok":true,"echo":{"hello":"world"}}`,
				"duration_ms": 1,
				"status_code": 0,
			})
		default:
			t.Errorf("unexpected dispatcher path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dispatcherSrv.Close()

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	now := time.Now()
	fn := &domainfunctions.Function{
		ID: "fn_img_dispatch_e2e", ProjectID: projectID, Name: "e2e-img-dispatch",
		Runtime: "image", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))

	cfg := &config.AppConfig{Functions: &config.Functions{
		Dispatcher: &config.Functions_Dispatcher{Url: dispatcherSrv.URL, SharedToken: sharedToken},
	}}
	uc := NewFunctionsWithSourcePacker(
		cfg,
		infrafunctions.NewDispatcherExecutor(cfg), // 真实 infra HTTP 适配（fake 端点）
		repo,
		newMockQueue(),
		nil, nil, Semaphores{}, nil, nil, nil,
		nil, // packer：image 源不需要
	)

	principal := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID: "svc-e2e", ActorKind: shared.ActorKindService, ProjectID: projectID,
	})

	dep, err := uc.CreateDeployment(principal, CreateDeploymentCommand{
		ProjectID:  projectID,
		FunctionID: fn.ID,
		Image: &domainfunctions.ImageSource{
			Reference:        imageRef,
			RegistryUsername: "bot",
			RegistryToken:    oneShotToken,
		},
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	require.Equal(t, 2, importCalls, "首次导入 + ready 门禁复检，恰好两次 import")

	// 首次导入（INSERT 之前）：一次性凭证随载荷、无预期 digest；复检：预期
	// digest = 钉死值、凭证为空（一次性凭证不落库，D8）。
	require.Len(t, importBodies, 2)
	first, recheck := importBodies[0], importBodies[1]
	require.Equal(t, imageRef, first["reference"])
	require.Equal(t, oneShotToken, first["registry_token"])
	require.Empty(t, first["expected_digest"], "首次导入无预期 digest")
	require.Equal(t, imageRef, recheck["reference"])
	require.Equal(t, pinnedDigest, recheck["expected_digest"], "复检带预期 digest（幂等锚）")
	require.Empty(t, recheck["registry_token"], "复检凭证恒空")

	// DB 行 source 投影正确（真实 Postgres 读回）。
	stored, err := repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, domainfunctions.DeploymentSourceImage, stored.SourceType)
	require.Equal(t, imageRef, stored.SourceURL, "source_url = 原始引用")
	require.Equal(t, pinnedDigest, stored.SourceRef, "source_ref = 钉死 digest")
	require.Empty(t, stored.SourceDir, "image 源 source_dir 恒空")
	require.Equal(t, int32(0), stored.TemplateVersion, "template_version=0 落库（并发降级 fail-safe）")
	require.Zero(t, stored.Size, "image 源无 zip 字节流")
	require.Equal(t, domainfunctions.DeploymentStatusReady, stored.Status)

	// image 源不写 zipPath（无盘上快照——补构建走幂等 ImportImage）。
	require.NoFileExists(t, zipPath(projectID, fn.ID, dep.ID))

	// 执行链路：同步 CreateExecution → executor.Execute → fake dispatcher
	// 的 executions 端点 → completed。
	rec, err := uc.CreateExecution(principal, CreateExecutionCommand{
		ProjectID:  projectID,
		FunctionID: fn.ID,
		Data:       executionData,
	})
	require.NoError(t, err)
	require.Equal(t, domainfunctions.ExecutionStatusCompleted, rec.Status)
	require.Equal(t, 1, execCalls, "执行走 fake dispatcher 的 executions 端点")
	require.Equal(t, `{"ok":true,"echo":{"hello":"world"}}`, rec.Response)
	require.Zero(t, importCalls-2, "执行不再触发 import")
}
