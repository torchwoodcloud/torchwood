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

// TestFunctionRepository_DeploymentSourceRoundTrip 覆盖部署源快照五列
// （迁移 000023，Functions 部署源多元化二期阶段 1/4，
// docs/design/functions-runtimes-and-sources.md §0/§2）：
//  1. git 源行 INSERT → Get/List 读回五列一致（INSERT 期写全）；
//  2. UpdateDeployment（列白名单 status/error/updated_at）不得触碰 source 列
//     ——不可变语义（update_guard 约定：漏登记正是期望行为）；
//  3. 未感知 source 的调用方（SourceType 零值）落库归一 'zip'（迁移 000023
//     列 DEFAULT 同值；空串会违反 CHECK）。
func TestFunctionRepository_DeploymentSourceRoundTrip(t *testing.T) {
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
	now := time.Now()

	// ① git 源行：五列写入 → 读回一致。
	gitDep := &domainfunctions.Deployment{
		ID:            "dep_git",
		FunctionID:    fn.ID,
		ProjectID:     projectID,
		Size:          2048,
		Status:        domainfunctions.DeploymentStatusPending,
		SourceType:    domainfunctions.DeploymentSourceGit,
		SourceURL:     "https://git.example.com/acme/widget.git",
		SourceRef:     "0123456789abcdef0123456789abcdef01234567",
		SourceDir:     "functions/greet",
		ContextSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, repo.CreateDeployment(ctx, gitDep))

	got, err := repo.GetDeployment(ctx, projectID, fn.ID, gitDep.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, domainfunctions.DeploymentSourceGit, got.SourceType)
	require.Equal(t, gitDep.SourceURL, got.SourceURL)
	require.Equal(t, gitDep.SourceRef, got.SourceRef)
	require.Equal(t, gitDep.SourceDir, got.SourceDir)
	require.Equal(t, gitDep.ContextSHA256, got.ContextSHA256)

	list, err := repo.ListDeployments(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, gitDep.SourceRef, list[0].SourceRef, "List 投影与 Get 一致")

	// ② UpdateDeployment 改 status → source 五列保持不变（白名单不含）。
	got.Status = domainfunctions.DeploymentStatusFailed
	got.Error = "build failed"
	got.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateDeployment(ctx, got))

	after, err := repo.GetDeployment(ctx, projectID, fn.ID, gitDep.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentStatusFailed, after.Status)
	require.Equal(t, domainfunctions.DeploymentSourceGit, after.SourceType, "source_type 不可变")
	require.Equal(t, gitDep.SourceURL, after.SourceURL, "source_url 不可变")
	require.Equal(t, gitDep.SourceRef, after.SourceRef, "source_ref 不可变")
	require.Equal(t, gitDep.SourceDir, after.SourceDir, "source_dir 不可变")
	require.Equal(t, gitDep.ContextSHA256, after.ContextSHA256, "context_sha256 不可变")

	// ③ 零值 SourceType 落库归一 zip（存量/未感知调用方语义）。
	legacy := &domainfunctions.Deployment{
		ID:         "dep_legacy",
		FunctionID: fn.ID,
		ProjectID:  projectID,
		Status:     domainfunctions.DeploymentStatusReady,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, repo.CreateDeployment(ctx, legacy))
	gotLegacy, err := repo.GetDeployment(ctx, projectID, fn.ID, legacy.ID)
	require.NoError(t, err)
	require.Equal(t, domainfunctions.DeploymentSourceZip, gotLegacy.SourceType)
	require.Empty(t, gotLegacy.SourceURL, "zip 源投影列恒空串")
	require.Empty(t, gotLegacy.SourceRef)
	require.Empty(t, gotLegacy.SourceDir)
}
