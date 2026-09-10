package server

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDatabases_ExecuteTransactions_PerOpGrant（Phase 1 裁决③）：grant 豁免严格
// per-op——种子 op 仅豁免自身，不得外溢放行其他 op 的显式越权授予。
// 回归面：原实现把 allowed 提升为批级变量，批内「种子 create + 显式越权
// update（授予未持有角色）」会绕过 ValidateGrantablePermissions。
func TestDatabases_ExecuteTransactions_PerOpGrant(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	// 普通用户主体（非 keys、非 PlatformAdmin）：allowPrivilegedGrant=false，
	// ValidateGrantablePermissions 生效——精确复现越权面。
	user := databases.Principal{Roles: []string{"users", "user:u1"}}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 128},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "users"},
		{Type: "update", Role: "users"},
	}, true))

	// 批 1：种子 create（空 ACE）+ 显式越权 update（授予未持有的 user:other）
	// → 整批 InvalidArgument，且先于任何写入（app 层前置校验）。
	_, _, err := uc.ExecuteTransactions(ctx, projectID, "app", []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "one"}},
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "two"},
			Permissions: []databases.Permission{{Type: "update", Role: "user:other"}}},
	}, databases.TransactionModeAtomic, user, "")
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "ops[1]")
	require.Contains(t, st.Message(), "user:other")

	// 批 1 无副作用：种子 create 也未执行（前置校验先行）。
	_, err = uc.GetDocument(ctx, projectID, "app", "notes", "n1", databases.SystemPrincipal)
	require.Equal(t, codes.NotFound, status.Code(err))

	// 批 2：种子 create + 合法 update（空 permissions = 不变更 ACL）→ 成功。
	v1 := int64(1)
	results, _, err := uc.ExecuteTransactions(ctx, projectID, "app", []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "one"}},
		{Type: databases.TransactionOpUpdate, CollectionID: "notes", DocumentID: "n1", Data: map[string]any{"title": "two"}, ExpectedVersion: &v1},
	}, databases.TransactionModeAtomic, user, "")
	require.NoError(t, err)
	require.Len(t, results, 2)
	require.True(t, results[0].OK)
	require.True(t, results[1].OK)
	require.Equal(t, int64(2), results[1].Version)

	got, err := uc.GetDocument(ctx, projectID, "app", "notes", "n1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "two", got.Data["title"])
	// 种子 ACE：owner 三元组（update 的空 permissions 不变更 ACL）。
	require.Len(t, got.Permissions, 3)
	for _, p := range got.Permissions {
		require.True(t, strings.HasPrefix(p.Role, "user:u1") || p.Role == "user:u1", "seed perms must bind creator role, got %s:%s", p.Type, p.Role)
	}
}
