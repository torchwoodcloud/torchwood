package documentdb

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
)

// outboxTestProject 创建带 EventPublisher 的 DocumentDB 测试环境：
// "app"."docs" 用户集合（documentSecurity=true，集合级 create/read，
// 文档级写权限由 _perms 决定）。
func outboxTestProject(t *testing.T, ctx context.Context) (databases.DocumentDB, *clients.Database, string, func()) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	docDB := NewPostgresDocumentDB(db, events.NewEventOutbox(db))
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "blob", Key: "blob", Type: "json"},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
		{Type: "read", Role: "any"},
		// 文档级写权限仅由 _perms 授予（避免集合级兜底影响断言）。
	}, true))
	cleanupFn := func() {
		_ = db.Close()
		cleanup()
	}
	return docDB, db, projectID, cleanupFn
}

// outboxOwnerPerms 是文档级写权限（owner user:u1 可 read/update/delete）。
func outboxOwnerPerms() []databases.Permission {
	return []databases.Permission{
		{Type: "read", Role: "user:u1"},
		{Type: "update", Role: "user:u1"},
		{Type: "delete", Role: "user:u1"},
	}
}

func outboxCreate(t *testing.T, docDB databases.DocumentDB, projectID, id string) databases.Document {
	t.Helper()
	created, err := docDB.CreateDocument(context.Background(), projectID, "app", "docs", databases.Document{
		ID:   id,
		Data: map[string]any{"title": "t1"},
	}, outboxOwnerPerms(), occPrincipal())
	require.NoError(t, err)
	return created
}

func outboxRows(t *testing.T, db *clients.Database, ctx context.Context) []model.DocumentEventsOutbox {
	t.Helper()
	var rows []model.DocumentEventsOutbox
	err := db.Conn(ctx).NewSelect().Model(&rows).Order("created_at ASC", "event_id ASC").Scan(ctx)
	require.NoError(t, err)
	return rows
}

func outboxPayload(t *testing.T, row model.DocumentEventsOutbox) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(row.Payload, &m))
	return m
}

// outboxVersion 断言 payload 中的 version（JSONB 数字为 float64）。
func outboxVersion(t *testing.T, m map[string]any, want int64) {
	t.Helper()
	require.Equal(t, float64(want), m["version"])
}

// TestOutbox_CreatePublishesCreateEvent：成功 Create 在同一事务落一行
// databases.documents.create；payload 为写后全文档、version=1、acl=写后。
func TestOutbox_CreatePublishesCreateEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()

	created := outboxCreate(t, docDB, projectID, "d1")

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 1)
	require.Equal(t, "databases.app.collections.docs", rows[0].Topic)
	m := outboxPayload(t, rows[0])
	require.Equal(t, domainevents.EventDocumentsCreate, m["event"])
	require.Equal(t, projectID, m["project_id"])
	require.Equal(t, "app", m["database_id"])
	require.Equal(t, "docs", m["collection_id"])
	require.Equal(t, "d1", m["document_id"])
	outboxVersion(t, m, 1)
	require.Equal(t, false, m["truncated"])
	require.NotContains(t, m, "transaction_id")

	data, ok := m["data"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "d1", data["id"])
	outboxVersion(t, data, 1)
	require.Equal(t, "t1", data["data"].(map[string]any)["title"])
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, data["permissions"])

	acl := m["acl"].(map[string]any)
	require.Equal(t, true, acl["document_security"])
	require.Equal(t, true, acl["doc_has_perms"])
	require.Equal(t, []any{"create:users", "read:any"}, acl["collection_permissions"])
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, acl["document_permissions"])
	require.Equal(t, created.ID, m["document_id"])
}

