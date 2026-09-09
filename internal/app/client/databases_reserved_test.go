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

// TestClientDatabases_ReservedIDDocumentCRUD：Client API 的 documents:count 迁移为
// REST 自定义动词（R10-P1-3/B3）后，"count" 不再是保留字，可作 document_id 正常
// 创建并对 Get/Update/Delete 各验证一次成功路径；Delete 后 Get 返回 NotFound。
func TestClientDatabases_ReservedIDDocumentCRUD(t *testing.T) {
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
	account := NewTestAccount(testConfig(), projectRepo, db)
	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "reserved-doc@torchwood.local",
		Password:  "User@123456",
		Name:      "Reserved Doc Owner",
	})
	require.NoError(t, err)

	serverUC := appserver.NewDatabases(projectRepo, docDB, nil)
	require.NoError(t, serverUC.CreateDatabase(adminCtx(ctx), projectID, "app", "Application DB"))
	require.NoError(t, serverUC.CreateCollection(adminCtx(ctx), projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
	}, true))

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Roles:     []string{"users", "user:" + user.ID},
	})
	clientUC := NewDatabases(projectRepo, docDB, nil)

	created, _, err := clientUC.CreateDocument(userCtx, "app", "notes", "count", map[string]any{
		"title": "Reserved note",
	}, nil, "")
	require.NoError(t, err, "document_id=\"count\" 应可正常创建")
	require.Equal(t, "count", created.ID)

	got, err := clientUC.GetDocument(userCtx, projectID, "app", "notes", "count")
	require.NoError(t, err)
	require.Equal(t, "Reserved note", got.Data["title"])

	updated, _, err := clientUC.UpdateDocument(userCtx, "app", "notes", "count", map[string]any{
		"title": "Renamed note",
	}, nil, nil, nil, &created.Version, "")
	require.NoError(t, err)
	require.Equal(t, "Renamed note", updated.Data["title"])

	_, delErr := clientUC.DeleteDocument(userCtx, "app", "notes", "count", &updated.Version, "")
	require.NoError(t, delErr)
	_, err = clientUC.GetDocument(userCtx, projectID, "app", "notes", "count")
	require.Equal(t, codes.NotFound, status.Code(err), "删除后 document_id=\"count\" 应不可再读")
}
