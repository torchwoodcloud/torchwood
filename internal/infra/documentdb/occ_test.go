package documentdb

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
)

// occTestProject 创建带 "app"."docs" 用户集合的测试环境。
func occTestProject(t *testing.T, ctx context.Context) (databases.DocumentDB, string, int64, func()) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
		{Type: "update", Role: "users"},
		{Type: "delete", Role: "users"},
	}, true))
	cleanupFn := func() {
		_ = db.Close()
		cleanup()
	}
	return docDB, projectID, internalID, cleanupFn
}

func occPrincipal() databases.Principal {
	return databases.Principal{Roles: []string{"users", "user:u1"}}
}

func occCreate(t *testing.T, docDB databases.DocumentDB, projectID, id string) databases.Document {
	t.Helper()
	created, err := docDB.CreateDocument(context.Background(), projectID, "app", "docs", databases.Document{
		ID:   id,
		Data: map[string]any{"title": "t1", "views": 1},
	}, []databases.Permission{
		{Type: "read", Role: "user:u1"},
		{Type: "update", Role: "user:u1"},
		{Type: "delete", Role: "user:u1"},
	}, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(1), created.Version, "Create 后 version 应为 1")
	return created
}

func versionColumnCount(t *testing.T, ctx context.Context, db *clients.Database, schema, table string) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = '_version'`,
		schema, table).Scan(&count))
	return count
}

// TestUpdateDocument_VersionRequired：用户集合 Update（含仅 increment / 仅 permissions）
// 未携带 ExpectedVersion → ErrVersionRequired，行不变。
func TestUpdateDocument_VersionRequired(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, _, cleanup := occTestProject(t, ctx)
	defer cleanup()

	created := occCreate(t, docDB, projectID, "d1")

	cases := []struct {
		name   string
		update databases.DocumentUpdate
	}{
		{"无 data", databases.DocumentUpdate{Document: databases.Document{ID: created.ID, Data: map[string]any{"title": "x"}}}},
		{"仅 increment", databases.DocumentUpdate{Document: databases.Document{ID: created.ID}, Increment: map[string]int64{"views": 5}}},
		{"仅 permissions", databases.DocumentUpdate{Document: databases.Document{ID: created.ID}, Permissions: []databases.Permission{{Type: "read", Role: "user:u1"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", tc.update, occPrincipal())
			require.ErrorIs(t, err, databases.ErrVersionRequired)
		})
	}

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "t1", got.Data["title"])
	require.Equal(t, int64(1), got.Version, "失败更新不得改行/版本")
}

// TestUpdateDocument_VersionMismatch：ExpectedVersion 与当前行不符 →
// ErrVersionMismatch，行不变（不覆盖并发写）。
func TestUpdateDocument_VersionMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, _, cleanup := occTestProject(t, ctx)
	defer cleanup()

	created := occCreate(t, docDB, projectID, "d1")

	_, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"title": "stale write"}},
		ExpectedVersion: 99,
	}, occPrincipal())
	require.ErrorIs(t, err, databases.ErrVersionMismatch)
	// B10 §10.1：冲突错误携带探测 SELECT 读到的当前 _version（=1，零额外查询）。
	var vc *databases.VersionConflictError
	require.ErrorAs(t, err, &vc)
	require.Equal(t, int64(1), vc.CurrentVersion)

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "t1", got.Data["title"], "version 不匹配时行必须保持不变")
	require.Equal(t, int64(1), got.Version)
}

// TestUpdateDocument_IncrementRequiresVersion：Increment 与普通 Update 同一条
// 强制路径——不传 version → ErrVersionRequired；传对 → +1 成功。
func TestUpdateDocument_IncrementRequiresVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, _, cleanup := occTestProject(t, ctx)
	defer cleanup()

	created := occCreate(t, docDB, projectID, "d1")

	_, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:  databases.Document{ID: created.ID},
		Increment: map[string]int64{"views": 5},
	}, occPrincipal())
	require.ErrorIs(t, err, databases.ErrVersionRequired, "Increment 未带 version 必须拒绝")

	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID},
		Increment:       map[string]int64{"views": 5},
		ExpectedVersion: 1,
	}, occPrincipal())
	require.NoError(t, err)
	require.EqualValues(t, 6, updated.Data["views"])
	require.Equal(t, int64(2), updated.Version, "Increment 成功写必须 +1")
}

// TestDeleteDocument_VersionMismatch：Delete 带错 version → ErrVersionMismatch，
// 行保留；不传 version → ErrVersionRequired。
func TestDeleteDocument_VersionMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, _, cleanup := occTestProject(t, ctx)
	defer cleanup()

	created := occCreate(t, docDB, projectID, "d1")

	err := docDB.DeleteDocument(ctx, projectID, "app", "docs", created.ID, databases.DeleteOptions{}, occPrincipal())
	require.ErrorIs(t, err, databases.ErrVersionRequired, "Delete 未带 version 必须拒绝")

	err = docDB.DeleteDocument(ctx, projectID, "app", "docs", created.ID, databases.DeleteOptions{ExpectedVersion: 99}, occPrincipal())
	require.ErrorIs(t, err, databases.ErrVersionMismatch)
	// B10 §10.1：delete 预读路径同样携带当前 _version（=1）。
	var vc *databases.VersionConflictError
	require.ErrorAs(t, err, &vc)
	require.Equal(t, int64(1), vc.CurrentVersion)

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got, "version 不匹配时行必须保留")

	require.NoError(t, docDB.DeleteDocument(ctx, projectID, "app", "docs", created.ID, databases.DeleteOptions{ExpectedVersion: got.Version}, occPrincipal()))
	gone, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Nil(t, gone)
}

// TestBulkUpdate_SkipsVersion：Bulk 是唯一允许跳过 OCC 的 Update/Delete 调用方——
// 不传 version 仍成功，且每行 _version +1。
func TestBulkUpdate_SkipsVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID, _, cleanup := occTestProject(t, ctx)
	defer cleanup()

	d1 := occCreate(t, docDB, projectID, "d1")
	d2 := occCreate(t, docDB, projectID, "d2")

	affected, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs", []string{d1.ID, d2.ID},
		map[string]any{"title": "bulk"}, nil, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)

	got1, err := docDB.GetDocument(ctx, projectID, "app", "docs", d1.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "bulk", got1.Data["title"])
	require.Equal(t, int64(2), got1.Version, "Bulk 更新必须 _version +1（不要求客户端 version）")

	affected, err = docDB.BulkDeleteDocuments(ctx, projectID, "app", "docs", []string{d1.ID, d2.ID}, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)
}

// TestUpsert_NoVersionCheck：Upsert 插入支 DEFAULT 1；更新支盲写（无 OCC）但 +1。
func TestUpsert_NoVersionCheck(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "email", Key: "email", Type: "string", Size: 256},
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_email", Type: "unique", Attributes: []string{"email"}},
	}, nil, true))

	// 插入支：version=1。
	created, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "u1",
		Data: map[string]any{"email": "a@b.c", "name": "first"},
	}, []string{"email"}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(1), created.Version, "Upsert 插入支 version 应为 1")

	// 更新支：不校验 version（盲写），version +1。
	updated, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "u1",
		Data: map[string]any{"email": "a@b.c", "name": "second"},
	}, []string{"email"}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "second", updated.Data["name"])
	require.Equal(t, int64(2), updated.Version, "Upsert 更新支必须 _version +1")

	got, err := docDB.GetDocument(ctx, projectID, "app", "members", "u1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Version)
}

// TestSystemCollection_NoVersionColumn：系统集合表没有 _version 列，
// 写路径不 ALTER；系统集合 Update/Delete 不要求 version 也不 SET _version。
func TestSystemCollection_NoVersionColumn(t *testing.T) {
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

	schema := testProjectSchema(t, projectID)
	require.Equal(t, 0, versionColumnCount(t, ctx, db, schema, "users"), "系统集合 users 表不得有 _version 列")

	// 系统集合写路径不要求 version（内部 Users 等高频更新）且不 SET _version。
	userDoc, err := docDB.CreateDocument(ctx, projectID, databases.SystemDatabaseID, "users", databases.Document{
		ID:   "u1",
		Data: map[string]any{"email": "a@b.c", "status": "active"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	updated, err := docDB.UpdateDocument(ctx, projectID, databases.SystemDatabaseID, "users", databases.SimpleDocumentUpdate(databases.Document{
		ID:   userDoc.ID,
		Data: map[string]any{"name": "n"},
	}, nil), databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "n", updated.Data["name"])
	// sentinel `_` 上的 SystemCollectionIDs 无 `_version` 列，读路径视为 1。
	require.Equal(t, int64(1), updated.Version)

	require.NoError(t, docDB.DeleteDocument(ctx, projectID, databases.SystemDatabaseID, "users", userDoc.ID, databases.DeleteOptions{}, databases.SystemPrincipal))
}

// TestVersionColumn_CreateCollectionReconcilesLegacyTable：物理解耦后"存量表"
// = catalog 登记的物理表缺 _version（带外 DROP 模拟）——读路径缺列视为 1、
// 不 ALTER；DDL touch（CreateAttribute）一次补列后 OCC 可用。
func TestVersionColumn_CreateCollectionReconcilesLegacyTable(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
	}, true))
	schema := testSchema(t, projectID, "app")
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")

	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (_id, _tenant) VALUES ('legacy', ?)`, tableName(schema, physical)), internalID)
	require.NoError(t, err)
	// 带外摘列模拟存量表（无 _version）。
	_, err = db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN _version`, tableName(schema, physical)))
	require.NoError(t, err)

	fresh := NewPostgresDocumentDB(db, nil)
	got, err := fresh.GetDocument(ctx, projectID, "app", "docs", "legacy", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Version, "存量表读路径缺列视为 1")
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "读路径不得 ALTER")

	// DDL touch（CreateAttribute）经 reconcile 一次补列。
	require.NoError(t, fresh.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
		ID: "extra", Key: "extra", Type: "string", Size: 64,
	}))
	require.Equal(t, 1, versionColumnCount(t, ctx, db, schema, physical), "DDL touch 必须给存量表补 _version")

	updated, err := fresh.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document: databases.Document{
			ID:   "legacy",
			Data: map[string]any{"title": "t"},
		},
		ExpectedVersion: 1,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Version, "reconcile 后更新 version 1→2")

	listPrincipal := databases.Principal{Roles: []string{"users", "user:u1"}}
	list, err := fresh.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "2")},
	}, listPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "legacy", list.Documents[0].ID)
}

// TestVersionColumn_TypeConflictFailClosed：物理表 _version 被非 bigint 列抢占
// （用户属性抢占，带外 ALTER 模拟）→ 写路径拒绝 OCC（version_column_conflict），
// 禁止类型错误的 +1。
func TestVersionColumn_TypeConflictFailClosed(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", nil, nil, nil, true))
	schema := testSchema(t, projectID, "app")
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")

	// 带外把 _version 抢占为 TEXT。
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s DROP COLUMN _version`, tableName(schema, physical)))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN _version TEXT NOT NULL DEFAULT 'v0'`, tableName(schema, physical)))
	require.NoError(t, err)

	fresh := NewPostgresDocumentDB(db, nil)
	_, err = fresh.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"title": "x"},
	}, nil, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnConflict, "写路径遇非 bigint _version 必须 fail-closed")

	err = fresh.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{ID: "n", Key: "n", Type: "integer"})
	require.ErrorIs(t, err, databases.ErrVersionColumnConflict, "DDL touch 遇非 bigint _version 必须 fail-closed")

	var udtName string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT udt_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = '_version'`,
		schema, physical).Scan(&udtName))
	require.Equal(t, "text", udtName)
}