// TestOutbox_UpdatePublishesUpdateEvent：Update 与 Increment 落
// databases.documents.update；acl=写前 _perms，version=写后。
func TestOutbox_UpdatePublishesUpdateEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()
	created := outboxCreate(t, docDB, projectID, "d1")

	// Update（data + increment 不能落在同一列，否则 SQL 重复赋值；version 必传）。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document: databases.Document{
			ID:   created.ID,
			Data: map[string]any{"title": "t2"},
		},
		Increment:       map[string]int64{"views": 5},
		ExpectedVersion: 1,
	}, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Version)

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 2, "Create + Update 各一行")
	updateRow := rows[1]
	m := outboxPayload(t, updateRow)
	require.Equal(t, domainevents.EventDocumentsUpdate, m["event"])
	outboxVersion(t, m, 2)
	data := m["data"].(map[string]any)
	require.Equal(t, "t2", data["data"].(map[string]any)["title"])
	require.Equal(t, float64(5), data["data"].(map[string]any)["views"], "Increment 合并进同一 update 事件")

	acl := m["acl"].(map[string]any)
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, acl["document_permissions"])
	require.Equal(t, true, acl["doc_has_perms"])
}

// TestOutbox_UpdateACLSnapshotIsPreWrite：update 同时替换 _perms 后，
// envelope.acl 仍为写前权限；data.permissions 为写后权限。
func TestOutbox_UpdateACLSnapshotIsPreWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()
	created := outboxCreate(t, docDB, projectID, "d1")

	newPerms := []databases.Permission{
		{Type: "read", Role: "user:u2"},
		{Type: "update", Role: "user:u2"},
		{Type: "delete", Role: "user:u2"},
	}
	_, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"title": "t2"}},
		Permissions:     newPerms,
		ExpectedVersion: 1,
	}, occPrincipal())
	require.NoError(t, err)

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 2)
	m := outboxPayload(t, rows[1])
	acl := m["acl"].(map[string]any)
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, acl["document_permissions"],
		"acl 必须是写前权限（未被本次替换影响）")
	data := m["data"].(map[string]any)
	require.ElementsMatch(t, []string{"read:user:u2", "update:user:u2", "delete:user:u2"}, data["permissions"])
}

// TestOutbox_DeletePublishesDeleteEvent：Delete 落 databases.documents.delete，
// 无 data、version=删除前、acl=写前。
func TestOutbox_DeletePublishesDeleteEvent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()
	created := outboxCreate(t, docDB, projectID, "d1")

	// 先 update 一次让 version=2，删除前快照应取 2。
	_, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"title": "t2"}},
		ExpectedVersion: 1,
	}, occPrincipal())
	require.NoError(t, err)

	require.NoError(t, docDB.DeleteDocument(ctx, projectID, "app", "docs", created.ID,
		databases.DeleteOptions{ExpectedVersion: 2}, occPrincipal()))

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 3)
	m := outboxPayload(t, rows[2])
	require.Equal(t, domainevents.EventDocumentsDelete, m["event"])
	outboxVersion(t, m, 2)
	_, hasData := m["data"]
	require.False(t, hasData, "delete 事件无 data")
	acl := m["acl"].(map[string]any)
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, acl["document_permissions"])
	require.Equal(t, true, acl["doc_has_perms"])

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Nil(t, got, "文档已删除")
}

// TestOutbox_BulkPublishesPerDocument：BulkUpdate / BulkDelete 每篇各落一行
// （SkipVersion 不跳过事件）。
func TestOutbox_BulkPublishesPerDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()
	d1 := outboxCreate(t, docDB, projectID, "d1")
	d2 := outboxCreate(t, docDB, projectID, "d2")

	affected, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs",
		[]string{d1.ID, d2.ID}, map[string]any{"title": "bulk"}, nil, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 4, "2×create + 2×update")
	for _, row := range rows[2:] {
		m := outboxPayload(t, row)
		require.Equal(t, domainevents.EventDocumentsUpdate, m["event"])
		outboxVersion(t, m, 2)
	}

	affected, err = docDB.BulkDeleteDocuments(ctx, projectID, "app", "docs",
		[]string{d1.ID, d2.ID}, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)

	rows = outboxRows(t, db, ctx)
	require.Len(t, rows, 6, "2×create + 2×update + 2×delete")
	for _, row := range rows[4:] {
		m := outboxPayload(t, row)
		require.Equal(t, domainevents.EventDocumentsDelete, m["event"])
		outboxVersion(t, m, 2)
		_, hasData := m["data"]
		require.False(t, hasData)
	}
}

