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

// TestFunctionRepository_V3ConcurrencyColumn concurrency 列读写往返（迁移
// 000017，docs/design/functions-v3.md §1.5 透传链起点）：显式值往返、零值
// 归一为平台默认 1、更新白名单登记（改得动）、CHECK 1..16 上界兜底。
func TestFunctionRepository_V3ConcurrencyColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewFunctionRepository(db)
	now := time.Now()

	// 显式 opt-in：concurrency=8 落库并回读。
	fn := &domainfunctions.Function{
		ID: "fn_v3", ProjectID: projectID, Name: "v3", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		Concurrency: 8, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateFunction(ctx, fn))
	got, err := repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, 8, got.Concurrency, "concurrency=8 往返")

	// 更新白名单（bun 更新写规范）：concurrency 已登记，16 改得动。
	fn.Concurrency = 16
	fn.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateFunction(ctx, fn))
	got, err = repo.GetFunction(ctx, projectID, fn.ID)
	require.NoError(t, err)
	require.Equal(t, 16, got.Concurrency, "更新白名单已登记 concurrency（改得动）")

	// 零值归一：Create/Update 不传（domain 零值 0）→ 平台默认 1（与列
	// DEFAULT 一致；否则 0 违反 CHECK 1..16）。
	def := &domainfunctions.Function{
		ID: "fn_v3_def", ProjectID: projectID, Name: "v3def", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, repo.CreateFunction(ctx, def))
	gotDef, err := repo.GetFunction(ctx, projectID, def.ID)
	require.NoError(t, err)
	require.Equal(t, 1, gotDef.Concurrency, "零值归一为平台默认 1")

	def.Concurrency = 0
	def.UpdatedAt = time.Now()
	require.NoError(t, repo.UpdateFunction(ctx, def))
	gotDef, err = repo.GetFunction(ctx, projectID, def.ID)
	require.NoError(t, err)
	require.Equal(t, 1, gotDef.Concurrency, "更新零值同样归一为 1")

	// CHECK 上界兜底：17 拒绝落库（超限报错而非静默截断；管理面校验随
	// B 切片 proto API 落地）。
	bad := &domainfunctions.Function{
		ID: "fn_v3_bad", ProjectID: projectID, Name: "v3bad", Runtime: "node-18.0",
		Entrypoint: "index.main", TimeoutSeconds: 15, Spec: "shared-1x", Enabled: true,
		Concurrency: 17, CreatedAt: now, UpdatedAt: now,
	}
	require.Error(t, repo.CreateFunction(ctx, bad), "CHECK concurrency BETWEEN 1 AND 16")
}
