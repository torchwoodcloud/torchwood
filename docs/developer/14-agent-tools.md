# 14 Agent 默认工具箱

Overlay，不是新 API。完整产品面仍是全部 RPC（当前 **247 个**：Client 68 + Server 155 + Console 24），Agent 默认仅暴露 **18 个动词**。

> 计数权威：`docs/developer/authz-matrix.md` 头部（生成物，`mise run gen:authz-matrix` 渲染）。
> 动词映射：`sdk/go/server/tools.go`（`Tools`）与 `sdk/typescript/src/server/tools.ts`（`agentTools`）。
> OpenAPI 以 `genproto/**/*.swagger.json` 为权威。

## 1. 定位

- **不是**新增一个 API 面，也**不是**把产品面砍到 20 个动词——Agent / 自动化默认只看见 18 动词表，Console、CLI、SDK 仍走完整 Server API。
- 逃生舱是 `InvokeJSON(fullMethod, protojson)`（`sdk/go/server/invoke.go`）：覆盖全部 `torchwood.server.v1.*` unary，继续排除 `APIKeysService`。新增 RPC 自动可用，无需改动工具箱。
- 本 catalog **不含** API key 的 create / list / get / delete——密钥只在 Console 或带合适 scope 的管理流程里创建，不交给普通 Agent 工具面。
- 全量计数口径：`proto/client` + `proto/server` + `proto/console` 的全部 `rpc` 条目，分布见 `authz-matrix.md` 头部（当前：PUBLIC 28 · END_USER 47 · SERVER 142 · PERMISSION 30 · SYSTEM 0）。
- 新增 RPC 后，overlay 是否收录为默认动词由产品决策（截至本文，leaderboards / analytics / runbooks 均未收录）。

## 2. 默认 18 个工具

顺序与 `sdk/go/server/tools.go` 的 `Tools` 一致，catalog 只读（Go `toolsByName`、TS `Object.freeze` + `Map`）：

| 工具名 | Server RPC | gRPC FullMethod | 输入要点 |
|--------|------------|-----------------|----------|
| `list_users` | `Users.ListUsers` | `/torchwood.server.v1.UsersService/ListUsers` | `page_size` / `page_token` / `queries[]` |
| `get_user` | `Users.GetUser` | `/torchwood.server.v1.UsersService/GetUser` | `id` |
| `create_user` | `Users.CreateUser` | `/torchwood.server.v1.UsersService/CreateUser` | `email`, `password`；可选 `name` / `status` / `labels` / `prefs` |
| `query_documents` | `Databases.ListDocuments` | `/torchwood.server.v1.DatabasesService/ListDocuments` | 必填 `database_id`、`collection_id`。**优先** `query`（AST，见 §3）；仍接受 `queries[]` + 分页；两者冲突 → InvalidArgument |
| `get_document` | `Databases.GetDocument` | `/torchwood.server.v1.DatabasesService/GetDocument` | `database_id`, `collection_id`, `document_id` |
| `create_document` | `Databases.CreateDocument` | `/torchwood.server.v1.DatabasesService/CreateDocument` | 三元组 + `data`；可选 `permissions` |
| `update_document` | `Databases.UpdateDocument` | `/torchwood.server.v1.DatabasesService/UpdateDocument` | 三元组；可选 `data` / `permissions` / `increment`。用户集合须带 `version`（OCC） |
| `upsert_document` | `Databases.UpsertDocument` | `/torchwood.server.v1.DatabasesService/UpsertDocument` | 三元组 + `data`；可选 `permissions`、`conflict_columns` |
| `delete_document` | `Databases.DeleteDocument` | `/torchwood.server.v1.DatabasesService/DeleteDocument` | 三元组；用户集合须带 `version`（OCC） |
| `list_collections` | `Databases.ListCollections` | `/torchwood.server.v1.DatabasesService/ListCollections` | `database_id`；可选 `queries[]`、分页 |
| `get_collection` | `Databases.GetCollection` | `/torchwood.server.v1.DatabasesService/GetCollection` | `database_id`, `collection_id` |
| `invoke_function` | `Functions.CreateExecution` | `/torchwood.server.v1.FunctionsService/CreateExecution` | `function_id`；可选 `deployment_id`、`data`、`async` |
| `list_files` | `Storage.ListFiles` | `/torchwood.server.v1.StorageService/ListFiles` | `bucket_id`；可选 `queries[]`、分页 |
| `get_file` | `Storage.GetFile` | `/torchwood.server.v1.StorageService/GetFile` | `bucket_id`, `file_id` |
| `grant_asset` | `Assets.Grant` | `/torchwood.server.v1.AssetsService/Grant` | `owner_id`, `def_code`, `quantity`, `idempotency_key`；可选 `expires_at` / `level` / `metadata` / `ref_type` / `ref_id` |
| `list_user_assets` | `Assets.ListUserAssets` | `/torchwood.server.v1.AssetsService/ListUserAssets` | `owner_id`；可选分页 |
| `get_order` | `Payments.GetOrder` | `/torchwood.server.v1.PaymentsService/GetOrder` | `order_id` |
| `get_health` | `Health.Check` | `/torchwood.server.v1.HealthService/Check` | 无入参（ACCESS_PUBLIC） |