// TestOutbox_BulkUpdatePermReplace_PreACL (R5-P2-6)：BulkUpdate 携带 perms
// 替换时每文档一条 update 事件——version/data=写后（data.permissions=新
// perms），acl=写前快照（owner ACE）；随后 BulkDelete 的 delete 事件
// version/acl 均基于写前（替换后的 perms）、无 data。
func TestOutbox_BulkUpdatePermReplace_PreACL(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()

	b1 := outboxCreate(t, docDB, projectID, "b1")
	b2 := outboxCreate(t, docDB, projectID, "b2")

	newPerms := []databases.Permission{
		{Type: "read", Role: "user:u2"},
		{Type: "update", Role: "user:u2"},
		{Type: "delete", Role: "user:u2"},
	}
	affected, err := docDB.BulkUpdateDocuments(ctx, projectID, "app", "docs",
		[]string{b1.ID, b2.ID}, map[string]any{"title": "bulk2"}, newPerms, occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 4, "2×create + 2×update")
	for _, row := range rows[2:] {
		m := outboxPayload(t, row)
		require.Equal(t, domainevents.EventDocumentsUpdate, m["event"])
		outboxVersion(t, m, 2)
		data, ok := m["data"].(map[string]any)
		require.True(t, ok)
		require.Equal(t, "bulk2", data["data"].(map[string]any)["title"])
		require.ElementsMatch(t, []string{"read:user:u2", "update:user:u2", "delete:user:u2"},
			data["permissions"], "data.permissions=写后（新 perms）")
		acl := m["acl"].(map[string]any)
		require.Equal(t, true, acl["doc_has_perms"])
		require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"},
			acl["document_permissions"], "acl=写前快照（owner ACE）")
	}

	// perms 已替换给 u2，u1 不再持有 delete → 以 SystemPrincipal 删除
	// （旁路 ACL；事件照常发布，version/acl 取写前）。
	affected, err = docDB.BulkDeleteDocuments(ctx, projectID, "app", "docs",
		[]string{b1.ID, b2.ID}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), affected)

	rows = outboxRows(t, db, ctx)
	require.Len(t, rows, 6)
	for _, row := range rows[4:] {
		m := outboxPayload(t, row)
		require.Equal(t, domainevents.EventDocumentsDelete, m["event"])
		outboxVersion(t, m, 2)
		_, hasData := m["data"]
		require.False(t, hasData, "delete 事件无 data")
		acl := m["acl"].(map[string]any)
		require.ElementsMatch(t, []string{"read:user:u2", "update:user:u2", "delete:user:u2"},
			acl["document_permissions"], "delete acl=写前（替换后的 perms）")
	}
}

// TestOutbox_UpsertEvents：Upsert 插入支 → create（acl=写后）；
// 更新支 → update（acl=写前）。
func TestOutbox_UpsertEvents(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()

	// 独立集合：conflict 列 title 需要唯一索引（ON CONFLICT 要求）。
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "members", "Members", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "name", Key: "name", Type: "string", Size: 256},
	}, []databases.Index{
		{ID: "uq_title", Type: "unique", Attributes: []string{"title"}},
	}, nil, true))

	// 插入支（SystemPrincipal 旁路权限，方便断言事件分叉）。
	created, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "u1",
		Data: map[string]any{"title": "first", "name": "n1"},
	}, []string{"title"}, outboxOwnerPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(1), created.Version)

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 1)
	m := outboxPayload(t, rows[0])
	require.Equal(t, domainevents.EventDocumentsCreate, m["event"])
	outboxVersion(t, m, 1)
	acl := m["acl"].(map[string]any)
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, acl["document_permissions"])
	require.Equal(t, true, acl["doc_has_perms"])

	// 更新支：盲写 +1，事件为 update，acl=写前。
	updated, err := docDB.UpsertDocument(ctx, projectID, "app", "members", databases.Document{
		ID:   "u1",
		Data: map[string]any{"title": "first", "name": "n2"},
	}, []string{"title"}, outboxOwnerPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, int64(2), updated.Version)

	rows = outboxRows(t, db, ctx)
	require.Len(t, rows, 2)
	m = outboxPayload(t, rows[1])
	require.Equal(t, domainevents.EventDocumentsUpdate, m["event"])
	outboxVersion(t, m, 2)
	acl = m["acl"].(map[string]any)
	require.ElementsMatch(t, []string{"read:user:u1", "update:user:u1", "delete:user:u1"}, acl["document_permissions"], "更新支 acl=写前")
	require.Equal(t, "n2", m["data"].(map[string]any)["data"].(map[string]any)["name"])
}

