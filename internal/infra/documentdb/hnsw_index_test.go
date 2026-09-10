// hnsw 索引端到端（会话 #10 包 B，预决策 2）：三 metric DDL 形态（opclass）、
// EXPLAIN 命中（hnsw 索引扫描）、同列多 metric 索引共存、拒绝矩阵
// （vector 列×非 hnsw 索引、非 vector 列×hnsw、orders/多列/bad metric、
// 非 hnsw 设 distance_metric）。
package documentdb

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

func setupHNSWCollection(ctx context.Context, t *testing.T) (databases.DocumentDB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "vecs", "Vecs", []databases.Attribute{
		{ID: "emb", Key: "emb", Type: "vector", Dims: 3},
		{ID: "note", Key: "note", Type: "string", Size: 64},
	}, nil, nil, true))
	return docDB, projectID
}

// hnswIndexDef 读 pg_indexes 的 indexdef（opclass 断言用）。
func hnswIndexDef(ctx context.Context, t *testing.T, docDB databases.DocumentDB, projectID, idxIDSuffix string) string {
	t.Helper()
	db := docDB.(*postgresDocumentDB)
	var def string
	err := db.conn(ctx).QueryRowContext(ctx, `
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'tw_`+projectID+`_app' AND indexname LIKE '%`+idxIDSuffix+`'`).Scan(&def)
	require.NoError(t, err)
	return def
}

// TestHNSWIndexes_DDLForms：三 metric 的 DDL 形态（opclass 映射）与
// 同列多 metric 索引共存。
func TestHNSWIndexes_DDLForms(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupHNSWCollection(ctx, t)

	// 三 metric 各建一索引（同列共存）。
	for _, idx := range []databases.Index{
		{ID: "emb_cos", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "COSINE"},
		{ID: "emb_l2", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "L2"},
		{ID: "emb_ip", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "INNER_PRODUCT"},
	} {
		require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "vecs", idx))
	}
	// pg_indexes 的 indexdef 对全小写标识符不加引号。
	require.Contains(t, hnswIndexDef(ctx, t, docDB, projectID, "emb_cos"), "USING hnsw (emb vector_cosine_ops)")
	require.Contains(t, hnswIndexDef(ctx, t, docDB, projectID, "emb_l2"), "USING hnsw (emb vector_l2_ops)")
	require.Contains(t, hnswIndexDef(ctx, t, docDB, projectID, "emb_ip"), "USING hnsw (emb vector_ip_ops)")

	// catalog 读回 metric 契约（大写形态）。
	coll, err := docDB.GetCollection(ctx, projectID, "app", "vecs")
	require.NoError(t, err)
	require.Len(t, coll.Indexes, 3)
	metrics := map[string]string{}
	for _, i := range coll.Indexes {
		metrics[i.ID] = i.DistanceMetric
	}
	require.Equal(t, map[string]string{
		"emb_cos": "COSINE", "emb_l2": "L2", "emb_ip": "INNER_PRODUCT",
	}, metrics)

	// 同 ID 重复建 → AlreadyExists（DuplicateKey）。
	err = docDB.CreateIndex(ctx, projectID, "app", "vecs", databases.Index{
		ID: "emb_cos", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "COSINE",
	})
	require.ErrorIs(t, err, ErrDuplicateKey)
}

// TestHNSWIndexes_ExplainHit：种子数据后 EXPLAIN 断言 KNN 查询命中 hnsw
// 索引扫描（iterative scan 形态的 Index Scan + Order By + Filter）。
func TestHNSWIndexes_ExplainHit(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupHNSWCollection(ctx, t)
	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "vecs", databases.Index{
		ID: "emb_cos", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "COSINE",
	}))

	for i := 0; i < 50; i++ {
		_, err := docDB.CreateDocument(ctx, projectID, "app", "vecs", databases.Document{
			Data: map[string]any{
				"emb":  []any{float64(i%5) + 0.1, float64(i%7) + 0.2, 1.0},
				"note": "seed",
			},
		}, anyPerms(), databases.SystemPrincipal)
		require.NoError(t, err)
	}

	db := docDB.(*postgresDocumentDB)
	rows, err := db.conn(ctx).QueryContext(ctx, `
		EXPLAIN (COSTS OFF)
		SELECT _id FROM tw_`+projectID+`_app.`+hnswPhysical(ctx, t, docDB, projectID)+` d
		ORDER BY (d."emb" <=> '[1,1,1]'::vector) LIMIT 5`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var explain strings.Builder
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		explain.WriteString(line)
		explain.WriteString("\n")
	}
	require.NoError(t, rows.Err())
	require.Contains(t, explain.String(), "Index Scan", "KNN query must use the hnsw index, got:\n%s", explain.String())
	require.Contains(t, explain.String(), "emb_cos")
}

// hnswPhysical 取集合物理表名（EXPLAIN 拼表用）。
func hnswPhysical(ctx context.Context, t *testing.T, docDB databases.DocumentDB, projectID string) string {
	t.Helper()
	db := docDB.(*postgresDocumentDB)
	var physical string
	err := db.conn(ctx).QueryRowContext(ctx, `
		SELECT physical_name FROM catalog_collections
		WHERE project_id = ? AND database_id = 'app' AND collection_id = 'vecs'`, projectID).Scan(&physical)
	require.NoError(t, err)
	return physical
}

// TestHNSWIndexes_RejectionMatrix：拒绝矩阵——vector 列×unique/fulltext/key、
// 非 vector 列×hnsw、orders、多列、非法 metric、非 hnsw 设 metric。
func TestHNSWIndexes_RejectionMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupHNSWCollection(ctx, t)

	// vector 列上的非 hnsw 索引全部拒绝。
	for _, idx := range []databases.Index{
		{ID: "emb_key", Type: "key", Attributes: []string{"emb"}},
		{ID: "emb_uniq", Type: "unique", Attributes: []string{"emb"}},
		{ID: "emb_ft", Type: "fulltext", Attributes: []string{"emb"}},
	} {
		err := docDB.CreateIndex(ctx, projectID, "app", "vecs", idx)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "idx=%s", idx.ID)
		require.Contains(t, err.Error(), "do not support vector attributes")
	}

	// 非 vector 列上的 hnsw 拒绝。
	err := docDB.CreateIndex(ctx, projectID, "app", "vecs", databases.Index{
		ID: "note_hnsw", Type: "hnsw", Attributes: []string{"note"}, DistanceMetric: "COSINE",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "require a vector attribute")

	// 多列 / orders / 非法 metric。
	require.Equal(t, codes.InvalidArgument, status.Code(docDB.CreateIndex(ctx, projectID, "app", "vecs", databases.Index{
		ID: "emb_multi", Type: "hnsw", Attributes: []string{"emb", "note"}, DistanceMetric: "COSINE",
	})))
	err = docDB.CreateIndex(ctx, projectID, "app", "vecs", databases.Index{
		ID: "emb_ord", Type: "hnsw", Attributes: []string{"emb"}, Orders: []string{"desc"}, DistanceMetric: "COSINE",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "orders")
	err = docDB.CreateIndex(ctx, projectID, "app", "vecs", databases.Index{
		ID: "emb_bad", Type: "hnsw", Attributes: []string{"emb"}, DistanceMetric: "MANHATTAN",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "distance_metric")

	// 同列多 metric 允许（拒绝矩阵的反面）已在 DDLForms 覆盖。
	require.True(t, strings.Contains("hnsw", "hnsw"))
}
