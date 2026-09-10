package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestAdminRepo_RevokeCredentials（M5 C1）：撤销时间戳持久化且只前推——
// 更晚的撤销覆盖、更早的不回退；未撤销行随行读出零值。
func TestAdminRepo_RevokeCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	repo := bunrepo.NewAdminRepository(db)
	_, err := db.ExecContext(ctx, `INSERT INTO admins (id, email, password_hash, role, created_at, updated_at) VALUES ('a-revoke-1', 'revoke@test.local', 'hash', 'owner', NOW(), NOW())`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, `DELETE FROM admins WHERE id = 'a-revoke-1'`)
	})

	got, err := repo.GetAdmin(ctx, "a-revoke-1")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.True(t, got.RevokedAt.IsZero(), "未撤销行的 revoked_at 应读出零值")

	// 首次撤销落库（截断到微秒：PG TIMESTAMPTZ 精度上限）。
	first := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	require.NoError(t, repo.RevokeCredentials(ctx, "a-revoke-1", first))
	got, err = repo.GetAdmin(ctx, "a-revoke-1")
	require.NoError(t, err)
	require.True(t, got.RevokedAt.Equal(first), "撤销时间戳应随行读出")

	// 更晚的撤销前推。
	second := time.Now().Truncate(time.Microsecond)
	require.NoError(t, repo.RevokeCredentials(ctx, "a-revoke-1", second))
	got, err = repo.GetAdmin(ctx, "a-revoke-1")
	require.NoError(t, err)
	require.True(t, got.RevokedAt.Equal(second))

	// 更早的时间戳不回退（幂等只前推）。
	require.NoError(t, repo.RevokeCredentials(ctx, "a-revoke-1", first))
	got, err = repo.GetAdmin(ctx, "a-revoke-1")
	require.NoError(t, err)
	require.True(t, got.RevokedAt.Equal(second))

	// 不存在的 admin 静默成功（删除路径行已同事务删除，失效由行不存在兜底）。
	require.NoError(t, repo.RevokeCredentials(ctx, "a-missing", second))
}