// TestVersionColumn_WritePathDoesNotAlter：文档写路径不得 ALTER TABLE ADD COLUMN。
// 缺列时 Create/Update/Delete/Upsert/Bulk fail-closed；CreateAttribute 一次补列后 OCC 仍过。
func TestVersionColumn_WritePathDoesNotAlter(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, internalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_title", Type: "unique", Attributes: []string{"title"}},
	}, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
		{Type: "update", Role: "users"},
		{Type: "delete", Role: "users"},
	}, true))

	schema := testSchema(t, projectID, "app")
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (_id, _tenant, "title") VALUES ('d1', ?, 't1')`, tableName(schema, physical)), internalID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN _version`, tableName(schema, physical)))
	require.NoError(t, err)

	// 新实例：避免旧进程 cache 把已 DROP 的列当成就绪。
	fresh := NewPostgresDocumentDB(db, nil)
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "DROP 后列必须不存在")

	got, err := fresh.GetDocument(ctx, projectID, "app", "docs", "d1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Version, "读路径缺列视为 1")
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "读路径不得 ALTER")

	listPrincipal := databases.Principal{Roles: []string{"users", "user:u1"}}
	_, err = fresh.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "1")},
	}, listPrincipal)
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), databases.ErrVersionColumnUnavailable.Error())

	_, err = fresh.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document: databases.Document{
			ID:   "d1",
			Data: map[string]any{"title": "t2"},
		},
		ExpectedVersion: 1,
	}, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable, "写路径缺列必须 fail-closed，不得 ALTER")
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "写路径不得 ALTER")

	_, err = fresh.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "d2",
		Data: map[string]any{"title": "n"},
	}, nil, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable)
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "CreateDocument 不得 ALTER")

	err = fresh.DeleteDocument(ctx, projectID, "app", "docs", "d1", databases.DeleteOptions{ExpectedVersion: 1}, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable)
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "DeleteDocument 不得 ALTER")

	_, err = fresh.UpsertDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "d3",
		Data: map[string]any{"title": "up"},
	}, []string{"title"}, nil, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable)
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "UpsertDocument 不得 ALTER")

	_, err = fresh.BulkUpdateDocuments(ctx, projectID, "app", "docs", []string{"d1"},
		map[string]any{"title": "bulk"}, nil, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable)
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "BulkUpdate 不得 ALTER")

	_, err = fresh.BulkDeleteDocuments(ctx, projectID, "app", "docs", []string{"d1"}, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable)
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical), "BulkDelete 不得 ALTER")

	require.NoError(t, fresh.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
		ID: "views", Key: "views", Type: "integer",
	}))
	require.Equal(t, 1, versionColumnCount(t, ctx, db, schema, physical), "CreateAttribute 必须给存量表补 _version")

	updated, err := fresh.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document: databases.Document{
			ID:   "d1",
			Data: map[string]any{"title": "t2"},
		},
		ExpectedVersion: 1,
	}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Version)

	list, err := fresh.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "2")},
	}, listPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "d1", list.Documents[0].ID)
}