// TestOutbox_FailedWriteNoRow：失败写（OCC 不匹配 / 权限不足）不产生
// outbox 行，已提交的行不受影响。
func TestOutbox_FailedWriteNoRow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()
	created := outboxCreate(t, docDB, projectID, "d1")

	_, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"title": "stale"}},
		ExpectedVersion: 99,
	}, occPrincipal())
	require.ErrorIs(t, err, databases.ErrVersionMismatch)
	require.Len(t, outboxRows(t, db, ctx), 1, "OCC 失败不产生 outbox 行")

	_, err = docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"title": "hacked"}},
		ExpectedVersion: 1,
	}, databases.Principal{Roles: []string{"users", "user:other"}})
	// 阶段③包 C：不可见 = 不存在（防枚举）——NotFound 取代 PermissionDenied。
	require.ErrorIs(t, err, databases.ErrDocumentNotFound)
	require.Len(t, outboxRows(t, db, ctx), 1, "权限失败不产生 outbox 行")

	// 阶段③包 C：不可见删除同理（探测经 SELECT policy ⇒ NotFound）。
	require.ErrorIs(t, docDB.DeleteDocument(ctx, projectID, "app", "docs", created.ID,
		databases.DeleteOptions{ExpectedVersion: 1}, databases.Principal{Roles: []string{"users", "user:other"}}),
		databases.ErrDocumentNotFound)
	require.Len(t, outboxRows(t, db, ctx), 1, "删除权限失败不产生 outbox 行")

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, "t1", got.Data["title"], "失败写不得改行")
}

// TestOutbox_SystemCollectionNoRows：系统集合写不产生 outbox 行。
func TestOutbox_SystemCollectionNoRows(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()
	docDB := NewPostgresDocumentDB(db, events.NewEventOutbox(db))
	require.NoError(t, testutil.SeedSystemDocumentCollections(ctx, db, docDB, projectID))

	_, err := docDB.CreateDocument(ctx, projectID, databases.SystemDatabaseID, "users", databases.Document{
		ID:   "u1",
		Data: map[string]any{"email": "a@b.c", "status": "active"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	_, err = docDB.UpdateDocument(ctx, projectID, databases.SystemDatabaseID, "users", databases.DocumentUpdate{
		Document: databases.Document{ID: "u1", Data: map[string]any{"status": "blocked"}},
	}, databases.SystemPrincipal)
	require.NoError(t, err)

	require.NoError(t, docDB.DeleteDocument(ctx, projectID, databases.SystemDatabaseID, "users", "u1",
		databases.DeleteOptions{SkipVersion: true}, databases.SystemPrincipal))

	require.Len(t, outboxRows(t, db, ctx), 0, "系统集合写不得产生 outbox 行")
}

// TestOutbox_OversizedDocumentTruncated：超大文档（>1 MiB 信封，防御性
// 截断路径——写入面 app 层已拒 1 MiB，此处直连 docDB 绕过该校验）写成功，
// outbox 行 truncated=true 且不含 data；业务行存在。
func TestOutbox_OversizedDocumentTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()

	blob := strings.Repeat("x", 600*1024)
	created, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "big",
		Data: map[string]any{"title": "t", "blob": map[string]any{"text": blob, "text2": blob}},
	}, outboxOwnerPerms(), occPrincipal())
	require.NoError(t, err)
	require.Equal(t, int64(1), created.Version, "超限事件不得回滚业务写")

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 1)
	m := outboxPayload(t, rows[0])
	require.Equal(t, true, m["truncated"])
	_, hasData := m["data"]
	require.False(t, hasData, "截断后 payload 不得含 data")
	require.Equal(t, domainevents.EventDocumentsCreate, m["event"])
	require.Equal(t, "big", m["document_id"])

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", "big", databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotNil(t, got, "业务行必须存在")
	require.Equal(t, len(blob), len(got.Data["blob"].(map[string]any)["text"].(string)))
	require.Equal(t, len(blob), len(got.Data["blob"].(map[string]any)["text2"].(string)))
}

