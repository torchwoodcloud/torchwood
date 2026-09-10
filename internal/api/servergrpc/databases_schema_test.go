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
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestDatabasesService_ExportCollectionSchema（B10）：handler 面——`as` 形态
// 白名单（缺省 jsonschema、未知形态 InvalidArgument）、未认证拒绝、正常导出
// 与 NotFound 分流。
func TestDatabasesService_ExportCollectionSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	uc := appserver.NewDatabases(bunrepo.NewProjectRepository(db), docDB, nil)
	svc := NewDatabasesService(uc)

	principal := &shared.Principal{
		ActorKind: shared.ActorKindService,
		ProjectID: projectID,
		Roles:     []string{"keys"},
	}
	authed := contexts.WithPrincipal(ctx, principal)

	// 用例层 setup：写路径要求 server write actor（与 audit 测试同形态的
	// 平台 admin 主体）。
	admin := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
	})
	require.NoError(t, uc.CreateDatabase(admin, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(admin, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256, Required: true},
	}, nil, nil, true))

	// 未携带 principal → Unauthenticated。
	_, err := svc.ExportCollectionSchema(ctx, &serverv1.ExportCollectionSchemaRequest{
		DatabaseId: "app", CollectionId: "posts",
	})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// 缺省 as = jsonschema：正常导出。
	resp, err := svc.ExportCollectionSchema(authed, &serverv1.ExportCollectionSchemaRequest{
		DatabaseId: "app", CollectionId: "posts",
	})
	require.NoError(t, err)
	require.Equal(t, "https://json-schema.org/draft/2020-12/schema", resp.Schema.Fields["$schema"].GetStringValue())
	require.Contains(t, resp.Schema.Fields["properties"].GetStructValue().Fields, "title")

	// 显式 as=jsonschema 等价。
	_, err = svc.ExportCollectionSchema(authed, &serverv1.ExportCollectionSchemaRequest{
		DatabaseId: "app", CollectionId: "posts", As: ptrString("jsonschema"),
	})
	require.NoError(t, err)

	// 未知形态 → InvalidArgument（域码 + violations 定位）。
	_, err = svc.ExportCollectionSchema(authed, &serverv1.ExportCollectionSchemaRequest{
		DatabaseId: "app", CollectionId: "posts", As: ptrString("avro"),
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "DOCUMENT.INVALID_ARGUMENT")

	// 集合不存在 → NotFound。
	_, err = svc.ExportCollectionSchema(authed, &serverv1.ExportCollectionSchemaRequest{
		DatabaseId: "app", CollectionId: "missing",
	})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func ptrString(s string) *string { return &s }
