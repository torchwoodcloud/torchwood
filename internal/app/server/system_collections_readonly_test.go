package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestSystemCollections_IsSystemFlag：cut 后 catalog 无系统集合；
// 用户集合与自定义库同名集合 is_system=false。
func TestSystemCollections_IsSystemFlag(t *testing.T) {
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

	for _, id := range []string{"users", "sessions", "identities", "groups", "memberships", "buckets", "files"} {
		coll, err := docDB.GetCollection(ctx, projectID, databases.SystemDatabaseID, id)
		require.NoError(t, err)
		require.Nil(t, coll, "cut 后 catalog 无系统集合 %s", id)

		_, err = uc.GetCollection(ctx, projectID, databases.SystemDatabaseID, id)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "Databases API 不得暴露 sentinel")
	}

	// 用户集合 is_system=false。
	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", nil, nil, nil, true))
	posts, err := uc.GetCollection(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.False(t, posts.IsSystem)

	// 自定义库中 id=users 的集合不受系统名单限制（黑名单限定项目数据面）。
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "users", "Custom Users", []databases.Attribute{
		{ID: "name", Key: "name", Type: "string", Size: 256},
		{ID: "password_hash", Key: "password_hash", Type: "string", Size: 512},
	}, nil, nil, true))
	users, err := uc.GetCollection(ctx, projectID, "app", "users")
	require.NoError(t, err)
	require.False(t, users.IsSystem)

	// 自定义库同名集合读取不脱敏（脱敏仅限 default 库系统集合）。
	_, err = docDB.CreateDocument(ctx, projectID, "app", "users", databases.Document{
		ID:   "cu-1",
		Data: map[string]any{"name": "custom", "password_hash": "hash"},
	}, []databases.Permission{{Type: "read", Role: "keys"}}, databases.SystemPrincipal)
	require.NoError(t, err)
	customDoc, err := uc.GetDocument(ctx, projectID, "app", "users", "cu-1", databases.Principal{Roles: []string{"keys"}})
	require.NoError(t, err)
	require.Equal(t, "hash", customDoc.Data["password_hash"])
}

// TestSystemCollections_SchemaOpsDenied 覆盖 schema 级 7 操作全拒。
func TestSystemCollections_SchemaOpsDenied(t *testing.T) {
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
	perms := []databases.Permission{{Type: "read", Role: "keys"}}

	assertInvalid := func(err error) {
		require.Error(t, err)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
	}

	assertInvalid(uc.CreateCollection(ctx, projectID, databases.SystemDatabaseID, "users", "Users", nil, nil, nil, true))
	assertInvalid(uc.UpdateCollection(ctx, projectID, databases.SystemDatabaseID, "users", databases.CollectionPatch{Name: "renamed"}, databases.Principal{Roles: []string{"keys"}}))
	assertInvalid(uc.DeleteCollection(ctx, projectID, databases.SystemDatabaseID, "users"))
	assertInvalid(uc.CreateAttribute(ctx, projectID, databases.SystemDatabaseID, "users", databases.Attribute{ID: "x", Key: "x", Type: "string"}))
	assertInvalid(uc.DeleteAttribute(ctx, projectID, databases.SystemDatabaseID, "users", "email"))
	assertInvalid(uc.CreateIndex(ctx, projectID, databases.SystemDatabaseID, "users", databases.Index{ID: "i", Type: "key", Attributes: []string{"email"}}))
	assertInvalid(uc.DeleteIndex(ctx, projectID, databases.SystemDatabaseID, "users", "users_email_unique"))
	assertInvalid(uc.UpdateCollection(ctx, projectID, databases.SystemDatabaseID, "groups", databases.CollectionPatch{Permissions: &perms}, databases.Principal{Roles: []string{"keys"}}))

	// 自定义库同名集合不受影响。
	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "users", "Custom Users", nil, nil, nil, true))
	require.NoError(t, uc.DeleteCollection(ctx, projectID, "app", "users"))
}

// TestSystemCollections_DocumentAPIRejectsSentinel：Databases API 摸不到项目数据面。
func TestSystemCollections_DocumentAPIRejectsSentinel(t *testing.T) {
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
	keysPrincipal := databases.Principal{Roles: []string{"keys"}}

	_, err := uc.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "groups", databases.Query{}, keysPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = uc.GetDocument(ctx, projectID, databases.SystemDatabaseID, "users", "user-1", keysPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, _, err = uc.CreateDocument(ctx, projectID, databases.SystemDatabaseID, "users", "", map[string]any{"email": "a@b.c"}, nil, keysPrincipal, "")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestSystemCollections_UpdateCollectionPermissionValidation 覆盖 UpdateCollection
// 授予未持有角色时 InvalidArgument；持 keys 角色（privileged）可正常授予。
func TestSystemCollections_UpdateCollectionPermissionValidation(t *testing.T) {
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
	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "App DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", nil, nil, nil, true))

	userPrincipal := databases.Principal{Roles: []string{"users", "user:u1"}}
	badPerms := []databases.Permission{{Type: "update", Role: "any"}}
	err := uc.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{Permissions: &badPerms}, userPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	keysPrincipal := databases.Principal{Roles: []string{"keys"}}
	goodPerms := []databases.Permission{{Type: "read", Role: "keys"}, {Type: "create", Role: "users"}}
	require.NoError(t, uc.UpdateCollection(ctx, projectID, "app", "posts", databases.CollectionPatch{Permissions: &goodPerms}, keysPrincipal))
}