// TestOutbox_ExecuteTxSharesTransactionID（阶段④包 A，§4.8）：execute-tx
// 批内全部事件共带同一非空 transaction_id；事件 seq 顺序 = op 顺序；
// 单文档路径 transaction_id 缺省。
func TestOutbox_ExecuteTxSharesTransactionID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, db, projectID, cleanup := outboxTestProject(t, ctx)
	defer cleanup()

	// 单文档路径先行：不带 transaction_id。
	outboxCreate(t, docDB, projectID, "solo")

	v1 := int64(1)
	_, err := docDB.ExecuteTransactions(ctx, projectID, "app", []databases.TransactionOp{
		{Type: databases.TransactionOpCreate, CollectionID: "docs", DocumentID: "t1",
			Data: map[string]any{"title": "one"}, Permissions: outboxOwnerPerms()},
		{Type: databases.TransactionOpCreate, CollectionID: "docs", DocumentID: "t2",
			Data: map[string]any{"title": "two"}, Permissions: outboxOwnerPerms()},
		{Type: databases.TransactionOpUpdate, CollectionID: "docs", DocumentID: "t1",
			Data: map[string]any{"title": "uno"}, ExpectedVersion: &v1},
	}, databases.TransactionModeAtomic, occPrincipal())
	require.NoError(t, err)

	rows := outboxRows(t, db, ctx)
	require.Len(t, rows, 4)
	solo := outboxPayload(t, rows[0])
	require.Equal(t, "solo", solo["document_id"])
	_, hasTx := solo["transaction_id"]
	require.False(t, hasTx, "单文档路径不得带 transaction_id")

	batchIDs := map[string]bool{}
	for _, row := range rows[1:] {
		m := outboxPayload(t, row)
		require.NotEmpty(t, m["transaction_id"], "批内事件必须共带 transaction_id")
		batchIDs[m["transaction_id"].(string)] = true
	}
	require.Len(t, batchIDs, 1, "同批 transaction_id 必须一致")

	// 事件顺序 = op 序：create t1 → create t2 → update t1（按 seq 升序核对）。
	require.True(t, rows[1].Seq < rows[2].Seq && rows[2].Seq < rows[3].Seq, "seq 必须随 op 序单调")
	require.Equal(t, "t1", outboxPayload(t, rows[1])["document_id"])
	require.Equal(t, "t2", outboxPayload(t, rows[2])["document_id"])
	require.Equal(t, domainevents.EventDocumentsUpdate, outboxPayload(t, rows[3])["event"])
}

// TestOutbox_NoPublisherIsNop：未注入 EventPublisher（单测）时写路径无
// outbox 副作用。
func TestOutbox_NoPublisherIsNop(t *testing.T) {
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
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
	}, nil, []databases.Permission{
		{Type: "create", Role: "users"},
	}, true))

	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "d1",
		Data: map[string]any{"title": "t"},
	}, outboxOwnerPerms(), occPrincipal())
	require.NoError(t, err)
	require.Len(t, outboxRows(t, db, ctx), 0)
}
