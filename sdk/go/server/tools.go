package server

import (
	"context"
	"encoding/json"
	"fmt"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Agent 默认工具名（E-7 overlay）。完整产品 API 仍是 185 个 RPC（Client 61 + Server 114 + Console 10）。
const (
	ToolListUsers       = "list_users"
	ToolGetUser         = "get_user"
	ToolCreateUser      = "create_user"
	ToolQueryDocuments  = "query_documents"
	ToolGetDocument     = "get_document"
	ToolCreateDocument  = "create_document"
	ToolUpdateDocument  = "update_document"
	ToolUpsertDocument  = "upsert_document"
	ToolDeleteDocument  = "delete_document"
	ToolListCollections = "list_collections"
	ToolGetCollection   = "get_collection"
	ToolInvokeFunction  = "invoke_function"
	ToolListFiles       = "list_files"
	ToolGetFile         = "get_file"
	ToolGrantAsset      = "grant_asset"
	ToolListUserAssets  = "list_user_assets"
	ToolGetOrder        = "get_order"
	ToolGetHealth       = "get_health"
)

// Tool 将 Agent 工具名映射到已有 Server unary（不新增 gRPC 服务）。
// 不含 API key 管理；InvokeJSON 仍排除 APIKeysService。
// P3-16：InputSchema 由 descriptor 生成的 JSON Schema（type: object），供 Agent/自动化直接驱动。
type Tool struct {
	Name        string
	FullMethod  string
	InputNotes  string
	InputSchema string `json:"input_schema"`
}

// Tools 是锁定的 18 个默认工具，顺序与 docs/review/wave3-e7-tool-catalog.md 一致。
// catalog 只读：不要原地改写切片或元素；LookupTool 使用 init 时的值拷贝。
var Tools = []Tool{
	{
		Name:       ToolListUsers,
		FullMethod: serverv1.UsersService_ListUsers_FullMethodName,
		InputNotes: "shared.v1.ListRequest：page_size / page_token / queries[]string",
	},
	{
		Name:       ToolGetUser,
		FullMethod: serverv1.UsersService_GetUser_FullMethodName,
		InputNotes: "id",
	},
	{
		Name:       ToolCreateUser,
		FullMethod: serverv1.UsersService_CreateUser_FullMethodName,
		InputNotes: "email, password；可选 name / status / labels / prefs",
	},
	{
		Name:       ToolQueryDocuments,
		FullMethod: serverv1.DatabasesService_ListDocuments_FullMethodName,
		InputNotes: "必填 database_id、collection_id。优先 query（shared.v1.Query AST：filter/orders/page_size/page_token）；仍接受 queries[]string + page_size/page_token。两者同时提供且冲突 → InvalidArgument",
	},
	{
		Name:       ToolGetDocument,
		FullMethod: serverv1.DatabasesService_GetDocument_FullMethodName,
		InputNotes: "database_id, collection_id, document_id",
	},
	{
		Name:       ToolCreateDocument,
		FullMethod: serverv1.DatabasesService_CreateDocument_FullMethodName,
		InputNotes: "database_id, collection_id, document_id, data；可选 permissions",
	},
	{
		Name:       ToolUpdateDocument,
		FullMethod: serverv1.DatabasesService_UpdateDocument_FullMethodName,
		InputNotes: "database_id, collection_id, document_id；可选 data / permissions / increment。用户集合须带 version（OCC）",
	},
	{
		Name:       ToolUpsertDocument,
		FullMethod: serverv1.DatabasesService_UpsertDocument_FullMethodName,
		InputNotes: "database_id, collection_id, document_id, data；可选 permissions、conflict_columns",
	},
	{
		Name:       ToolDeleteDocument,
		FullMethod: serverv1.DatabasesService_DeleteDocument_FullMethodName,
		InputNotes: "database_id, collection_id, document_id。用户集合须带 version（OCC）",
	},
	{
		Name:       ToolListCollections,
		FullMethod: serverv1.DatabasesService_ListCollections_FullMethodName,
		InputNotes: "database_id；可选 queries[]string、page_size、page_token",
	},
	{
		Name:       ToolGetCollection,
		FullMethod: serverv1.DatabasesService_GetCollection_FullMethodName,
		InputNotes: "database_id, collection_id",
	},
	{
		Name:       ToolInvokeFunction,
		FullMethod: serverv1.FunctionsService_CreateExecution_FullMethodName,
		InputNotes: "function_id；可选 deployment_id、data、async",
	},
	{
		Name:       ToolListFiles,
		FullMethod: serverv1.StorageService_ListFiles_FullMethodName,
		InputNotes: "bucket_id；可选 queries[]string、page_size、page_token",
	},
	{
		Name:       ToolGetFile,
		FullMethod: serverv1.StorageService_GetFile_FullMethodName,
		InputNotes: "bucket_id, file_id",
	},
	{
		Name:       ToolGrantAsset,
		FullMethod: serverv1.AssetsService_Grant_FullMethodName,
		InputNotes: "owner_id, def_code, quantity, idempotency_key；可选 expires_at / level / metadata / ref_type / ref_id",
	},
	{
		Name:       ToolListUserAssets,
		FullMethod: serverv1.AssetsService_ListUserAssets_FullMethodName,
		InputNotes: "owner_id；可选 page_size、page_token",
	},
	{
		Name:       ToolGetOrder,
		FullMethod: serverv1.PaymentsService_GetOrder_FullMethodName,
		InputNotes: "order_id",
	},
	{
		Name:       ToolGetHealth,
		FullMethod: serverv1.HealthService_Check_FullMethodName,
		InputNotes: "无入参",
	},
}

var toolsByName map[string]Tool

func init() {
	// P3-16：为每个 Tool 生成 InputSchema（descriptor → JSON Schema）
	for i := range Tools {
		Tools[i].InputSchema = buildInputSchema(Tools[i].FullMethod)
	}
	toolsByName = make(map[string]Tool, len(Tools))
	for _, t := range Tools {
		toolsByName[t.Name] = t
	}
}

// buildInputSchema 由 FullMethod 的 input descriptor 生成简易 JSON Schema（type: object）。
func buildInputSchema(fullMethod string) string {
	md, err := findServerMethod(fullMethod)
	if err != nil {
		return `{"type":"object"}`
	}
	msg := md.Input()
	schema := map[string]any{
		"type":       "object",
		"properties": buildProperties(msg),
	}
	b, _ := json.Marshal(schema)
	return string(b)
}

func buildProperties(md protoreflect.MessageDescriptor) map[string]any {
	props := make(map[string]any, md.Fields().Len())
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		jsonName := fd.JSONName()
		prop := map[string]any{}
		// 类型映射
		switch fd.Kind() {
		case protoreflect.StringKind:
			prop["type"] = "string"
		case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
			protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
			protoreflect.Uint32Kind, protoreflect.Fixed32Kind, protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
			prop["type"] = "integer"
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			prop["type"] = "number"
		case protoreflect.BoolKind:
			prop["type"] = "boolean"
		case protoreflect.BytesKind:
			prop["type"] = "string"
			prop["format"] = "byte"
		case protoreflect.EnumKind:
			prop["type"] = "string"
		case protoreflect.MessageKind:
			// google.protobuf.Struct / Timestamp 等按 object/string 处理
			if fd.Message().FullName() == "google.protobuf.Struct" {
				prop["type"] = "object"
			} else if fd.Message().FullName() == "google.protobuf.Timestamp" {
				prop["type"] = "string"
				prop["format"] = "date-time"
			} else {
				prop["type"] = "object"
				// 嵌套对象展开一层（避免无限递归）
				if fd.Message().Fields().Len() > 0 && fd.Message().Fields().Len() <= 10 {
					prop["properties"] = buildProperties(fd.Message())
				}
			}
		default:
			prop["type"] = "string"
		}
		if fd.IsList() {
			prop = map[string]any{"type": "array", "items": prop}
		}
		// 兼容旧 SDK：字段描述来自 proto 注释，暂不填充 description
		props[jsonName] = prop
	}
	return props
}

// LookupTool 按工具名查找 catalog 条目。
func LookupTool(name string) (Tool, bool) {
	t, ok := toolsByName[name]
	return t, ok
}

// InvokeTool 按 catalog 工具名调用对应 Server unary（走 InvokeJSON）。
func (c *Client) InvokeTool(ctx context.Context, name string, reqJSON []byte) ([]byte, error) {
	t, ok := LookupTool(name)
	if !ok {
		return nil, fmt.Errorf("torchwood: unknown tool %q", name)
	}
	return c.InvokeJSON(ctx, t.FullMethod, reqJSON)
}
