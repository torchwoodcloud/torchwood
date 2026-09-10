package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

func namespaceExists(t *testing.T, ctx context.Context, db *clients.Database, name string) bool {
	t.Helper()
	var reg any
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, name).Scan(&reg))
	return reg != nil
}

// TestDataPlaneSchema_DeleteDatabaseDefaultKeepsUsers：infra DeleteDatabase("default")
// 不得碰到 tw_<project>.users（§4.2 验收 1）。
func TestDataPlaneSchema_DeleteDatabaseDefaultKeepsUsers(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "default", "default"))

	usersRepo := bunrepo.NewUserRepository(db)
	require.NoError(t, usersRepo.Insert(ctx, projectID, &users.User{
		ID:     "u-keep",
		Email:  "keep@torchwood.local",
		Name:   "Keep",
		Status: users.StatusActive,
	}))

	projectSchema, err := ident.ProjectSchemaName(projectID)
	require.NoError(t, err)
	defaultSchema, err := ident.SchemaName(projectID, "default")
	require.NoError(t, err)

	require.True(t, namespaceExists(t, ctx, db, projectSchema))
	require.True(t, namespaceExists(t, ctx, db, defaultSchema))

	require.NoError(t, docDB.DeleteDatabase(ctx, projectID, "default"))

	got, err := usersRepo.GetByID(ctx, projectID, "u-keep")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, "keep@torchwood.local", got.Email)

	require.True(t, namespaceExists(t, ctx, db, projectSchema), "一段式 tw_<pid> 必须仍在")
	require.False(t, namespaceExists(t, ctx, db, defaultSchema), "tw_<pid>_default 应被 DROP")
}

// TestDataPlaneSchema_DeleteDatabaseSentinelRefused：infra DeleteDatabase("_")
// 失败且一段式 schema 仍在（§4.2 验收 2 / P0 DDL 分叉）。
func TestDataPlaneSchema_DeleteDatabaseSentinelRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)

	projectSchema, err := ident.ProjectSchemaName(projectID)
	require.NoError(t, err)
	require.True(t, namespaceExists(t, ctx, db, projectSchema))

	err = docDB.DeleteDatabase(ctx, projectID, ident.ProjectDataPlaneID)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	require.True(t, namespaceExists(t, ctx, db, projectSchema), "一段式 tw_<pid> 必须仍在")
	coll, err := docDB.GetCollection(ctx, projectID, ident.ProjectDataPlaneID, "users")
	require.NoError(t, err)
	require.Nil(t, coll, "cut 后 catalog 无 sentinel users")
	var rel any
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regclass(?)`, projectSchema+".users").Scan(&rel))
	require.NotNil(t, rel, "静态 users 必须仍在")
}

func TestDataPlaneSchema_CreateCollectionWhitelist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)

	err := docDB.CreateCollection(ctx, projectID, ident.ProjectDataPlaneID, "posts", "Posts", nil, nil, nil, true)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestDataPlaneSchema_SystemCRUDHitsOneSegment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectSchema, err := ident.ProjectSchemaName(projectID)
	require.NoError(t, err)
	var rel any
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regclass(?)`, projectSchema+".users").Scan(&rel))
	require.NotNil(t, rel)

	usersRepo := bunrepo.NewUserRepository(db)
	require.NoError(t, usersRepo.Insert(ctx, projectID, &users.User{
		ID:     "u-seg",
		Email:  "seg@torchwood.local",
		Name:   "Seg",
		Status: users.StatusActive,
	}))
	got, err := usersRepo.GetByID(ctx, projectID, "u-seg")
	require.NoError(t, err)
	require.Equal(t, "seg@torchwood.local", got.Email)
}