补充要点：

- 上传分片、OAuth 回调、Realtime WebSocket 为自定义 HTTP，不在本表，也不可经 `InvokeJSON` 调用。完整字段以 `tools.go` 的 `InputNotes` 与对应 proto 为准。
- **OCC 冲突合并重试**：`update_document` / `delete_document` 撞版本时返回 `DOCUMENT.VERSION_CONFLICT`（FailedPrecondition，retryable），错误体 ErrorInfo metadata 携带 `current_version=<当前 _version>`（探测读到的实际值，零额外查询）——Agent 直接取该值重放合并重试，不必先 GET 文档。
- **契约发现面**：① `GET /v1/server/databases/{database_id}/collections/{collection_id}:exportSchema?as=jsonschema` 导出集合契约的 **JSON Schema 2020-12** 文档——Agent 据此合成 / 校验文档载荷（attrs 类型映射、`required`、系统字段以 readOnly 注释）。② `GET /.well-known/torchwood` 为机器可读目录：查询算子全集、域码表（code + retryable）、databases 面动词的 REST 形态与 scope 清单——Agent 接入先读目录再选动词。

## 3. query_documents 双栈

`ListDocumentsRequest` 同时承载两套查询：

- `optional shared.v1.Query query`：`filter` 树 + `orders` + `page_size` / `page_token`（权威）；
- 旧栈 `queries[]` + 分页（查询 DSL 串，如 `equal("status","active")`）。

两者同时提供且冲突 → InvalidArgument。

### 3.1 gRPC / InvokeJSON / InvokeTool（protojson，camelCase）

Agent 默认填 AST：

```json
{
  "databaseId": "app",
  "collectionId": "notes",
  "query": {
    "filter": { "eq": { "attribute": "status", "values": ["active"] } },
    "pageSize": 20
  }
}
```

旧客户端只填 `queries` 仍有效。

### 3.2 HTTP（grpc-gateway）

| 方式 | 路径 | 载荷 |
|------|------|------|
| GET 旧栈 | `GET /v1/server/databases/{db}/collections/{coll}/documents` | query：`queries` / `page_size` / `page_token` |
| POST AST | `POST /v1/server/databases/{db}/collections/{coll}/documents:list` | **body 是 `shared.v1.Query` 本身**（非整包 ListDocumentsRequest） |

TS SDK 尚无 `documents:list` 封装；需 AST 时直接 `fetch` 该路径或走 Go `InvokeTool`。不要把 §3.1 的 InvokeJSON 示例原样 POST 到 REST。

## 4. 调用方式

### 4.1 Go

```go
import "github.com/torchwoodcloud/torchwood/sdk/go/server"

srv, _ := server.New("127.0.0.1:9060", server.WithAPIKey(key), server.WithProjectID("app"))

// 按工具名（catalog 校验）
out, err := srv.InvokeTool(ctx, "query_documents", reqJSON) // 等价 InvokeJSON(tool.FullMethod, ...)

// 逃生舱：任意 Server unary（排除 APIKeysService）
respJSON, err := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{"pageSize":10}`))
```

`InvokeTool` 未命中返回 `torchwood: unknown tool "<name>"`。

### 4.2 TypeScript

```ts
import { agentTools, lookupAgentTool } from "@torchwood/sdk";

