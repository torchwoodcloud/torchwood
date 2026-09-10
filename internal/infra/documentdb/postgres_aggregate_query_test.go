package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAggregateDocuments_QueryFieldWhitelist (R6)：聚合的过滤/排序字段走与
// List/Count 同源的 validateQueryFields——未声明列不落 PG 42703、search 需
// fulltext 索引、$version 过 readiness 检查（系统集合拒绝 / 用户集合未
// reconcile → version_column_unavailable）。
func TestAggregateDocuments_QueryFieldWhitelist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, []databases.Permission{{Type: "read", Role: "any"}}, true))

	bob := databases.Principal{Roles: []string{"users", "user:u1"}}
	aggs := []databases.AggregateSpec{{Function: databases.AggregateSum, Field: "views"}}

	// ① 过滤未声明列 → InvalidArgument（不落 PG 42703）。
	_, err := docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("nonexistent", "x")},
	}, aggs, "", bob)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "invalid query field")

	// ② search 对无 fulltext 索引的集合 → InvalidArgument。
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Search("title", "hello")},
	}, aggs, "", bob)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "fulltext")

	// ③a 系统集合 $version → invalid query field（系统表无 _version 列，
	// 与 List 路径同语义），不落 PG。files 有数值列 size（满足聚合数值白名单
	// 的前置校验），keys 角色可读。
	_, err = docDB.AggregateDocuments(ctx, projectID, databases.SystemDatabaseID, "files", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "1")},
	}, []databases.AggregateSpec{{Function: databases.AggregateSum, Field: "size"}}, "", databases.Principal{Roles: []string{"keys"}})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "invalid query field")

	// ③b 用户集合未 reconcile（DROP _version 列模拟）→ version_column_unavailable
	//（读路径以 status 消息承载哨兵文案，与 List 路径同语义），不落 PG 42703。
	schema := testSchema(t, projectID, "app")
	_, err = db.ExecContext(ctx, `ALTER TABLE `+tableName(schema, testPhysicalName(t, ctx, db, projectID, "app", "posts"))+` DROP COLUMN _version`)
	require.NoError(t, err)
	fresh := NewPostgresDocumentDB(db, nil)
	_, err = fresh.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "1")},
	}, aggs, "", databases.SystemPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), databases.ErrVersionColumnUnavailable.Error())

	// 合法字段过滤不受影响。
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts2", "Posts2", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, []databases.Permission{{Type: "read", Role: "any"}}, true))
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts2", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "x")},
	}, aggs, "", bob)
	require.NoError(t, err)
}

// TestCountAndAggregate_RejectNonFilterOperators（返工 R9 + R9b）：整集语义
// 对排序/分页算子显式拒绝。R9b：typed AST 的 page_size/page_token 随分页
// 字段归一进 AST 后一并拦截（此前 DSL 路径已拦、typed 路径漏拦）。
func TestCountAndAggregate_RejectNonFilterOperators(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, []databases.Permission{{Type: "read", Role: "any"}}, true))

	bob := databases.Principal{Roles: []string{"users", "user:u1"}}
	aggs := []databases.AggregateSpec{{Function: databases.AggregateSum, Field: "views"}}
	assertRejected := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Contains(t, status.Convert(err).Message(), "not supported on")
	}

	// count：orderAsc / page_size（typed AST，R9b）/ page_token（typed AST，
	// R9b）/ cursor / 请求级 page token。
	_, err := docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "x"), Orders: []query.Order{{Attribute: "views"}}},
	}, bob)
	assertRejected(t, err)
	_, err = docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{PageSize: 10},
	}, bob)
	assertRejected(t, err)
	_, err = docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{PageToken: "ka:doc-1"},
	}, bob)
	assertRejected(t, err)
	_, err = docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST:       &query.Query{CursorAfter: "doc-1"},
		PageToken: "ka:doc-1",
	}, bob)
	assertRejected(t, err)

	// aggregate：orderDesc / page_size（typed AST，R9b）/ page token。
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Orders: []query.Order{{Attribute: "views", Desc: true}}},
	}, aggs, "", bob)
	assertRejected(t, err)
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "x"), PageSize: 5},
	}, aggs, "", bob)
	assertRejected(t, err)
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{PageToken: "ka:doc-1"},
	}, aggs, "", bob)
	assertRejected(t, err)
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		PageToken: "ka:doc-1",
	}, aggs, "", bob)
	assertRejected(t, err)

	// 纯过滤不受影响（count 与 aggregate 均可走通）。
	_, err = docDB.CountDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "x")},
	}, bob)
	require.NoError(t, err)
	_, err = docDB.AggregateDocuments(ctx, projectID, "app", "posts", databases.Query{
		AST: &query.Query{Filter: query.Eq("title", "x")},
	}, aggs, "", bob)
	require.NoError(t, err)
}