// TestVersionColumn_CreateTableInTxDoesNotPoisonCache：同一外层事务里建表并写
// 文档后回滚，不得把未提交的 _version 列写入 versionColumns。
func TestVersionColumn_CreateTableInTxDoesNotPoisonCache(t *testing.T) {
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

	err := db.RunInTx(ctx, func(txCtx context.Context) error {
		if err := docDB.CreateCollection(txCtx, projectID, "app", "docs", "Docs", []databases.Attribute{
			{ID: "title", Key: "title", Type: "string", Size: 256},
		}, nil, nil, true); err != nil {
			return err
		}
		if _, err := docDB.CreateDocument(txCtx, projectID, "app", "docs", databases.Document{
			ID:   "d1",
			Data: map[string]any{"title": "t1"},
		}, nil, databases.SystemPrincipal); err != nil {
			return err
		}
		return fmt.Errorf("rollback")
	})
	require.Error(t, err)

	schema := testSchema(t, projectID, "app")
	// 回滚后 catalog 行与物理表同时撤销（元数据与 DDL 同事务）。
	var tblCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = ?`, schema).Scan(&tblCount))
	require.Zero(t, tblCount, "回滚后不得残留物理表")
	rolledBack, err := docDB.GetCollection(ctx, projectID, "app", "docs")
	require.NoError(t, err)
	require.Nil(t, rolledBack, "回滚后不得残留 catalog 行")

	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, nil, true))
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	_, err = db.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s DROP COLUMN _version`, tableName(schema, physical)))
	require.NoError(t, err)

	_, err = docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: "d1", Data: map[string]any{"title": "t2"}},
		ExpectedVersion: 1,
	}, databases.SystemPrincipal)
	require.ErrorIs(t, err, databases.ErrVersionColumnUnavailable, "回滚后 cache 不得把已撤销列当就绪")
	require.Zero(t, versionColumnCount(t, ctx, db, schema, physical))
}

