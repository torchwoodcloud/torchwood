package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestAdminRepo_UpdateAdminMetadataGetAdminRoundtrip 复现线上事故序列
// （2026-09-17 部署环境 admin lookup failed）：UpdateCurrentAdmin 写入
// metadata 后 GetAdmin 在认证热路径持续 Scan 报错。逐分支验证
// 设置/清除时区前后 GetAdmin 往返与落库形态。
func TestAdminRepo_UpdateAdminMetadataGetAdminRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	repo := bunrepo.NewAdminRepository(db)
	_, err := db.ExecContext(ctx, `INSERT INTO admins (id, email, password_hash, role, created_at, updated_at) VALUES ('a-meta-1', 'meta@test.local', 'hash', 'owner', NOW(), NOW())`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM admins WHERE id = 'a-meta-1'`)
	})

	got, err := repo.GetAdmin(ctx, "a-meta-1")
	require.NoError(t, err)
	require.NotNil(t, got)

	// 设置时区分支（tz 非空：set={"timezone":...}, remove=[]）。
	require.NoError(t, repo.UpdateAdminMetadata(ctx, "a-meta-1",
		map[string]string{"timezone": "Asia/Shanghai"}, nil, time.Now()))
	var raw string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT metadata::text FROM admins WHERE id = 'a-meta-1'`).Scan(&raw))
	t.Logf("metadata after set: %s", raw)
	got, err = repo.GetAdmin(ctx, "a-meta-1")
	require.NoError(t, err, "写入后 GetAdmin 不应报错（线上事故序列）")
	require.Equal(t, "Asia/Shanghai", got.Metadata["timezone"])

	// 清除时区分支（tz 空串：set={}, remove=["timezone"]）。
	require.NoError(t, repo.UpdateAdminMetadata(ctx, "a-meta-1",
		map[string]string{}, []string{"timezone"}, time.Now()))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT metadata::text FROM admins WHERE id = 'a-meta-1'`).Scan(&raw))
	t.Logf("metadata after remove: %s", raw)
	got, err = repo.GetAdmin(ctx, "a-meta-1")
	require.NoError(t, err)
	require.NotContains(t, got.Metadata, "timezone")
}