const tool = lookupAgentTool("query_documents"); // { name, fullMethod, inputNotes } | undefined
console.log(agentTools.map(t => t.name));        // 18 个只读条目
```

TS 不提供 `InvokeJSON`；catalog 仅提供名字与 `fullMethod`，实际执行由宿主按 `fullMethod` 自选 gRPC / HTTP 传输。

## 5. 完整 API 权威来源

- **Proto**：`proto/client/`、`proto/server/`、`proto/console/`、`proto/shared/`；
- **OpenAPI**：`mise run generate:proto` 后的 `genproto/**/*.swagger.json`（snake_case 字段名，时间 RFC3339）；
- **Scope**：Server RPC 的 scope 门随 `method_auth` 声明在 proto（策略唯一声明源），启动期收集进 PolicySet 并 fail-closed 校验（见 `05-authentication.md` §3/§7）；
- **计数**：以 `authz-matrix.md` 头部与 `grep -r "^\s*rpc " proto | wc -l` 实时结果为准（数字随 API 演进变化）。

Agent 集成建议：以 `genproto/**/*.swagger.json` 为 schema 权威生成工具 schema；`agentTools` / `Tools` 仅作默认 18 动词的便捷别名。

## 6. 边界与常见问答

**Q: 18 个够用吗？**

够做大部分 Agent 用例（查用户 / 读写文档 / 查集合 / 调函数 / 查文件 / 资产与订单）。其余（如建库 / 建属性 / 删数据）走 `InvokeJSON` 逃生舱，无需等工具箱收录。

**Q: 为什么不把 API Key 管理放进来？**

`APIKeysService` 被双重排除：泄露的 Key 若能自铸新 Key，等同永久提权。密钥生命周期只在 Console 或 PERMISSION 面（key 凭证禁入）处理——scope 词表已无 `apikeys` 资源，创建携带 `apikeys:write` 的 key 会被词表校验直接拒绝。

**Q: 一个项目跑多个 Agent（多个 API key），数据互相可见吗？**

不（per-key 私有）。每个 key 创建文档时空 ACE 种子绑 `read/update/delete:key:<自身id>`——默认只有创建者 key 可读写删，其他 key 查询返回 NotFound（防枚举）。跨 key 协作（如主 Agent 复核子 Agent 产出）需由持有者显式授予对方 `key:<id>` 的文档 ACE；`_created_by` 字段即对方 key 的授予目标。遗留的显式 `keys` 授予仍共享（向后兼容），但默认不再产生。详见 `06-databases.md` §7。

**Q: TS SDK 没有 InvokeJSON 怎么办？**

TS 属 fetch 层，`HttpTransport.request` 已支持 `auth:"apiKey"` 的任意路径；按 genproto 的 OpenAPI 路径直接 `fetch` 即可。`agentTools` 只给 `fullMethod` 就是为宿主自选传输。

**Q: 新增 RPC 后要改动哪里？**

- Proto 层：按 `09-api-guide.md` 加 `method_auth`（access + admin_roles / api_key_scope）与 `google.api.http`，字段删除必 `reserved`；
- 注解即策略：无需在任何 Go 侧登记 scope / 角色——启动期从 proto 收集并过语义断言，漏配直接启动失败；
- 工具层（可选）：仅当产品决定收录为默认动词时，才在 `tools.go` / `tools.ts` 追加；
- 生成物：`mise run generate:proto` 后提交 genproto，`buf breaking` 拦截不兼容变更；`mise run gen:authz-matrix` 重新生成矩阵文档。

## 7. 本地验证

```bash
mise run generate:proto                               # 生成 genproto
buf breaking --against '.git#branch=origin/main'  # 无 breaking change
golangci-lint run ./...                           # 全量门禁
go test ./sdk/go/server -run TestTools -v         # 校验 18 条 catalog 与 FullMethod 存在性
# OpenAPI 权威检查：每个 Server RPC 在 genproto/server/v1/*.swagger.json 有且仅有一条 operationId
```

## 相关文档

- `12-sdk.md` — InvokeJSON 与 SDK 全景
- `09-api-guide.md` — proto 注解与 OpenAPI 建模
- `16-document-modeling.md` — Agent 写文档的建模规约
