// vector 列端到端（会话 #10 §10.5 P0 最后一项，包 A）：vector 属性落地
// pgvector 原生 VECTOR(dims) 列，读写往返（浮点精度边界）、dims 校验、
// default/array 拒绝、读回 JSON 数组契约、维度不匹配拒绝、CreateAttribute
// 加列后立即可写（列级 GRANT 滞后修复）。
package documentdb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func setupVectorCollection(ctx context.Context, t *testing.T) (databases.DocumentDB, string) {
	t.Helper()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })
	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	t.Cleanup(cleanup)
	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "docs", "Docs", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256},
		{ID: "emb", Key: "emb", Type: "vector", Dims: 3},
	}, nil, nil, true))
	return docDB, projectID
}

// TestVectorColumns_DDLAndRoundtrip：物理列形态（udt_name=vector）与读写往返
// （浮点精度边界：小数、负数、科学计数形态的短浮点）。
func TestVectorColumns_DDLAndRoundtrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupVectorCollection(ctx, t)

	// 物理列类型断言：pgvector 列的 udt_name 为 "vector"。
	db := docDB.(*postgresDocumentDB)
	var udt string
	err := db.conn(ctx).QueryRowContext(ctx, `
		SELECT udt_name FROM information_schema.columns
		WHERE table_schema = 'tw_`+projectID+`_app' AND table_name = (
			SELECT physical_name FROM catalog_collections
			WHERE project_id = ? AND database_id = 'app' AND collection_id = 'docs')
			AND column_name = 'emb'`, projectID).Scan(&udt)
	require.NoError(t, err)
	require.Equal(t, "vector", udt)

	created, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{
			"title": "v1",
			// 0.1/0.2/0.3 是浮点精度的经典边界；-2.5 负值；1e-8 极小值。
			"emb": []any{0.1, -2.5, 1e-8},
		},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	got, err := docDB.GetDocument(ctx, projectID, "app", "docs", created.ID, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	// 读回契约 = JSON 数组（原型 3：投影覆盖 ::text::jsonb）。
	emb, ok := got.Data["emb"].([]any)
	require.True(t, ok, "vector read-back must be a JSON array, got %T", got.Data["emb"])
	require.Len(t, emb, 3)
	require.Equal(t, 0.1, emb[0])
	require.Equal(t, -2.5, emb[1])
	require.Equal(t, 1e-8, emb[2])

	// data 通道整列替换（字面量 + ::vector cast）。
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"emb": []any{1.0, 2.0, 3.0}}},
		ExpectedVersion: got.Version,
	}, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{1.0, 2.0, 3.0}, updated.Data["emb"])
}

// TestVectorColumns_AttributeValidation：dims 域校验（2..2000）、default 拒绝、
// array 拒绝、非 vector 设 dims 拒绝——app 层与 adapter 第二道防线同语义。
func TestVectorColumns_AttributeValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupVectorCollection(ctx, t)

	for _, attr := range []databases.Attribute{
		{ID: "v0", Key: "v0", Type: "vector"},                        // dims 缺失
		{ID: "v1", Key: "v1", Type: "vector", Dims: 1},               // 下界外
		{ID: "v2", Key: "v2", Type: "vector", Dims: 2001},            // 上界外
		{ID: "vd", Key: "vd", Type: "vector", Dims: 3, Default: "x"}, // default 拒绝
		{ID: "va", Key: "va", Type: "vector", Dims: 3, Array: true},  // array 拒绝
		{ID: "sd", Key: "sd", Type: "string", Dims: 3},               // 非 vector 设 dims
	} {
		err := docDB.CreateAttribute(ctx, projectID, "app", "docs", attr)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "attr=%s", attr.Key)
	}
	// 合法域端点：2 与 2000 可建（2000 维值不入库，仅验 DDL 成功）。
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
		ID: "emb2", Key: "emb2", Type: "vector", Dims: 2,
	}))
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
		ID: "emb2000", Key: "emb2000", Type: "vector", Dims: 2000,
	}))
	// catalog 读回 dims 契约完整。
	coll, err := docDB.GetCollection(ctx, projectID, "app", "docs")
	require.NoError(t, err)
	dims := map[string]int{}
	for _, a := range coll.Attributes {
		if a.Type == "vector" {
			dims[a.Key] = a.Dims
		}
	}
	require.Equal(t, map[string]int{"emb": 3, "emb2": 2, "emb2000": 2000}, dims)
}

