// B5 export/import 往返测试（转出门禁三件套）：
//  1. export→drop→import 往返一致性（行数/内容/catalog 逐字段/物理名保真）；
//  2. snapshot_seq 与 `:changes?since_seq=` 的一致性窗口（导出内容不重复
//     出现、导出后的新写入恰为增量）；
//  3. 空项目导出/导入。
package documentdb

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainevents "github.com/torchwoodcloud/torchwood/internal/domain/events"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/infra/events"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
)

func exportImportEnv(t *testing.T) (*postgresDocumentDB, *clients.Database) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	return &postgresDocumentDB{db: db, pub: events.NewEventOutbox(db)}, db
}

// createSeedProject 建项目 + 多库多集合 + 带类型覆盖的文档（_acl/_version/
// 数组/datetime/json/vector 全形态），返回 (projectID, 集合规格)。
func createSeedProject(t *testing.T, ctx context.Context, p *postgresDocumentDB, db *clients.Database) string {
	t.Helper()
	projectID, _, cleanup := testutil.CreateTestProjectT(ctx, t, db)
	t.Cleanup(cleanup)

	require.NoError(t, p.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, p.CreateDatabase(ctx, projectID, "db2", "Second DB"))

	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "notes", "Notes", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "tags", Key: "tags", Type: "string", Array: true},
		{ID: "due", Key: "due", Type: "datetime"},
		{ID: "meta", Key: "meta", Type: "json"},
	}, []databases.Index{
		{ID: "title_key", Type: "key", Attributes: []string{"title"}},
	}, nil, true))
	require.NoError(t, p.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "score", Key: "score", Type: "float"},
		{ID: "ok", Key: "ok", Type: "boolean"},
	}, nil, []databases.Permission{{Type: "read", Role: "any"}}, false))
	require.NoError(t, p.CreateCollection(ctx, projectID, "db2", "items", "Items", []databases.Attribute{
		{ID: "v", Key: "v", Type: "vector", Dims: 4},
	}, []databases.Index{
		{ID: "v_hnsw", Type: "hnsw", Attributes: []string{"v"}, DistanceMetric: "COSINE"},
	}, nil, true))

	notesPerms := []databases.Permission{
		{Type: "read", Role: "user:u1"},
		{Type: "update", Role: "user:u1"},
		{Type: "delete", Role: "user:u1"},
	}
	for i := 1; i <= 5; i++ {
		perms := notesPerms
		if i > 3 {
			perms = nil // 后 2 篇无文档级 _acl（继承集合安全面）
		}
		_, err := p.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
			Data: map[string]any{
				"title": "note-" + string(rune('a'+i-1)),
				"views": int64(i * 10),
				"tags":  []any{"t" + string(rune('a'+i-1)), "shared"},
				"due":   time.Date(2026, 9, i, 8, 30, 0, 0, time.UTC),
				"meta":  map[string]any{"page": i},
			},
		}, perms, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	// 第 1 篇更新一次：_version 升到 2（OCC 列保真对照）。
	first, err := p.ListDocuments(ctx, projectID, "app", "notes", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	target := first.Documents[0]
	_, err = p.UpdateDocument(ctx, projectID, "app", "notes", databases.DocumentUpdate{
		Document: databases.Document{
			ID:   target.ID,
			Data: map[string]any{"views": int64(999)},
		},
		ExpectedVersion: target.Version,
	}, databases.SystemPrincipal)
	require.NoError(t, err)

	for i := 1; i <= 3; i++ {
		_, err := p.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
			Data: map[string]any{"score": float64(i) / 2, "ok": i%2 == 0},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	for i := 1; i <= 2; i++ {
		_, err := p.CreateDocument(ctx, projectID, "db2", "items", databases.Document{
			Data: map[string]any{"v": []any{0.1 * float64(i), 0.2, 0.3, float64(i) / 10}},
		}, nil, databases.SystemPrincipal)
		require.NoError(t, err)
	}
	return projectID
}

// readTableJSON 读物理表全行为 to_jsonb JSON（_id 序），供往返对照。
func readTableJSON(t *testing.T, ctx context.Context, db *clients.Database, schema, physical string) [][]byte {
	t.Helper()
	var rows []struct {
		Doc json.RawMessage `bun:"doc"`
	}
	require.NoError(t, db.NewSelect().ColumnExpr(`to_jsonb(d.*) AS doc`).
		TableExpr(tableName(schema, physical)+` AS d`).
		Order(`d._id ASC`).Scan(ctx, &rows))
	out := make([][]byte, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Doc)
	}
	return out
}

// assertJSONRowsEqual 语义比较两批行 JSON（对象键序无关、数值归一）。
func assertJSONRowsEqual(t *testing.T, before, after [][]byte) {
	t.Helper()
	require.Equal(t, len(before), len(after), "row count mismatch")
	for i := range before {
		var a, b any
		require.NoError(t, json.Unmarshal(before[i], &a))
		require.NoError(t, json.Unmarshal(after[i], &b))
		require.Equal(t, a, b, "row %d content mismatch:\nbefore=%s\nafter=%s", i, before[i], after[i])
	}
}

