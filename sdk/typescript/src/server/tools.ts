/** Agent 默认工具（E-7 overlay）。完整产品 API 仍是 237 个 RPC（Client 68 + Server 146 + Console 23）；不含 API key 管理。 */

export const TOOL_LIST_USERS = "list_users";
export const TOOL_GET_USER = "get_user";
export const TOOL_CREATE_USER = "create_user";
export const TOOL_QUERY_DOCUMENTS = "query_documents";
export const TOOL_GET_DOCUMENT = "get_document";
export const TOOL_CREATE_DOCUMENT = "create_document";
export const TOOL_UPDATE_DOCUMENT = "update_document";
export const TOOL_UPSERT_DOCUMENT = "upsert_document";
export const TOOL_DELETE_DOCUMENT = "delete_document";
export const TOOL_LIST_COLLECTIONS = "list_collections";
export const TOOL_GET_COLLECTION = "get_collection";
export const TOOL_INVOKE_FUNCTION = "invoke_function";
export const TOOL_LIST_FILES = "list_files";
export const TOOL_GET_FILE = "get_file";
export const TOOL_GRANT_ASSET = "grant_asset";
export const TOOL_LIST_USER_ASSETS = "list_user_assets";
export const TOOL_GET_ORDER = "get_order";
export const TOOL_GET_HEALTH = "get_health";

export interface AgentTool {
  readonly name: string;
  /** gRPC FullMethod，与 Go SDK InvokeJSON 路径一致。 */
  readonly fullMethod: string;
  readonly inputNotes: string;
}

function freezeTool(tool: AgentTool): Readonly<AgentTool> {
  return Object.freeze(tool);
}

/** 只读 catalog；元素已 freeze。TOOL_* 与 Go `ToolListUsers` 等常量同名同值。 */
export const agentTools: readonly Readonly<AgentTool>[] = Object.freeze([
  freezeTool({
    name: TOOL_LIST_USERS,
    fullMethod: "/torchwood.server.v1.UsersService/ListUsers",
    inputNotes: "shared.v1.ListRequest：page_size / page_token / queries[]string",
  }),
  freezeTool({
    name: TOOL_GET_USER,
    fullMethod: "/torchwood.server.v1.UsersService/GetUser",
    inputNotes: "id",
  }),
  freezeTool({
    name: TOOL_CREATE_USER,
    fullMethod: "/torchwood.server.v1.UsersService/CreateUser",
    inputNotes: "email, password；可选 name / status / labels / prefs",
  }),
  freezeTool({
    name: TOOL_QUERY_DOCUMENTS,
    fullMethod: "/torchwood.server.v1.DatabasesService/ListDocuments",
    inputNotes:
      "必填 database_id、collection_id。query 为 shared.v1.Query AST（filter/orders/select/page_size/page_token，单 AST——DSL 字符串已退役）；page_size/page_token 与 query 内同名字段冲突即拒",
  }),
  freezeTool({
    name: TOOL_GET_DOCUMENT,
    fullMethod: "/torchwood.server.v1.DatabasesService/GetDocument",
    inputNotes: "database_id, collection_id, document_id",
  }),
  freezeTool({
    name: TOOL_CREATE_DOCUMENT,
    fullMethod: "/torchwood.server.v1.DatabasesService/CreateDocument",
    inputNotes: "database_id, collection_id, document_id, data；可选 permissions",
  }),
  freezeTool({
    name: TOOL_UPDATE_DOCUMENT,
    fullMethod: "/torchwood.server.v1.DatabasesService/UpdateDocument",
    inputNotes:
      "database_id, collection_id, document_id；可选 data / permissions / increment。用户集合须带 version（OCC）",
  }),
  freezeTool({
    name: TOOL_UPSERT_DOCUMENT,
    fullMethod: "/torchwood.server.v1.DatabasesService/UpsertDocument",
    inputNotes:
      "database_id, collection_id, document_id, data；可选 permissions、conflict_columns",
  }),
  freezeTool({
    name: TOOL_DELETE_DOCUMENT,
    fullMethod: "/torchwood.server.v1.DatabasesService/DeleteDocument",
    inputNotes: "database_id, collection_id, document_id。用户集合须带 version（OCC）",
  }),
  freezeTool({
    name: TOOL_LIST_COLLECTIONS,
    fullMethod: "/torchwood.server.v1.DatabasesService/ListCollections",
    inputNotes: "database_id；可选 queries[]string、page_size、page_token",
  }),
  freezeTool({
    name: TOOL_GET_COLLECTION,
    fullMethod: "/torchwood.server.v1.DatabasesService/GetCollection",
    inputNotes: "database_id, collection_id",
  }),
  freezeTool({
    name: TOOL_INVOKE_FUNCTION,
    fullMethod: "/torchwood.server.v1.FunctionsService/CreateExecution",
    inputNotes: "function_id；可选 deployment_id、data、async",
  }),
  freezeTool({
    name: TOOL_LIST_FILES,
    fullMethod: "/torchwood.server.v1.StorageService/ListFiles",
    inputNotes: "bucket_id；可选 queries[]string、page_size、page_token",
  }),
  freezeTool({
    name: TOOL_GET_FILE,
    fullMethod: "/torchwood.server.v1.StorageService/GetFile",
    inputNotes: "bucket_id, file_id",
  }),
  freezeTool({
    name: TOOL_GRANT_ASSET,
    fullMethod: "/torchwood.server.v1.AssetsService/Grant",
    inputNotes:
      "owner_id, def_code, quantity, idempotency_key；可选 expires_at / level / metadata / ref_type / ref_id",
  }),
  freezeTool({
    name: TOOL_LIST_USER_ASSETS,
    fullMethod: "/torchwood.server.v1.AssetsService/ListUserAssets",
    inputNotes: "owner_id；可选 page_size、page_token",
  }),
  freezeTool({
    name: TOOL_GET_ORDER,
    fullMethod: "/torchwood.server.v1.PaymentsService/GetOrder",
    inputNotes: "order_id",
  }),
  freezeTool({
    name: TOOL_GET_HEALTH,
    fullMethod: "/torchwood.server.v1.HealthService/Check",
    inputNotes: "无入参",
  }),
]);

const toolsByName = new Map(agentTools.map((t) => [t.name, t]));

export function lookupAgentTool(name: string): Readonly<AgentTool> | undefined {
  return toolsByName.get(name);
}
