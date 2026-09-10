package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/infra/projectschema"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestFirstDatabase_NoSystemCollections：系统资源不寄居缺省第一业务库（app）。
// cut 后 catalog 无 database_id='_'；ListCollections("app") 不含 7 个
// 系统集合；业务库可建普通 users。DeleteDatabase("app") 不得碰到静态 users。
func TestFirstDatabase_NoSystemCollections(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewProjectRepository(db)
	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	projectsUC := NewProjects(repo, docDB, db, projectschema.NewSchemaManager(db), nil, nil)
	uc := NewDatabases(repo, docDB, nil)

	p, err := projectsUC.CreateProject(ctx, CreateProjectCommand{
		ID:   "pr3app",
		Name: "PR3 First DB",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = projectsUC.DeleteProjectInternal(context.Background(), p.ID) })

	defaultSystem, err := db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Where("project_id = ? AND database_id = ? AND is_system = TRUE", p.ID, "app").
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, defaultSystem, "零行 database_id='app' AND is_system")

	sentinelSystem, err := db.NewSelect().Model((*model.DocumentCollection)(nil)).
		Where("project_id = ? AND database_id = ? AND is_system = TRUE", p.ID, databases.SystemDatabaseID).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, sentinelSystem, "cut 后 catalog 无 database_id='_' 系统集合")

	cols, _, _, err := uc.ListCollections(ctx, p.ID, "app", databases.ListQuery{})
	require.NoError(t, err)
	gotIDs := map[string]bool{}
	for _, c := range cols {
		require.False(t, c.IsSystem, "ListCollections(app) 不得返回 is_system 集合")
		gotIDs[c.ID] = true
	}
	for _, id := range databases.SystemCollectionIDs {
		require.False(t, gotIDs[id], "ListCollections(app) 不得含系统集合 %s", id)
	}

	coll, err := uc.GetCollection(ctx, p.ID, "app", "users")
	require.NoError(t, err)
	require.Nil(t, coll, "未自建时 app.users 不存在")

	require.NoError(t, uc.CreateCollection(ctx, p.ID, "app", "users", "Users", []databases.Attribute{
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, nil, nil, true))
	created, err := uc.GetCollection(ctx, p.ID, "app", "users")
	require.NoError(t, err)
	require.NotNil(t, created)
	require.False(t, created.IsSystem)

	sysUsers, err := docDB.GetCollection(ctx, p.ID, databases.SystemDatabaseID, "users")
	require.NoError(t, err)
	require.Nil(t, sysUsers, "cut 后 catalog 无 sentinel users")

	var staticUsers any
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regclass(?)`, "tw_"+p.ID+".users").Scan(&staticUsers))
	require.NotNil(t, staticUsers, "静态 users 必须在 tw_<project>")

	require.NoError(t, uc.DeleteDatabase(ctx, p.ID, "app"))
	gone, err := uc.GetDatabase(ctx, p.ID, "app")
	require.NoError(t, err)
	require.Nil(t, gone)

	var businessNS, projectNS any
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, "tw_"+p.ID+"_app").Scan(&businessNS))
	require.Nil(t, businessNS)
	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regnamespace(?)`, "tw_"+p.ID).Scan(&projectNS))
	require.NotNil(t, projectNS)

	require.NoError(t, db.DB.QueryRowContext(ctx, `SELECT to_regclass(?)`, "tw_"+p.ID+".users").Scan(&staticUsers))
	require.NotNil(t, staticUsers, "DeleteDatabase(app) 不得碰到静态 users")

	require.NoError(t, uc.CreateDatabase(ctx, p.ID, "app", "app"))
	require.NoError(t, uc.CreateCollection(ctx, p.ID, "app", "users", "Users", []databases.Attribute{
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, nil, nil, true))
	recreated, err := uc.GetCollection(ctx, p.ID, "app", "users")
	require.NoError(t, err)
	require.NotNil(t, recreated)
	require.False(t, recreated.IsSystem, "重建 app 后业务 users 不得是系统集合")
}
