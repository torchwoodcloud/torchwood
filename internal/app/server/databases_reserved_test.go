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

// collectionDocDB 包装 fakeDocDB，使 GetCollection 返回可用集合，
// 供 CreateDocument 的单元测试使用。
type collectionDocDB struct {
	*fakeDocDB
}

func (collectionDocDB) GetCollection(context.Context, string, string, string) (*databases.Collection, error) {
	return &databases.Collection{ID: "coll1"}, nil
}

// TestCreateDocument_FormerReservedIDsAreRegularIDs：REST 自定义动词迁移
// （R10-P1-3/B3）后，旧字面量路由保留字（count/bulk）成为合法 document_id，
// 可正常创建并对 Get/Update/Delete 各验证一次成功路径。
func TestCreateDocument_FormerReservedIDsAreRegularIDs(t *testing.T) {
	d := &Databases{projectRepo: fakeProjectRepo{}, docDB: collectionDocDB{fakeDocDB: newFakeDocDB()}}
	ctx := platformAdminCtx(context.Background())
	principal := databases.Principal{PlatformAdmin: true}

	for _, id := range []string{"count", "bulk"} {
		created, _, err := d.CreateDocument(ctx, "p1", "db1", "coll1", id, map[string]any{"a": 1}, nil, principal, "")
		require.NoError(t, err, "document_id %q 应可正常创建", id)
		require.Equal(t, id, created.ID)

		got, err := d.GetDocument(ctx, "p1", "db1", "coll1", id, principal)
		require.NoError(t, err)
		require.Equal(t, 1, got.Data["a"])

		updated, _, err := d.UpdateDocument(ctx, "p1", "db1", "coll1", id, map[string]any{"a": 2}, nil, nil, nil, principal, &created.Version, "")
		require.NoError(t, err)
		require.Equal(t, 2, updated.Data["a"])

		_, delErr := d.DeleteDocument(ctx, "p1", "db1", "coll1", id, principal, &updated.Version, "")
		require.NoError(t, delErr)
		_, err = d.GetDocument(ctx, "p1", "db1", "coll1", id, principal)
		require.Equal(t, codes.NotFound, status.Code(err), "删除后 document_id %q 应不可再读", id)
	}

	// 普通 id 仍可正常创建（文档写入 fakeDocDB 成功）。
	doc, _, err := d.CreateDocument(ctx, "p1", "db1", "coll1", "doc_1", map[string]any{"a": 1}, nil, principal, "")
	require.NoError(t, err)
	require.Equal(t, "doc_1", doc.ID)
}

// TestDatabases_ReservedIDDocumentCRUD：id="count" 的文档在真实 documentdb
// adapter 上创建后 Get/Update/Delete 语义正常（REST 自定义动词迁移 B3）。
func TestDatabases_ReservedIDDocumentCRUD(t *testing.T) {
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
	principal := databases.Principal{Roles: []string{"keys"}}

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))

	created, _, err := uc.CreateDocument(ctx, projectID, "app", "posts", "count", map[string]any{"title": "reserved"}, nil, principal, "")
	require.NoError(t, err)
	require.Equal(t, "count", created.ID)

	got, err := uc.GetDocument(ctx, projectID, "app", "posts", "count", principal)
	require.NoError(t, err)
	require.Equal(t, "reserved", got.Data["title"])

	updated, _, err := uc.UpdateDocument(ctx, projectID, "app", "posts", "count", map[string]any{"title": "renamed"}, nil, nil, nil, principal, &created.Version, "")
	require.NoError(t, err)
	require.Equal(t, "renamed", updated.Data["title"])

	_, delErr := uc.DeleteDocument(ctx, projectID, "app", "posts", "count", principal, &updated.Version, "")
	require.NoError(t, delErr)
	_, err = uc.GetDocument(ctx, projectID, "app", "posts", "count", principal)
	require.Equal(t, codes.NotFound, status.Code(err))
}
