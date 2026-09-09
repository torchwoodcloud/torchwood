package bunrepo_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

// M5 C7（审计留存不变量）：项目控制面清理不再删除 audit_logs——项目删除
// 本身已记审计，删除审计行等于销毁证据；留存/归档归运维面。
func TestProjectRepo_DeleteControlPlaneRows_KeepsAuditLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	_, err := db.ExecContext(ctx, `INSERT INTO audit_logs (id, project_id, actor_kind, action, status, metadata) VALUES ('al-keep-1', ?, 'admin', 'test.action', 'success', '{}')`, projectID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO api_keys (id, project_id, name, secret_hash, created_at, updated_at) VALUES ('ak-1', ?, 'k', 'hash', NOW(), NOW())`, projectID)
	require.NoError(t, err)

	repo := bunrepo.NewProjectRepository(db)
	require.NoError(t, repo.DeleteProjectControlPlaneRows(ctx, projectID))

	auditCount, err := db.NewSelect().Table("audit_logs").Where("project_id = ?", projectID).Count(ctx)
	require.NoError(t, err)
	keyCount, err := db.NewSelect().Table("api_keys").Where("project_id = ?", projectID).Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, auditCount, "审计行必须随项目删除保留")
	require.Equal(t, 0, keyCount, "api_keys 属控制面派生行，随项目删除清理")
}
