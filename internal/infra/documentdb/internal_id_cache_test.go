package documentdb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/testutil"
	"github.com/torchwooddev/torchwood/pkg/ident"
)

// TestInternalIDCache_InvalidationOnRecreate：Round4 J5-3——删除项目后同 ID
// 重建，internalIDCache 必须失效：否则旧实例以陈旧 internal_id 打 _tenant
// 标签，新数据静默分裂到错误租户命名空间。
func TestInternalIDCache_InvalidationOnRecreate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, firstInternalID, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}}, nil, nil, true))

	appSchema, err := ident.SchemaName(projectID, "app")
	require.NoError(t, err)

	writePost := func(title string) string {
		created, err := docDB.CreateDocument(ctx, projectID, "app", "posts",
			databases.Document{Data: map[string]any{"title": title}},
			[]databases.Permission{{Type: "read", Role: "any"}},
			databases.SystemPrincipal)
		require.NoError(t, err)
		return created.ID
	}

	tenantOf := func(t *testing.T, docID string) int64 {
		t.Helper()
		var tenant int64
		physical := testPhysicalName(t, ctx, db, projectID, "app", "posts")
		require.NoError(t, db.NewSelect().
			TableExpr(fmt.Sprintf("%q.%q", appSchema, physical)).
			Column("_tenant").
			Where("_id = ?", docID).
			Limit(1).
			Scan(ctx, &tenant))
		return tenant
	}

	// 第一次写：填充 internalIDCache（internal_id = firstInternalID）。
	id1 := writePost("before-delete")
	require.Equal(t, firstInternalID, tenantOf(t, id1), "_tenant 应为首次解析的 internal_id")

	// 模拟项目删除：DROP 业务 schema + 清全局 catalog 行 + 删控制面行
	// （缓存此时已陈旧；catalog 位于 public 两表，见 db/migrations/000003）。
	_, err = db.ExecContext(ctx,
		`DROP SCHEMA IF EXISTS `+quoteIdent(appSchema)+` CASCADE`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`DELETE FROM public.catalog_collections WHERE project_id = ?`, projectID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`DELETE FROM public.catalog_databases WHERE project_id = ?`, projectID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM public.projects WHERE id = ?`, projectID)
	require.NoError(t, err)

	// 同 ID 重建：internal_id 自增得到新值。
	rebuilt := &model.Project{
		ID:        projectID,
		Name:      projectID + "-rebuilt-" + time.Now().Format("150405.000000000"),
		Status:    "active",
		Settings:  map[string]any{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	require.NoError(t, db.NewInsert().Model(rebuilt).Scan(ctx))
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM public.projects WHERE id = ?`, rebuilt.ID)
	})
	require.NotEqual(t, firstInternalID, rebuilt.InternalID, "重建应获得新的 internal_id")
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}}, nil, nil, true))

	// 未失效时写入会带陈旧 _tenant；失效后必须重新解析为新值（J5-3 接线语义）。
	docDB.(InternalIDCacheInvalidator).InvalidateInternalIDCache(projectID)
	id2 := writePost("after-recreate")
	require.Equal(t, rebuilt.InternalID, tenantOf(t, id2),
		"失效重建后 _tenant 必须是新 internal_id")
}
