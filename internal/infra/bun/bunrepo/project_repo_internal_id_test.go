package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/testutil"
)

// TestUpdateProject_PreservesInternalID（2026-09-08 dev 事故回归锚点）：
// 更新注册策略等任何项目字段都不得触碰 internal_id——它是数据面 _tenant
// 租户号与 roles_sig Tenant 的唯一来源，漂移即文档数据分裂 + 验签失配
// fail-closed（事故中改一次注册策略 internal_id 便 +1）。
func TestUpdateProject_PreservesInternalID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, internalID, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewProjectRepository(db)
	got, err := repo.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, internalID, got.InternalID)

	// 事故原样路径：改注册策略（app 层先读后写 + 置新 updated_at）。
	got.RegistrationPolicy = "invite_only"
	got.UpdatedAt = got.UpdatedAt.Add(time.Second)
	require.NoError(t, repo.UpdateProject(ctx, got))

	reloaded, err := repo.GetProject(ctx, projectID)
	require.NoError(t, err)
	require.Equal(t, "invite_only", reloaded.RegistrationPolicy, "registration_policy 应更新成功")
	require.Equal(t, internalID, reloaded.InternalID,
		"internal_id 不得因 UPDATE 漂移：identity 列对 UPDATE 只读")
}
