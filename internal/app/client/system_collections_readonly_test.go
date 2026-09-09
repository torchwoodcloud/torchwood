package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestClientDatabases_SystemCollectionAPIRejectsSentinel：Client Databases API
// 不得通过 sentinel 摸系统集合；自定义库同名集合仍可用。
func TestClientDatabases_SystemCollectionAPIRejectsSentinel(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	projectRepo := bunrepo.NewProjectRepository(db)
	serverUC := appserver.NewDatabases(projectRepo, docDB, nil)
	clientUC := NewDatabases(projectRepo, docDB, nil)

	_, err := clientUC.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "groups", databases.Query{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	account := NewTestAccount(testConfig(), projectRepo, db)
	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "syscoll@torchwood.local",
		Password:  "User@123456",
		Name:      "SysColl",
	})
	require.NoError(t, err)

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Roles:     []string{"users", "user:" + user.ID},
	})
	_, err = clientUC.GetDocument(userCtx, projectID, databases.SystemDatabaseID, "users", user.ID)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	for _, coll := range []string{"users", "groups"} {
		_, _, err := clientUC.CreateDocument(userCtx, databases.SystemDatabaseID, coll, "", map[string]any{"name": "x"}, nil, "")
		require.Equal(t, codes.InvalidArgument, status.Code(err), "create into sentinel %s", coll)
	}

	require.NoError(t, serverUC.CreateDatabase(adminCtx(ctx), projectID, "app", "App DB"))
	require.NoError(t, serverUC.CreateCollection(adminCtx(ctx), projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "users"},
		{Type: "update", Role: "users"},
		{Type: "delete", Role: "users"},
	}, true))
	created, _, err := clientUC.CreateDocument(userCtx, "app", "notes", "", map[string]any{"title": "Note"}, nil, "")
	require.NoError(t, err)
	got, err := clientUC.GetDocument(userCtx, projectID, "app", "notes", created.ID)
	require.NoError(t, err)
	require.Equal(t, "Note", got.Data["title"])

	require.NoError(t, serverUC.CreateCollection(adminCtx(ctx), projectID, "app", "users", "Custom Users", []databases.Attribute{
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "users"},
		{Type: "update", Role: "users"},
		{Type: "delete", Role: "users"},
	}, true))
	custom, _, err := clientUC.CreateDocument(userCtx, "app", "users", "", map[string]any{"name": "custom write"}, nil, "")
	require.NoError(t, err)
	got, err = clientUC.GetDocument(userCtx, projectID, "app", "users", custom.ID)
	require.NoError(t, err)
	require.Equal(t, "custom write", got.Data["name"])
}
