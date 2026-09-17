package console_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/console"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/password"
)

// TestAdmins_UpdateProfile_RealDB_GetAdminRoundtrip 复现线上事故
// （2026-09-17 部署环境：UpdateCurrentAdmin 之后认证热路径 GetAdmin 持续
// Internal "admin lookup failed"）。真库真事务走部署版本的完整 UpdateProfile
// 路径（改密 / 时区 / 组合），每步之后以 validator 同款 repo.GetAdmin 回读。
func TestAdmins_UpdateProfile_RealDB_GetAdminRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := adminActorCtx(context.Background())
	db := testutil.SetupTestDB(t)
	repo := bunrepo.NewAdminRepository(db)
	uc := console.NewAdmins(repo, nil, db)

	oldHash, err := password.Hash("OldPassw0rd")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO admins (id, email, password_hash, role, created_at, updated_at) VALUES ('a-real-1', 'real1@test.local', ?, 'owner', NOW(), NOW())`, oldHash)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM admins WHERE id = 'a-real-1'`)
	})

	// 基线：GetAdmin 可读。
	got, err := repo.GetAdmin(ctx, "a-real-1")
	require.NoError(t, err)
	require.NotNil(t, got)

	// 序列一（部署事故序列主嫌疑：改密 → RevokeCredentials）。
	_, err = uc.UpdateProfile(ctx, console.UpdateProfileCommand{CallerID: "a-real-1", CurrentPassword: "OldPassw0rd", NewPassword: "NewPassw0rd"})
	require.NoError(t, err)
	got, err = repo.GetAdmin(ctx, "a-real-1")
	require.NoError(t, err, "改密后 GetAdmin 不应报错（validator 认证热路径）")
	require.False(t, got.RevokedAt.IsZero())

	// 序列二：时区写入。
	_, err = uc.UpdateProfile(ctx, console.UpdateProfileCommand{CallerID: "a-real-1", Timezone: strPtr("Asia/Shanghai")})
	require.NoError(t, err)
	got, err = repo.GetAdmin(ctx, "a-real-1")
	require.NoError(t, err, "时区写入后 GetAdmin 不应报错")
	require.Equal(t, "Asia/Shanghai", got.Metadata["timezone"])

	// 序列三：改密 + 时区组合（同事务双写）。
	_, err = uc.UpdateProfile(ctx, console.UpdateProfileCommand{CallerID: "a-real-1", CurrentPassword: "NewPassw0rd", NewPassword: "NewPassw1rd", Timezone: strPtr("UTC")})
	require.NoError(t, err)
	got, err = repo.GetAdmin(ctx, "a-real-1")
	require.NoError(t, err, "改密+时区组合后 GetAdmin 不应报错")
	require.Equal(t, "UTC", got.Metadata["timezone"])

	var raw string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT metadata::text FROM admins WHERE id = 'a-real-1'`).Scan(&raw))
	t.Logf("final metadata raw: %s", raw)
}
