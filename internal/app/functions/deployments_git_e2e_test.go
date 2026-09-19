package functions

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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

// 端到端最小冒烟（二期阶段 3，真实 DB；DSN 未配置时跳过——集成链路在
// `mise run test`/带 DSN 的验收跑法覆盖）：httptest 起 fake packer
// → PackerClient（真实 infra 适配）→ app CreateDeployment(git)（fake
// executor）→ 断言 zipPath 文件存在 + DB 行 source 列正确。

// TestCreateDeployment_GitEndToEndSmoke 进程内全链路冒烟：形状校验 →
// PackerClient → fake packer → 写盘 → bunrepo INSERT（source 投影落真实
// Postgres）→ buildDeployment（fake executor Build）→ ready。
func TestCreateDeployment_GitEndToEndSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	if testutil.AdminDSN() == "" || testutil.TestDSN() == "" {
		t.Skip("integration DSN not configured (TORCHWOOD_TEST_*_DATABASE_SOURCE)")
	}

	const (
		commitSHA = "0123456789abcdef0123456789abcdef01234567"
		checksum  = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		zipBytes  = "PK\x03\x04e2e-materialized-zip"
	)
	// fake packer：同款路由形状（POST /v1/pack/git +
	// x-tw-packer-token 认证），返回固定 PackResponse。
	packCalls := 0
	packerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/pack/git", r.URL.Path)
		require.Equal(t, "e2e-shared-token", r.Header.Get("X-Tw-Packer-Token"))
		packCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"commit_sha": commitSHA,
			"checksum":   checksum,
			"zip_base64": base64.StdEncoding.EncodeToString([]byte(zipBytes)),
		})
	}))
	defer packerSrv.Close()

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewFunctionRepository(db)
	now := time.Now()
	fn := &domainfunctions.Function{
		ID: "fn_e2e", ProjectID: projectID, Name: "e2e",
		Runtime: "node-24.0", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))

	cfg := &config.AppConfig{Functions: &config.Functions{
		Packer: &config.Functions_Packer{Url: packerSrv.URL, SharedToken: "e2e-shared-token"},
	}}
	uc := NewFunctionsWithSourcePacker(
		cfg,
		newMockExecutor(nil, nil), // fake executor：Build 成功
		repo,
		newMockQueue(),
		nil, nil, Semaphores{}, nil, nil, nil,
		infrafunctions.NewPackerClient(cfg), // 真实 SourcePacker 适配
	)

	dep, err := uc.CreateDeployment(contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID: "svc-e2e", ActorKind: shared.ActorKindService, ProjectID: projectID,
	}), CreateDeploymentCommand{
		ProjectID:  projectID,
		FunctionID: fn.ID,
		Git: &domainfunctions.GitSource{
			URL:       "https://git.example.com/acme/widget.git",
			Ref:       "main",
			Directory: "functions/greet",
			Username:  "git",
			Token:     "one-shot-e2e-token",
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, packCalls, "恰好一次 pack 调用")
	require.Equal(t, domainfunctions.DeploymentStatusReady, dep.Status)
	require.Equal(t, commitSHA, dep.SourceRef)

	// zipPath 文件存在（重建语义的盘上快照）。
	path := zipPath(projectID, fn.ID, dep.ID)
	t.Cleanup(func() { _ = removeZip(projectID, fn.ID, dep.ID) })
	require.FileExists(t, path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, zipBytes, string(data), "盘上 zip = packer 物化产物（字节级）")

	// DB 行 source 列正确（真实 Postgres 读回）。
	stored, err := repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, domainfunctions.DeploymentSourceGit, stored.SourceType)
	require.Equal(t, "https://git.example.com/acme/widget.git", stored.SourceURL)
	require.Equal(t, commitSHA, stored.SourceRef, "source_ref = 钉死 commit SHA")
	require.Equal(t, "functions/greet", stored.SourceDir)
	require.Equal(t, checksum, stored.ContextSHA256)
	require.Equal(t, int64(len(zipBytes)), stored.Size)
	require.Equal(t, domainfunctions.DeploymentStatusReady, stored.Status)
}
