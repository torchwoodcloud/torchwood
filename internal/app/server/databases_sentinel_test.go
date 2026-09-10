package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDatabases_ListFiltersSentinelAndGetRejects(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "App"))

	uc := NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)

	list, err := uc.ListDatabases(ctx, projectID)
	require.NoError(t, err)
	for _, item := range list {
		require.NotEqual(t, ident.ProjectDataPlaneID, item.ID, "ListDatabases 不得返回 sentinel")
		require.NotEqual(t, "(project)", item.Name)
	}

	_, err = uc.GetDatabase(ctx, projectID, ident.ProjectDataPlaneID)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	infra, err := docDB.GetDatabase(ctx, projectID, ident.ProjectDataPlaneID)
	require.NoError(t, err)
	require.Nil(t, infra, "cut 后 catalog 无 sentinel")
}

func TestDatabases_CreateDeleteSentinelRejected(t *testing.T) {
	ctx := platformAdminCtx(context.Background())
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)

	err := uc.CreateDatabase(ctx, "proj-1", ident.ProjectDataPlaneID, "x")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	err = uc.DeleteDatabase(ctx, "proj-1", ident.ProjectDataPlaneID)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