// TestQueryVersion_TypeConflictFailClosed (F4)：读路径遇非 bigint _version 列
// 与写路径同码 version_column_conflict（Fail-closed），仅"列不存在"才是
// version_column_unavailable。
func TestQueryVersion_TypeConflictFailClosed(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", nil, nil, []databases.Permission{
		{Type: "read", Role: "any"},
	}, true))

	schema := testSchema(t, projectID, "app")
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	_, err := db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN _version DROP DEFAULT`, tableName(schema, physical)))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s ALTER COLUMN _version TYPE TEXT USING _version::text`, tableName(schema, physical)))
	require.NoError(t, err)

	fresh := NewPostgresDocumentDB(db, nil)
	_, err = fresh.ListDocuments(ctx, projectID, "app", "docs", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "1")},
	}, databases.Principal{Roles: []string{"users", "user:u1"}})
	require.ErrorIs(t, err, databases.ErrVersionColumnConflict)
	require.Contains(t, err.Error(), databases.ErrVersionColumnConflict.Error())
	require.NotContains(t, err.Error(), databases.ErrVersionColumnUnavailable.Error())
}

// TestCreateAttribute_AdapterRejectsReservedColumns (F7)：直调 adapter 的
// CreateAttribute 也拒绝系统保留列（含 _version）——app 层校验之外的第二道
// fail-closed，防止 ADD COLUMN 破坏 OCC 列。
func TestCreateAttribute_AdapterRejectsReservedColumns(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", nil, nil, nil, true))

	err := docDB.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{Key: "_version", Type: "integer"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), `attribute key "_version" is reserved`)

	// 列未被改动：_version 仍为 bigint（未退化成用户属性）。
	schema := testSchema(t, projectID, "app")
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	var udtName string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT udt_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = '_version'`,
		schema, physical).Scan(&udtName))
	require.Equal(t, "int8", udtName)
}

// TestCreateAttribute_AdapterArrayElementWhitelist（阶段③-b，D-5 修订）：
// array=true 已是合法属性（PG 原生 T[] 列）——adapter 接受标量子集元素并
// 落 catalog/物理列；email/url/json 元素类型仍拒（第二道防线，app 层同口径）。
func TestCreateAttribute_AdapterArrayElementWhitelist(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", nil, nil, nil, true))

	err := docDB.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
		ID: "tags", Key: "tags", Type: "string", Array: true,
	})
	require.NoError(t, err, "array=true 属性现已合法（阶段③-b 预决策 1）")

	coll, err := docDB.GetCollection(ctx, projectID, "app", "docs")
	require.NoError(t, err)
	require.NotNil(t, coll)
	found := false
	for _, a := range coll.Attributes {
		if a.Key == "tags" {
			found = true
			require.True(t, a.Array, "catalog 契约读回 array 字段")
		}
	}
	require.True(t, found)
	schema := testSchema(t, projectID, "app")
	physical := testPhysicalName(t, ctx, db, projectID, "app", "docs")
	var udt string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT udt_name FROM information_schema.columns WHERE table_schema = ? AND table_name = ? AND column_name = 'tags'`,
		schema, physical).Scan(&udt))
	require.Equal(t, "_text", udt, "物理列形态 TEXT[]")

	for _, elemType := range []string{"email", "url", "json"} {
		err = docDB.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
			ID: "bad", Key: "bad", Type: elemType, Array: true,
		})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "type=%s", elemType)
		require.Contains(t, err.Error(), "array supports string, integer, float, boolean, datetime")
	}
	err = docDB.CreateCollection(ctx, projectID, "app", "arr_coll", "Arr", []databases.Attribute{
		{ID: "bad", Key: "bad", Type: "json", Array: true},
	}, nil, nil, true)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "array supports string, integer, float, boolean, datetime")
	coll, err = docDB.GetCollection(ctx, projectID, "app", "arr_coll")
	require.NoError(t, err)
	require.Nil(t, coll)
}

// TestQueryVersion_SystemCollectionRejected：系统集合 $version 查询 → InvalidArgument
// （系统表无此列），不落 PG。groups 对 keys 角色可读（非敏感系统集合），
// 走 validateQueryFields 的 isSystem 分支。
func TestQueryVersion_SystemCollectionRejected(t *testing.T) {
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

	_, err := docDB.ListDocuments(ctx, projectID, databases.SystemDatabaseID, "groups", databases.Query{
		AST: &query.Query{Filter: query.Eq("$version", "1")},
	}, databases.Principal{Roles: []string{"keys"}})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "invalid query field")
	require.NotContains(t, err.Error(), "42703", "不得落到 PG 未定义列错误")
	require.False(t, strings.Contains(err.Error(), databases.ErrVersionColumnUnavailable.Error()),
		"系统集合拒绝应走 invalid query field，而非 version_column_unavailable")
}
