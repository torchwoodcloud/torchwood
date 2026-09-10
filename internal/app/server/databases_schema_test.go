package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAttributeJSONSchema_TypeMappingMatrix（B10 判据：类型映射矩阵）：
// catalog attr 类型 → JSON Schema 2020-12 的完整映射，与物理列类型
// （pgTypeFor）同域；array=true 外层包装 items；default/deprecated 透出。
func TestAttributeJSONSchema_TypeMappingMatrix(t *testing.T) {
	cases := []struct {
		name string
		attr databases.Attribute
		want map[string]any
	}{
		{"string", databases.Attribute{Type: "string"}, map[string]any{"type": "string"}},
		{"email", databases.Attribute{Type: "email"}, map[string]any{"type": "string", "format": "email"}},
		{"url", databases.Attribute{Type: "url"}, map[string]any{"type": "string", "format": "uri"}},
		{"datetime", databases.Attribute{Type: "datetime"}, map[string]any{"type": "string", "format": "date-time"}},
		{"integer", databases.Attribute{Type: "integer"}, map[string]any{"type": "integer"}},
		{"float", databases.Attribute{Type: "float"}, map[string]any{"type": "number"}},
		{"boolean", databases.Attribute{Type: "boolean"}, map[string]any{"type": "boolean"}},
		{"json", databases.Attribute{Type: "json"}, map[string]any{"type": "object"}},
		// vector：数组形态 + dims 定长注释（min=max）。
		{"vector", databases.Attribute{Type: "vector", Dims: 768}, map[string]any{
			"type": "array", "items": map[string]any{"type": "number"}, "minItems": 768, "maxItems": 768,
		}},
		// array=true 标量元素 → 外层 array + items（vector 与 array 互斥，
		// adapter 建列已拒，此处仅验证标量包装）。
		{"array<string>", databases.Attribute{Type: "string", Array: true}, map[string]any{
			"type": "array", "items": map[string]any{"type": "string"},
		}},
		{"array<integer>", databases.Attribute{Type: "integer", Array: true}, map[string]any{
			"type": "array", "items": map[string]any{"type": "integer"},
		}},
		// default_value 非空 → default 透出（类型与 catalog 反序列化形态一致）。
		{"default", databases.Attribute{Type: "integer", Default: 42}, map[string]any{
			"type": "integer", "default": 42,
		}},
		// Options.deprecated → JSON Schema deprecated 关键字（catalog 无独立
		// deprecated 列，经 options 通道承载；D3 契约定稿前的保留通道）。
		{"deprecated", databases.Attribute{Type: "string", Options: map[string]any{"deprecated": true}}, map[string]any{
			"type": "string", "deprecated": true,
		}},
		{"deprecated-false", databases.Attribute{Type: "string", Options: map[string]any{"deprecated": false}}, map[string]any{
			"type": "string",
		}},
		// 未知类型回退 string（与物理缺省 TEXT 对齐；app 校验层本就拒绝）。
		{"unknown", databases.Attribute{Type: "banana"}, map[string]any{"type": "string"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, attributeJSONSchema(tc.attr))
		})
	}
}

