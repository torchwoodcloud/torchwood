// B13c（存在性缓存，原物理名缓存）：失效桥接测试——删建同逻辑 ID 的集合/库
// 后，缓存必须收敛到 catalog 现值（物理名恒 = collectionID，2026-09-06 勘误）：
// 文档落重建后的表、旧表已消亡、跨路径（缓存命中路径）与 catalog 一致。
package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

// TestPhysicalNameCache_DeleteRecreateBridge：DeleteCollection 后重建同逻辑
// ID 集合——新物理名写穿进缓存，后续写入落新表（若缓存未失效，写入会打在
// 已 DROP 的旧表上以 42P01 显式失败，或读回旧数据）。
func TestPhysicalNameCache_DeleteRecreateBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 64}}, nil, nil, true))
	_, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		ID: "p1", Data: map[string]any{"title": "v1"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)

	physicalA := testPhysicalName(t, ctx, db, projectID, "app", "posts")
	require.Equal(t, "posts", physicalA)
	// 点查一次填充缓存（模拟热路径已命中）。
	_, _, gotA, err := docDB.(*postgresDocumentDB).resolvePhysicalTable(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, physicalA, gotA)

	// 删集合（缓存失效，物理表随即消亡）→ 同逻辑 ID 重建（同名物理表，
	// 缓存写穿收敛）。
	require.NoError(t, docDB.DeleteCollection(ctx, projectID, "app", "posts"))
	// 同名复用语义下"表已消亡"只能在删除后、重建前的中间态观测。
	var gone bool
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT NOT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = ? AND table_name = ?)`,
		testSchema(t, projectID, "app"), physicalA).Scan(&gone))
	require.True(t, gone, "deleted physical table must be dropped")
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 64}}, nil, nil, true))

	physicalB := testPhysicalName(t, ctx, db, projectID, "app", "posts")
	require.Equal(t, physicalA, physicalB, "物理名恒 = collectionID，删建同名复用同名表")
	// 缓存命中路径必须返回 catalog 现值。
	_, _, gotB, err := docDB.(*postgresDocumentDB).resolvePhysicalTable(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, physicalB, gotB)

	// 新文档经缓存路径落新表。
	_, err = docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		ID: "p2", Data: map[string]any{"title": "v2"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1, "recreated collection must start empty (p1 gone with old table)")
	require.Equal(t, "p2", list.Documents[0].ID)

	// 旧数据不可达（adapter 契约：缺失行返回 nil 文档而非错误）。
	got, err := docDB.GetDocument(ctx, projectID, "app", "posts", "p1", databases.SystemPrincipal)
	require.NoError(t, err)
	require.Nil(t, got, "p1 must not exist in the recreated collection")
}

// TestPhysicalNameCache_DeleteDatabaseBridge：DeleteDatabase 批量失效该库全部
// 集合键——重建同名库与集合后写入必须落新物理表。
func TestPhysicalNameCache_DeleteDatabaseBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 64}}, nil, nil, true))
	_, err := docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		ID: "p1", Data: map[string]any{"title": "v1"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	physicalA := testPhysicalName(t, ctx, db, projectID, "app", "posts")
	_, _, _, err = docDB.(*postgresDocumentDB).resolvePhysicalTable(ctx, projectID, "app", "posts")
	require.NoError(t, err) // 填充缓存

	require.NoError(t, docDB.DeleteDatabase(ctx, projectID, "app"))
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 64}}, nil, nil, true))
	physicalB := testPhysicalName(t, ctx, db, projectID, "app", "posts")
	require.Equal(t, physicalA, physicalB, "物理名恒 = collectionID")

	_, err = docDB.CreateDocument(ctx, projectID, "app", "posts", databases.Document{
		ID: "p2", Data: map[string]any{"title": "v2"},
	}, nil, databases.SystemPrincipal)
	require.NoError(t, err)
	list, err := docDB.ListDocuments(ctx, projectID, "app", "posts", databases.Query{}, databases.SystemPrincipal)
	require.NoError(t, err)
	require.Len(t, list.Documents, 1)
	require.Equal(t, "p2", list.Documents[0].ID)
}
