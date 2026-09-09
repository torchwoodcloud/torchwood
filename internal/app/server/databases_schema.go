package server

import (
	"context"
	"sort"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// JSON Schema 2020-12 导出（B10，redesign §4.1 Agent 面 / §10.1）：从 catalog
// 契约（attrs 单一事实源）生成集合的机器可读 schema。类型映射与物理列类型
//（postgres_collection_ddl.go pgTypeFor）同域：string/email/url/datetime/
// integer/float/boolean/json/vector。Agent 用途：载荷合成前的约束发现、
// 客户端校验器生成——物理表名不出现在文档任何位置。

// jsonSchemaDialect 是导出文档锁定 的 JSON Schema 方言版本。
const jsonSchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// ExportCollectionSchema 拉取集合契约并生成 JSON Schema 2020-12 文档。
// 读语义守卫与 GetCollection 同链（不做文档 ACL 判定——导出的是 catalog
// 契约而非数据行）；集合不存在 → NotFound。
func (d *Databases) ExportCollectionSchema(ctx context.Context, projectID, databaseID, collectionID string) (map[string]any, error) {
	if err := shared.RejectExternalDatabaseID(databaseID); err != nil {
		return nil, err
	}
	if _, err := d.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	col, err := d.docDB.GetCollection(ctx, projectID, databaseID, collectionID)
	if err != nil {
		return nil, shared.MapDocumentDBError(err)
	}
	if col == nil {
		return nil, status.Error(codes.NotFound, "collection not found")
	}
	return JSONSchemaOfCollection(col), nil
}

// JSONSchemaOfCollection 把集合契约翻译为 JSON Schema 2020-12 文档（纯函数）：
//   - 用户属性按类型映射矩阵生成（attributeJSONSchema）；required=true 的属性
//     进顶层 required（键序稳定）；default_value 非空 → default；
//     Options["deprecated"] 真值 → "deprecated": true（catalog 无独立 deprecated
//     列，经 options 通道承载；现阶段 DDL 不产生该标记，关键字保留给 D3 契约）；
//   - _id/_created_at/_updated_at/_created_by/_updated_by/_version/_acl 系统
//     字段以 readOnly 属性注释（_id 可在 create 时指定，不标 readOnly）；
//   - array=true 的标量属性 → type=array + items=元素 schema（vector 与 array
//     互斥，adapter 建列时已拒绝）。
func JSONSchemaOfCollection(col *databases.Collection) map[string]any {
	props := map[string]any{}
	for _, a := range col.Attributes {
		props[a.Key] = attributeJSONSchema(a)
	}
	for k, v := range systemFieldSchemas() {
		props[k] = v
	}

	doc := map[string]any{
		"$schema":    jsonSchemaDialect,
		"$id":        "urn:torchwood:project:" + col.ProjectID + ":database:" + col.DatabaseID + ":collection:" + col.ID,
		"title":      col.Name,
		"type":       "object",
		"properties": props,
		// 契约出处与边界说明（Agent 可读注释通道；物理名永不出现）。
		"$comment": "Generated from the collection catalog contract (declared attributes). " +
			"Undeclared keys are rejected on write; system fields are managed by the server.",
	}
	var required []any
	for _, a := range col.Attributes {
		// B4 生命周期：deprecated 属性读屏蔽写拒收——不进 required（其数据
		// 在读回中不可见）；migrating 属性照常（读服务旧列）。
		if a.StatusOrDefault() == databases.AttrStatusDeprecated {
			continue
		}
		if a.Required {
			required = append(required, a.Key)
		}
	}
	if len(required) > 0 {
		sort.Slice(required, func(i, j int) bool { return required[i].(string) < required[j].(string) })
		doc["required"] = required
	}
	return doc
}

// attributeJSONSchema 是单属性的类型映射矩阵（B10 判据）：
//
//	string        → {"type":"string"}
//	email         → {"type":"string","format":"email"}
//	url           → {"type":"string","format":"uri"}
//	datetime      → {"type":"string","format":"date-time"}
//	integer       → {"type":"integer"}
//	float         → {"type":"number"}
//	boolean       → {"type":"boolean"}
//	json          → {"type":"object"}（物理 JSONB，任意 JSON 值）
//	vector(dims)  → {"type":"array","items":{"type":"number"},"minItems":dims,"maxItems":dims}
//	其余/未知     → {"type":"string"}（与物理缺省 TEXT 对齐）
//
// array=true（标量元素）→ 外层 {"type":"array","items":<元素 schema>}。
func attributeJSONSchema(a databases.Attribute) map[string]any {
	base := map[string]any{}
	switch strings.ToLower(a.Type) {
	case "string":
		base["type"] = "string"
	case "email":
		base["type"] = "string"
		base["format"] = "email"
	case "url":
		base["type"] = "string"
		base["format"] = "uri"
	case "datetime":
		base["type"] = "string"
		base["format"] = "date-time"
	case "integer":
		base["type"] = "integer"
	case "float":
		base["type"] = "number"
	case "boolean":
		base["type"] = "boolean"
	case "json":
		base["type"] = "object"
	case "vector":
		base["type"] = "array"
		base["items"] = map[string]any{"type": "number"}
		// dims 作定长注释：向量维度固定，min=max（2..2000，建列时已校验）。
		base["minItems"] = a.Dims
		base["maxItems"] = a.Dims
	default:
		base["type"] = "string"
	}

	var schema map[string]any
	if a.Array && strings.ToLower(a.Type) != "vector" {
		schema = map[string]any{"type": "array", "items": base}
	} else {
		schema = base
	}
	if a.Default != nil {
		schema["default"] = a.Default
	}
	// B4 生命周期：deprecated 属性以 JSON Schema deprecated 关键字标注（读
	// 屏蔽写拒收的契约信号；此前该关键字仅保留给 Options 通道）。
	if a.StatusOrDefault() == databases.AttrStatusDeprecated ||
		a.StatusOrDefault() == databases.AttrStatusMigrating {
		schema["deprecated"] = true
	}
	if v, ok := a.Options["deprecated"]; ok {
		if b, ok := v.(bool); ok && b {
			schema["deprecated"] = true
		}
	}
	return schema
}

// systemFieldSchemas 是文档系统字段的注释形态（_acl/_version 等不进 catalog
// attrs，但属于文档形状——以 readOnly 标注服务端管理，供 Agent 区分载荷与
// 回读字段；键集与 ReservedAttributeKeys 对齐）。
func systemFieldSchemas() map[string]any {
	ro := func(desc string) map[string]any {
		return map[string]any{"readOnly": true, "description": desc}
	}
	createdAt := func(desc string) map[string]any {
		return map[string]any{"type": "string", "format": "date-time", "readOnly": true, "description": desc}
	}
	return map[string]any{
		"_id":         map[string]any{"type": "string", "description": "document id; settable on create, immutable afterwards"},
		"_created_at": createdAt("creation timestamp (RFC 3339)"),
		"_updated_at": createdAt("last successful write timestamp (RFC 3339)"),
		"_created_by": ro("actor that created the document"),
		"_updated_by": ro("actor of the last successful write"),
		"_version": map[string]any{
			"type":        "integer",
			"readOnly":    true,
			"description": "optimistic concurrency (OCC) version; incremented on every successful write; update/delete must carry the value last read",
		},
		"_acl": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"readOnly":    true,
			"description": "document-level access control entries (type:role, e.g. user:<id>); managed via the server ACL channel",
		},
	}
}