// TestVectorColumns_ValueValidation：写入值维度不匹配/非数值数组拒绝
// （create 与 update 两通道）。
func TestVectorColumns_ValueValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupVectorCollection(ctx, t)

	_, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"emb": []any{1.0, 2.0}}, // 2 维 ≠ 3
	}, anyPerms(), databases.SystemPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "expected 3")

	_, err = docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"emb": "[1,2,3]"}, // 字符串不是 JSON 数组
	}, anyPerms(), databases.SystemPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "must be a JSON array")

	_, err = docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"emb": []any{1.0, "x", 3.0}}, // 混入非数值
	}, anyPerms(), databases.SystemPrincipal)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	created, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		Data: map[string]any{"emb": []any{1.0, 2.0, 3.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	_, err = docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: created.ID, Data: map[string]any{"emb": []any{9.0}}},
		ExpectedVersion: created.Version,
	}, databases.Principal{Roles: []string{"any"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "expected 3")
}

// TestVectorColumns_CreateAttributeGrantReady：CreateAttribute 新加 vector 列后
// 立即以应用身份（tw_app，列级授权）写入成功——列级 GRANT 滞后修复的回归锁。
func TestVectorColumns_CreateAttributeGrantReady(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupVectorCollection(ctx, t)

	created, err := docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "g1",
		Data: map[string]any{"title": "grant"},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)

	// 加列后立即 update（tw_app 路径）：新列的 INSERT/UPDATE 列授权必须就绪。
	require.NoError(t, docDB.CreateAttribute(ctx, projectID, "app", "docs", databases.Attribute{
		ID: "emb_late", Key: "emb_late", Type: "vector", Dims: 4,
	}))
	updated, err := docDB.UpdateDocument(ctx, projectID, "app", "docs", databases.DocumentUpdate{
		Document:        databases.Document{ID: "g1", Data: map[string]any{"emb_late": []any{0.5, 0.5, 0.5, 0.5}}},
		ExpectedVersion: created.Version,
	}, databases.Principal{Roles: []string{"any"}})
	require.NoError(t, err)
	require.Equal(t, []any{0.5, 0.5, 0.5, 0.5}, updated.Data["emb_late"])

	// 新列上再 Create（INSERT 列授权同样就绪）。
	_, err = docDB.CreateDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "g2",
		Data: map[string]any{"emb_late": []any{1.0, 0.0, 0.0, 0.0}},
	}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
}

// TestVectorColumns_UpsertUpdateBranch：upsert 更新支的 vector 写路径
// （buildUpdateParts 同一编码）。
func TestVectorColumns_UpsertUpdateBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	docDB, projectID := setupVectorCollection(ctx, t)

	require.NoError(t, docDB.CreateIndex(ctx, projectID, "app", "docs", databases.Index{
		ID: "title_uniq", Type: "unique", Attributes: []string{"title"},
	}))
	upserted, err := docDB.UpsertDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "u1",
		Data: map[string]any{"title": "up", "emb": []any{1.0, 1.0, 1.0}},
	}, []string{"title"}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, []any{1.0, 1.0, 1.0}, upserted.Data["emb"])

	// 冲突命中走更新支：vector 整列替换。
	upserted, err = docDB.UpsertDocument(ctx, projectID, "app", "docs", databases.Document{
		ID:   "u1",
		Data: map[string]any{"title": "up", "emb": []any{0.0, 0.0, 2.0}},
	}, []string{"title"}, anyPerms(), databases.SystemPrincipal)
	require.NoError(t, err)
	require.Equal(t, []any{0.0, 0.0, 2.0}, upserted.Data["emb"])
}