func assertTimeEqual(t *testing.T, name string, a, b time.Time) {
	t.Helper()
	require.True(t, a.Equal(b), "%s mismatch: %v vs %v", name, a, b)
}

// TestExportImportRoundTrip 是完成判据①：export→drop→import 后行数/内容/
// catalog 逐字段对照，且物理名沿用导出值。
func TestExportImportRoundTrip(t *testing.T) {
	p, db := exportImportEnv(t)
	ctx := context.Background()
	projectID := createSeedProject(t, ctx, p, db)

	outDir := t.TempDir()
	manifest, err := ExportProject(ctx, db, projectID, outDir)
	require.NoError(t, err)
	require.Equal(t, ExportFormatVersion, manifest.FormatVersion)
	require.Equal(t, projectID, manifest.ProjectID)
	require.Len(t, manifest.Databases, 2)
	require.Len(t, manifest.Collections, 3)
	require.Greater(t, manifest.SnapshotSeq, int64(0), "有文档写入必有 outbox 事件")

	// row_count 与物理名记录。
	rowCounts := map[string]int64{}
	physicals := map[string]string{}
	for _, c := range manifest.Collections {
		key := c.DatabaseID + "/" + c.CollectionID
		require.NotEmpty(t, c.PhysicalName, key)
		require.FileExists(t, outDir+"/"+c.DataFile, key)
		physicals[key] = c.PhysicalName
		rowCounts[key] = c.RowCount
	}
	require.Equal(t, int64(5), rowCounts["app/notes"])
	require.Equal(t, int64(3), rowCounts["app/posts"])
	require.Equal(t, int64(2), rowCounts["db2/items"])

	// 导出前快照（各集合全行 JSON）。
	schemaOf := func(dbID string) string {
		s, err := ident.SchemaName(projectID, dbID)
		require.NoError(t, err)
		return s
	}
	beforeNotes := readTableJSON(t, ctx, db, schemaOf("app"), physicals["app/notes"])
	beforePosts := readTableJSON(t, ctx, db, schemaOf("app"), physicals["app/posts"])
	beforeItems := readTableJSON(t, ctx, db, schemaOf("db2"), physicals["db2/items"])

	// ---- drop：删两个业务库（物理表 + catalog 行；outbox 在 public 不受影响）。
	require.NoError(t, p.DeleteDatabase(ctx, projectID, "app"))
	require.NoError(t, p.DeleteDatabase(ctx, projectID, "db2"))

	// ---- import。
	report, err := ImportProject(ctx, db, projectID, outDir)
	require.NoError(t, err)
	require.Equal(t, manifest.SnapshotSeq, report.SnapshotSeq)
	require.Equal(t, int64(10), report.RowsImported)
	require.Contains(t, report.ResumeHint, "since_seq=")

	// ---- catalog_databases 逐字段对照。
	for _, want := range manifest.Databases {
		var got model.DocumentDatabase
		require.NoError(t, db.NewSelect().Model(&got).
			Where("project_id = ? AND database_id = ?", projectID, want.ID).Scan(ctx))
		require.Equal(t, want.Name, got.Name)
		assertTimeEqual(t, "database "+want.ID+" created_at", want.CreatedAt, got.CreatedAt)
		assertTimeEqual(t, "database "+want.ID+" updated_at", want.UpdatedAt, got.UpdatedAt)
	}

	// ---- catalog_collections 逐字段对照（含物理名保真与 JSONB 原文）。
	for _, want := range manifest.Collections {
		var got model.DocumentCollection
		key := want.DatabaseID + "/" + want.CollectionID
		require.NoError(t, db.NewSelect().Model(&got).
			Where("project_id = ? AND database_id = ? AND collection_id = ?",
				projectID, want.DatabaseID, want.CollectionID).Scan(ctx), key)
		require.Equal(t, physicals[key], got.PhysicalName, "物理名必须沿用导出值: %s", key)
		require.Equal(t, want.Name, got.Name)
		require.Equal(t, want.DocumentSecurity, got.DocumentSecurity)
		require.Equal(t, want.Disabled, got.Disabled)
		require.Equal(t, want.Permissions, got.Permissions)
		require.Equal(t, want.Attrs, got.Attrs)
		require.Equal(t, want.Indexes, got.Indexes)
		require.Equal(t, want.SchemaVersion, got.SchemaVersion)
		require.Equal(t, want.DDLSeq, got.DDLSeq)
		assertTimeEqual(t, key+" created_at", want.CreatedAt, got.CreatedAt)
		assertTimeEqual(t, key+" updated_at", want.UpdatedAt, got.UpdatedAt)
	}

	// ---- 行内容/行数对照（含 _acl/_version/数组/datetime/json/vector）。
	assertJSONRowsEqual(t, beforeNotes, readTableJSON(t, ctx, db, schemaOf("app"), physicals["app/notes"]))
	assertJSONRowsEqual(t, beforePosts, readTableJSON(t, ctx, db, schemaOf("app"), physicals["app/posts"]))
	assertJSONRowsEqual(t, beforeItems, readTableJSON(t, ctx, db, schemaOf("db2"), physicals["db2/items"]))

	// ---- _acl/_version 保真抽验：notes 有 _acl 的行 RLS 判定仍生效
	//（user:u1 可见 3 篇）、无 _acl 的行（document_security=true 时不可见）。
	visible, err := p.ListDocuments(ctx, projectID, "app", "notes", databases.Query{},
		databases.Principal{Roles: []string{"user:u1"}})
	require.NoError(t, err)
	require.Len(t, visible.Documents, 3, "_acl 保真后可见性判定必须与导出前一致")

	// ---- 往返后再走现役写路径：导入的集合可直接增改查（列授权/RLS/_acl 通道健全）。
	created, err := p.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		Data: map[string]any{"title": "post-import", "views": int64(1)},
	}, notesACL(), databases.SystemPrincipal)
	require.NoError(t, err)
	require.NotEmpty(t, created.ID)
	require.Equal(t, int64(1), created.Version)
}

