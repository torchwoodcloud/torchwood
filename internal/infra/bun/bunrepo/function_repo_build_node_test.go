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

// TestFunctionRepository_DeploymentBuildNodeRoundTrip 覆盖构建亲和列
// build_node（迁移 000024，四期 4a-1，
// docs/design/functions-runtimes-and-sources.md §4 M5）：
//  1. 往返：CreateDeployment（BuildNode）→ Get/List 读回一致；
//  2. UpdateDeployment（列白名单 status/error/build_node/updated_at）可更新
//     build_node（构建后写入的操作列，与 source 快照列的不可变语义相反）；
//  3. 存量形态：零值 BuildNode 落库读回空串（DEFAULT ”，无亲和）。
func TestFunctionRepository_DeploymentBuildNodeRoundTrip(t *testing.T) {
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

	// ① 往返：INSERT 携带 BuildNode → Get/List 读回一致。
	dep := &domainfunctions.Deployment{
		ID:         "dep_node",
		FunctionID: fn.ID,
		ProjectID:  projectID,
		Size:       1024,
		Status:     domainfunctions.DeploymentStatusPending,
		BuildNode:  "dispatcher-1",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, repo.CreateDeployment(ctx, dep))

	got, err := repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "dispatcher-1", got.BuildNode, "build_node INSERT 期写入")

	list, err := repo.ListDeployments(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "dispatcher-1", list[0].BuildNode, "List 投影与 Get 一致")

	// ② UpdateDeployment 更新 build_node（白名单登记——构建后写入的操作列；
	// 对比：source 快照五列有意不登记、保持不可变）。
	got.BuildNode = "dispatcher-2"
	got.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateDeployment(ctx, got))

	after, err := repo.GetDeployment(ctx, projectID, fn.ID, dep.ID)
	require.NoError(t, err)
	require.Equal(t, "dispatcher-2", after.BuildNode, "build_node 必须经 UpdateDeployment 白名单可更新")

	// ③ 存量形态：零值 BuildNode（迁移前调用方）落库读回空串。
	legacy := &domainfunctions.Deployment{
		ID:         "dep_nonode",
		FunctionID: fn.ID,
		ProjectID:  projectID,
		Status:     domainfunctions.DeploymentStatusReady,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	require.NoError(t, repo.CreateDeployment(ctx, legacy))
	gotLegacy, err := repo.GetDeployment(ctx, projectID, fn.ID, legacy.ID)
	require.NoError(t, err)
	require.Empty(t, gotLegacy.BuildNode, "零值落库 = 空串（无亲和，DEFAULT ''）")
}
