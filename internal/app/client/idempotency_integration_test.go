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
)

// Client 面写幂等（redesign §4.1/§10.1）：EndUser 主体同 key 重放返回原响应。
func TestClientDatabases_IdempotencyReplay(t *testing.T) {
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
	store := bunrepo.NewIdempotencyStore(db)
	account := NewTestAccount(testConfig(), projectRepo, db)
	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "client-idem@torchwood.local",
		Password:  "User@123456",
		Name:      "Client Idem",
	})
	require.NoError(t, err)

	serverUC := appserver.NewDatabases(projectRepo, docDB, nil)
	require.NoError(t, serverUC.CreateDatabase(adminCtx(ctx), projectID, "app", "Application DB"))
	require.NoError(t, serverUC.CreateCollection(adminCtx(ctx), projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{{Type: "create", Role: "users"}}, true))

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Roles:     []string{"users", "user:" + user.ID},
	})
	clientUC := NewDatabases(projectRepo, docDB, store)

	first, replayed, err := clientUC.CreateDocument(userCtx, "app", "notes", "note-1", map[string]any{"title": "a"}, nil, "req-c-1")
	require.NoError(t, err)
	require.False(t, replayed)

	second, replayed, err := clientUC.CreateDocument(userCtx, "app", "notes", "note-1", map[string]any{"title": "a"}, nil, "req-c-1")
	require.NoError(t, err)
	require.True(t, replayed)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, first.Version, second.Version)
}
