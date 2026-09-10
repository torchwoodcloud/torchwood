package servergrpc

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

// TestDatabasesService_AuditResource (A8): databases gRPC handler 注入审计资源，
// CreateDocument 后 LatestAuditLog().ResourceID 非空且格式正确。
func TestDatabasesService_AuditResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	env, err := testutil.NewInterceptorEnv(db, &config.AppConfig{}, docDB)
	require.NoError(t, err)

	uc := appserver.NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	svc := NewDatabasesService(uc)

	adminCtx := auditAdminCtx(ctx)
	require.NoError(t, uc.CreateDatabase(adminCtx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(adminCtx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	const method = "/torchwood.server.v1.DatabasesService/CreateDocument"
	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) {
		return svc.CreateDocument(ctx, req.(*serverv1.CreateDocumentRequest))
	}

	principal := &shared.Principal{
		ActorKind: shared.ActorKindService,
		ProjectID: projectID,
		Roles:     []string{"keys"},
	}
	req := &serverv1.CreateDocumentRequest{
		DatabaseId:   "app",
		CollectionId: "posts",
		DocumentId:   "doc-1",
		Data:         mapStringToStruct(t, map[string]any{"title": "t"}),
	}
	_, err = env.Audit.UnaryAuditMiddleware(contexts.WithPrincipal(ctx, principal), req, info, handler)
	require.NoError(t, err)

	log, err := env.LatestAuditLog(ctx)
	require.NoError(t, err)
	require.Equal(t, method, log.Action)
	require.Equal(t, "databases/app/collections/posts/documents/doc-1", log.ResourceID)
}

// TestDatabasesService_UpsertDocumentAuditResource (T2): UpsertDocument 注入
// 文档级审计资源路径。
func TestDatabasesService_UpsertDocumentAuditResource(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)

	env, err := testutil.NewInterceptorEnv(db, &config.AppConfig{}, docDB)
	require.NoError(t, err)

	uc := appserver.NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	svc := NewDatabasesService(uc)

	adminCtx := auditAdminCtx(ctx)
	require.NoError(t, uc.CreateDatabase(adminCtx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(adminCtx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_title", Type: "unique", Attributes: []string{"title"}},
	}, nil, true))

	const method = "/torchwood.server.v1.DatabasesService/UpsertDocument"
	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) {
		return svc.UpsertDocument(ctx, req.(*serverv1.UpsertDocumentRequest))
	}

	principal := &shared.Principal{
		ActorKind: shared.ActorKindService,
		ProjectID: projectID,
		Roles:     []string{"keys"},
	}
	req := &serverv1.UpsertDocumentRequest{
		DatabaseId:      "app",
		CollectionId:    "posts",
		DocumentId:      "doc-1",
		Data:            mapStringToStruct(t, map[string]any{"title": "t"}),
		ConflictColumns: []string{"title"},
	}
	_, err = env.Audit.UnaryAuditMiddleware(contexts.WithPrincipal(ctx, principal), req, info, handler)
	require.NoError(t, err)

	log, err := env.LatestAuditLog(ctx)
	require.NoError(t, err)
	require.Equal(t, method, log.Action)
	require.Equal(t, "databases/app/collections/posts/documents/doc-1", log.ResourceID)
}

func mapStringToStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	require.NoError(t, err)
	return s
}

// auditAdminCtx 返回携带平台 admin principal 的上下文（DDL 写方法仅限平台 admin）。
func auditAdminCtx(ctx context.Context) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
	})
}