func notesACL() []databases.Permission {
	return []databases.Permission{{Type: "read", Role: "user:u1"}}
}

// TestExportSnapshotSeqChangesContinuity 是完成判据②：export 的 snapshot_seq
// 与 `:changes?since_seq=` 无缝续接——导出内容不出现，导出后的新写入恰为增量。
func TestExportSnapshotSeqChangesContinuity(t *testing.T) {
	p, db := exportImportEnv(t)
	ctx := context.Background()
	projectID := createSeedProject(t, ctx, p, db)

	outDir := t.TempDir()
	manifest, err := ExportProject(ctx, db, projectID, outDir)
	require.NoError(t, err)

	// snapshot_seq = 导出快照内 outbox 全局 max(seq)。
	var maxSeq int64
	require.NoError(t, db.NewSelect().TableExpr("document_events_outbox").
		ColumnExpr("COALESCE(MAX(seq), 0)").Scan(ctx, &maxSeq))
	require.Equal(t, maxSeq, manifest.SnapshotSeq)

	// 一致性窗口下界：since_seq=snapshot_seq 当前为空（导出内容不重复出现），
	// 且不触发 ErrResumeExpired（游标仍在 outbox 可用窗口内）。
	changes, hasMore, _, err := p.ListChanges(ctx, projectID, "app", "notes",
		databases.ListChangesOptions{SinceSeq: manifest.SnapshotSeq}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Empty(t, changes)

	// 导出后写入新文档：`:changes?since_seq=<snapshot_seq>` 恰返回新文档
	//（不含导出内容）。
	created, err := p.CreateDocument(ctx, projectID, "app", "notes", databases.Document{
		Data: map[string]any{"title": "after-export", "views": int64(1)},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	changes, hasMore, _, err = p.ListChanges(ctx, projectID, "app", "notes",
		databases.ListChangesOptions{SinceSeq: manifest.SnapshotSeq}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Len(t, changes, 1)
	require.Equal(t, created.ID, changes[0].DocumentID)
	require.Equal(t, domainevents.EventDocumentsCreate, changes[0].Event)
	require.NotNil(t, changes[0].Data, "create 事件带全文档（增量可直接收敛本地副本）")
}

// TestExportImportEmptyProject 是完成判据③的空项目面：无库无集合时导出
// manifest 为空但 snapshot_seq 语义完整，导入幂等成功。
func TestExportImportEmptyProject(t *testing.T) {
	_, db := exportImportEnv(t)
	ctx := context.Background()

	projectID, _, cleanup := testutil.CreateTestProjectT(ctx, t, db)
	t.Cleanup(cleanup)

	outDir := t.TempDir()
	manifest, err := ExportProject(ctx, db, projectID, outDir)
	require.NoError(t, err)
	require.Empty(t, manifest.Databases)
	require.Empty(t, manifest.Collections)
	require.GreaterOrEqual(t, manifest.SnapshotSeq, int64(0))

	report, err := ImportProject(ctx, db, projectID, outDir)
	require.NoError(t, err)
	require.Empty(t, report.CollectionsRestored)
	require.Equal(t, manifest.SnapshotSeq, report.SnapshotSeq)

	// 导入器拒收 manifest 缺失（半成品目录）与项目不匹配的导入物。
	_, err = ImportProject(ctx, db, projectID, t.TempDir())
	require.Error(t, err)
	_, err = ImportProject(ctx, db, "otherproj", outDir)
	require.ErrorContains(t, err, "does not match")
}