// TestJSONSchemaOfCollection_DocumentShape：完整文档形态——$schema 方言、
// $id 标识、required 键序稳定、系统字段 readOnly 注释、required 缺省省略。
func TestJSONSchemaOfCollection_DocumentShape(t *testing.T) {
	col := &databases.Collection{
		ID:         "posts",
		DatabaseID: "app",
		ProjectID:  "prj_1",
		Name:       "Posts",
		Attributes: []databases.Attribute{
			{ID: "title", Key: "title", Type: "string", Size: 256, Required: true},
			{ID: "views", Key: "views", Type: "integer", Required: true},
			{ID: "rating", Key: "rating", Type: "float"},
			{ID: "published", Key: "published", Type: "boolean"},
			{ID: "meta", Key: "meta", Type: "json"},
			{ID: "author_email", Key: "author_email", Type: "email"},
			{ID: "link", Key: "link", Type: "url"},
			{ID: "created_day", Key: "created_day", Type: "datetime"},
			{ID: "embedding", Key: "embedding", Type: "vector", Dims: 3},
			{ID: "tags", Key: "tags", Type: "string", Array: true},
		},
	}
	doc := JSONSchemaOfCollection(col)

	require.Equal(t, "https://json-schema.org/draft/2020-12/schema", doc["$schema"])
	require.Equal(t, "urn:torchwood:project:prj_1:database:app:collection:posts", doc["$id"])
	require.Equal(t, "Posts", doc["title"])
	require.Equal(t, "object", doc["type"])

	props := doc["properties"].(map[string]any)
	for _, key := range []string{"title", "views", "rating", "published", "meta",
		"author_email", "link", "created_day", "embedding", "tags"} {
		require.Contains(t, props, key)
	}
	// required 键序稳定（字典序），与属性声明序无关。
	require.Equal(t, []any{"title", "views"}, doc["required"])

	// 系统字段注释：readOnly + 描述；_id 可由客户端在 create 指定（不标 readOnly）。
	sys := systemFieldSchemas()
	for _, key := range []string{"_id", "_created_at", "_updated_at", "_created_by", "_updated_by", "_version", "_acl"} {
		require.Contains(t, props, key)
		require.NotEmpty(t, sys[key].(map[string]any)["description"], key)
	}
	for _, key := range []string{"_created_at", "_updated_at", "_created_by", "_updated_by", "_version", "_acl"} {
		require.Equal(t, true, props[key].(map[string]any)["readOnly"], key)
	}
	require.Nil(t, props["_id"].(map[string]any)["readOnly"])
	require.Equal(t, "integer", props["_version"].(map[string]any)["type"])

	// 无 required 属性时顶层 required 省略（非空数组才出现）。
	noRequired := &databases.Collection{ID: "d", DatabaseID: "app", ProjectID: "p", Name: "D",
		Attributes: []databases.Attribute{{ID: "a", Key: "a", Type: "string"}}}
	doc2 := JSONSchemaOfCollection(noRequired)
	require.NotContains(t, doc2, "required")

	// 物理名永不出现：文档不含 physical name 字段（契约导出仅逻辑契约）。
	require.NotContains(t, doc, "physical_name")
}

// TestDatabases_ExportCollectionSchema（B10 集成）：真实 catalog 往返——建
// 集合（多类型 attrs）→ ExportCollectionSchema 读回契约 → 文档形态与类型
// 映射成立；集合缺失 → NotFound；sentinel `_` 库 → 拒绝。
func TestDatabases_ExportCollectionSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := platformAdminCtx(context.Background())
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	uc := NewDatabases(bunrepo.NewProjectRepository(db), documentdb.NewPostgresDocumentDB(db, nil), nil)

	require.NoError(t, uc.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, uc.CreateCollection(ctx, projectID, "app", "posts", "Posts", []databases.Attribute{
		{ID: "title", Key: "title", Type: "string", Size: 256, Required: true},
		{ID: "views", Key: "views", Type: "integer"},
		{ID: "embedding", Key: "embedding", Type: "vector", Dims: 4},
	}, nil, nil, true))

	doc, err := uc.ExportCollectionSchema(ctx, projectID, "app", "posts")
	require.NoError(t, err)
	require.Equal(t, "https://json-schema.org/draft/2020-12/schema", doc["$schema"])
	props := doc["properties"].(map[string]any)
	require.Equal(t, "string", props["title"].(map[string]any)["type"])
	require.Equal(t, "integer", props["views"].(map[string]any)["type"])
	require.Equal(t, 4, props["embedding"].(map[string]any)["maxItems"])
	require.Equal(t, []any{"title"}, doc["required"])

	// 集合不存在 → NotFound。
	_, err = uc.ExportCollectionSchema(ctx, projectID, "app", "missing")
	require.Equal(t, codes.NotFound, status.Code(err))

	// sentinel `_` 库（内部寻址专用）对外拒绝。
	_, err = uc.ExportCollectionSchema(ctx, projectID, "_", "users")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
