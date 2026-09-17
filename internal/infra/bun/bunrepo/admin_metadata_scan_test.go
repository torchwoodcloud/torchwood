package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestAdminRepo_MetadataScanTolerance 是 2026-09-17 线上事故的回归护栏：
// admins.metadata 落入任何合法 JSON 形态（含非字符串值）都不得让 GetAdmin
// 报错——GetAdmin 在认证热路径上（validator 每请求读 admins 行），偏好
// 形态只允许降级（非字符串键被跳过），不允许打挂 console 认证。
func TestAdminRepo_MetadataScanTolerance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	repo := bunrepo.NewAdminRepository(db)
	_, err := db.ExecContext(ctx, `INSERT INTO admins (id, email, password_hash, role, metadata, created_at, updated_at) VALUES ('a-meta-tol', 'tol@test.local', 'hash', 'owner', '{"timezone":"UTC"}', NOW(), NOW())`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM admins WHERE id = 'a-meta-tol'`)
	})

	// 非 object 形态：读为空偏好，绝不报错。
	for _, form := range []string{`'null'::jsonb`, `'[]'::jsonb`, `'"str"'::jsonb`, `'5'::jsonb`, `'true'::jsonb`} {
		_, err := db.ExecContext(ctx, `UPDATE admins SET metadata = `+form+` WHERE id = 'a-meta-tol'`)
		require.NoError(t, err, form)
		got, err := repo.GetAdmin(ctx, "a-meta-tol")
		require.NoError(t, err, "metadata=%s 必须容忍（认证热路径）", form)
		require.Empty(t, got.Metadata, "metadata=%s 非字符串内容应被跳过", form)
	}

	// object 含非字符串值：字符串键保留，其余跳过。
	_, err = db.ExecContext(ctx, `UPDATE admins SET metadata = '{"timezone":"Asia/Shanghai","level":3,"flag":true,"nested":{"a":1}}'::jsonb WHERE id = 'a-meta-tol'`)
	require.NoError(t, err)
	got, err := repo.GetAdmin(ctx, "a-meta-tol")
	require.NoError(t, err)
	require.Equal(t, "Asia/Shanghai", got.Metadata["timezone"])
	require.NotContains(t, got.Metadata, "level")
	require.NotContains(t, got.Metadata, "flag")
	require.NotContains(t, got.Metadata, "nested")

	// 事故序列复跑：坏形态下自助更新（写路径合并）仍可用并修复行。
	tz := "Europe/Berlin"
	require.NoError(t, bunrepo.NewAdminRepository(db).UpdateAdminMetadata(ctx, "a-meta-tol", map[string]string{"timezone": tz}, nil, time.Now()))
	require.NoError(t, err)
	got, err = repo.GetAdmin(ctx, "a-meta-tol")
	require.NoError(t, err)
	require.Equal(t, tz, got.Metadata["timezone"])
}
