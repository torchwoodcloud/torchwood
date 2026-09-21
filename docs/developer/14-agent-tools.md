# 14 Agent 默认工具箱

Overlay，不是新 API。完整产品面仍是全部 RPC（当前 **263 个**：Client 69 + Server 170 + Console 24），Agent 默认仅暴露 **18 个动词**。

> 计数权威：`docs/developer/authz-matrix.md` 头部（生成物，`mise run gen:authz-matrix` 渲染，字节级漂移锁定）。
> 动词映射：`sdk/go/server/tools.go`（`Tools`）与 `sdk/typescript/src/server/tools.ts`（`agentTools`），两份 catalog 同名同值。
> OpenAPI 以 `genproto/**/*.swagger.json` 为权威。

## 1. 定位

- **不是**新增一个 API 面，也**不是**把产品面砍到 18 个动词——Agent / 自动化默认只看见 18 动词表，Console、CLI、SDK 仍走完整 Server API。
- 逃生舱是 `InvokeJSON(fullMethod, protojson)`（`sdk/go/server/invoke.go`）：覆盖全部 `torchwood.server.v1.*` unary，仅排除 `APIKeysService` 整个服务（含 `WhoAmI`）。新增 RPC 自动可用，无需改动工具箱。
- 本 catalog **不含** API key 的 create / list / get / delete——密钥只在 Console 或带合适 scope 的管理流程里创建，不交给普通 Agent 工具面。
- 全量计数口径：`proto/client` + `proto/server` + `proto/console` 的全部 `rpc` 条目，分布见 `authz-matrix.md` 头部（当前：PUBLIC 30 · END_USER 47 · SERVER 156 · PERMISSION 30 · SYSTEM 0）。
- catalog 只覆盖 7 个服务：Users、Databases、Functions、Storage、Assets、Payments、Health。其余 Server 服务（groups、projects、runtime_vars、leaderboards、analytics、runbook、billing、subscriptions、outbox、audit_logs、oauth_providers、auth、apikeys）均未收录；是否收录为默认动词由产品决策。
- 产品定位背景：AI / Agent-Native（`docs/roadmap.md` §0）——Server API + 细粒度 scope 供 Agent 按需限权调用，本章的 18 动词表是其在 SDK 层的便捷投影。

## 2. 默认 18 个工具

顺序与 `sdk/go/server/tools.go` 的 `Tools` 一致，catalog 只读（Go `toolsByName` 值拷贝、TS `Object.freeze` + `Map`）：

| 工具名 | Server RPC | gRPC FullMethod | Key Scope | 输入要点 |
|--------|------------|-----------------|-----------|----------|
| `list_users` | `Users.ListUsers` | `/torchwood.server.v1.UsersService/ListUsers` | `users.read` | `queries[]`（静态表面 DSL，`ParseUserList` 白名单）+ `page_size` / `page_token` |
| `get_user` | `Users.GetUser` | `/torchwood.server.v1.UsersService/GetUser` | `users.read` | `id` |
| `create_user` | `Users.CreateUser` | `/torchwood.server.v1.UsersService/CreateUser` | `users.write` | `email`, `password`；可选 `name` / `status` / `labels` / `prefs` |
| `query_documents` | `Databases.ListDocuments` | `/torchwood.server.v1.DatabasesService/ListDocuments` | `databases.read` | 必填 `database_id`、`collection_id`。过滤走 `query`（单 AST，见 §3）；顶层 `page_size` / `page_token` 作 GET 面简单分页，与 `query` 内同名字段冲突即拒。DSL 字符串字段已从 proto 移除（`reserved`） |
| `get_document` | `Databases.GetDocument` | `/torchwood.server.v1.DatabasesService/GetDocument` | `databases.read` | `database_id`, `collection_id`, `document_id` |
| `create_document` | `Databases.CreateDocument` | `/torchwood.server.v1.DatabasesService/CreateDocument` | `databases.write` | 三元组 + `data`；可选 `permissions` / `request_id`（写幂等键） |
| `update_document` | `Databases.UpdateDocument` | `/torchwood.server.v1.DatabasesService/UpdateDocument` | `databases.write` | 三元组；可选 `data` / `permissions` / `increment` / `array_updates`（数组列原子更新）/ `request_id`。用户集合须带 `version`（OCC） |
| `upsert_document` | `Databases.UpsertDocument` | `/torchwood.server.v1.DatabasesService/UpsertDocument` | `databases.write` | 三元组 + `data`；可选 `permissions` / `conflict_columns` / `request_id` |
| `delete_document` | `Databases.DeleteDocument` | `/torchwood.server.v1.DatabasesService/DeleteDocument` | `databases.write` | 三元组；用户集合须带 `version`（OCC）；可选 `request_id` |
| `list_collections` | `Databases.ListCollections` | `/torchwood.server.v1.DatabasesService/ListCollections` | `databases.read` | `database_id` + `page_size` / `page_token`（proto 仍留 `queries` 字段，服务端不消费） |
| `get_collection` | `Databases.GetCollection` | `/torchwood.server.v1.DatabasesService/GetCollection` | `databases.read` | `database_id`, `collection_id` |
| `invoke_function` | `Functions.CreateExecution` | `/torchwood.server.v1.FunctionsService/CreateExecution` | `functions.write` | `function_id`；可选 `deployment_id`（缺省最新 ready）、`data`、`async` |
| `list_files` | `Storage.ListFiles` | `/torchwood.server.v1.StorageService/ListFiles` | `storage.read` | `bucket_id` + `page_size` / `page_token`；`queries[]` 不支持，携带即 InvalidArgument |
| `get_file` | `Storage.GetFile` | `/torchwood.server.v1.StorageService/GetFile` | `storage.read` | `bucket_id`, `file_id` |
| `grant_asset` | `Assets.Grant` | `/torchwood.server.v1.AssetsService/Grant` | `assets.write` | `owner_id`, `def_code`, `quantity`, `idempotency_key`；可选 `expires_at` / `level` / `metadata` / `ref_type` / `ref_id` |
| `list_user_assets` | `Assets.ListUserAssets` | `/torchwood.server.v1.AssetsService/ListUserAssets` | `assets.read` | `owner_id`；可选 `page_size` / `page_token` |
| `get_order` | `Payments.GetOrder` | `/torchwood.server.v1.PaymentsService/GetOrder` | `payments.read` | `order_id` |
| `get_health` | `Health.Check` | `/torchwood.server.v1.HealthService/Check` | —（PUBLIC） | 无入参 |

补充要点：

- **InputSchema**：Go `Tool` 结构体带 `InputSchema` 字段——init 时由 FullMethod 的 input descriptor 生成简易 JSON Schema（`type: object`；Struct→object、Timestamp→date-time、枚举→string；嵌套 message 展开一层且仅当字段数 ≤10），供 Agent/自动化直接驱动（`tools.go` `buildInputSchema`）。TS `AgentTool` 仅有 `name` / `fullMethod` / `inputNotes`。
- 上传分片、OAuth 回调、支付回调、函数触发器（`/f/{project_id}/{trigger_token}`）、Realtime WebSocket 为自定义 HTTP，不在本表，也不可经 `InvokeJSON` 调用。完整字段以 `tools.go` 的 `InputNotes` 与对应 proto 为准。
- **OCC 冲突合并重试**：`update_document` / `delete_document` 撞版本时返回 `DOCUMENT.VERSION_CONFLICT`（FailedPrecondition，retryable），错误体 ErrorInfo metadata 携带 `current_version=<探测读到的当前 _version>`（`internal/app/shared/docdb_errors.go`）——Agent 直接取该值重放合并重试，不必先 GET 文档。
- **契约发现面**：① `GET /v1/server/databases/{database_id}/collections/{collection_id}:exportSchema`（`ExportCollectionSchema`，REST 自定义动词，`as` 挂在本动词上、缺省即 jsonschema）导出集合契约的 **JSON Schema 2020-12** 文档——Agent 据此合成 / 校验文档载荷（attrs 类型映射、`required`、系统字段以 readOnly 注释）。② `GET /.well-known/torchwood` 为机器可读目录（纯 HTTP 静态路由，公开端点，payload 构造期直读单一事实源）：查询算子全集（canonical 名 + proto 字段 + 值数量约束，containsAny/containsAll 标注 array_only）、域码表（code + retryable）、databases 面 29 个动词的 REST 形态与 scope、API key scope 词表全量（resource + read/write/admin）。Agent 接入先读目录再选动词。③ `GET /v1/server/api-keys/whoami`（`WhoAmI`，自证凭证型 PUBLIC）返回调用 key 自身的行——Agent 自检凭证身份用。

## 3. query_documents 与单 AST 查询

`ListDocumentsRequest` 的唯一过滤载体是 `query`（`shared.v1.Query` typed AST）；DSL 字符串字段（`queries`）已从 proto 删除（`reserved 3` / `reserved "queries"`）。顶层 `page_size` / `page_token` 保留为 GET 面简单分页参数；与 `query` 内同名字段同时设置且不等 → InvalidArgument。

`Query` 形状（`proto/shared/v1/query.proto`）：

- `filter`：布尔表达式树。叶子 `Comparison`（attribute + values），算子 21 个：`eq` / `ne` / `lt` / `lte` / `gt` / `gte` / `in` / `contains` / `not_contains` / `starts_with` / `not_starts_with` / `ends_with` / `not_ends_with` / `search` / `not_search` / `is_null` / `is_not_null` / `between`(恰 2 值) / `not_between`(恰 2 值) / `contains_any` / `contains_all`（后两个仅 array=true 属性，服务端白名单校验）；组合子 `and` / `or` 递归嵌套（深度 ≤8）。否定一律用 `not*` 变体（index 友好），无通用 NOT。
- `orders`：`{attribute, desc}` 列表。
- `select`：投影——服务端把返回 `data` 裁剪到指定字段。
- `page_size` / `page_token`：权威分页器。
- `vector_search`：KNN 算子，在 filter 树之外（距离不可作布尔谓词）。与 `filter` 可组合（AND），与 `orders` 互斥；`page_size` 即 k，多页经服务端发放的 `kvc:` 距离游标续传；`metric` 缺省 COSINE，`max_distance` 为后置阈值，`ef_search` ∈ [1,500]。仅 typed AST，DSL 字符串不支持。命中时 `ListDocumentsResponse.distances` 与 documents 平行回传。

### 3.1 gRPC / InvokeJSON / InvokeTool（protojson，camelCase）

```json
{
  "databaseId": "app",
  "collectionId": "notes",
  "query": {
    "filter": { "eq": { "attribute": "status", "values": ["active"] } },
    "select": ["title", "status"],
    "pageSize": 20
  }
}
```

### 3.2 HTTP（grpc-gateway）

| 方式 | 路径 | 载荷 |
|------|------|------|
| GET 简单分页 | `GET /v1/server/databases/{db}/collections/{coll}/documents` | query：`page_size` / `page_token`（无过滤） |
| POST AST | `POST /v1/server/databases/{db}/collections/{coll}/documents:list` | **body 是 `shared.v1.Query` 本身**（proto 注解 `body: "query"`，非整包 ListDocumentsRequest） |

TS SDK 的 `ServerDatabasesService.listDocuments` 已封装双形态：带 `query` 时走 POST `:list`（分页字段并入 body），无 `query` 时走 GET 简单分页；`vector_search` 查询回传 `distances`。

其他 list 面的查询形态各不相同，勿混用：`list_users` 的 `queries[]` 是静态表面 DSL（`ParseUserList` 白名单：equal/greaterThan/lessThan + 白名单属性）；`list_collections` 的 `queries[]` 服务端不消费；`list_files` 携带 `queries[]` 直接拒绝。

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

TS 不提供 `InvokeJSON`；catalog 仅提供名字与 `fullMethod`，实际执行由宿主自选：可直接 `HttpTransport.request(method, path, { auth: "apiKey" })` 打任意 OpenAPI 路径，或用 `@torchwood/sdk` 的 server 服务类（如 `ServerDatabasesService.listDocuments`，已封装 §3.2 双形态）。

## 5. 完整 API 权威来源

- **Proto**：`proto/client/`、`proto/server/`、`proto/console/`、`proto/shared/`；
- **OpenAPI**：`mise run generate:proto` 后的 `genproto/**/*.swagger.json`（snake_case 字段名，时间 RFC3339）；
- **Scope**：Server RPC 的 scope 门随 `method_auth` 声明在 proto（策略唯一声明源），启动期收集进 PolicySet 并 fail-closed 校验（见 `05-authentication.md` §3/§7）；
- **计数**：以 `authz-matrix.md` 头部与 `grep -c "^  rpc " proto/{client,server,console}/v1/*.proto` 实时结果为准（数字随 API 演进变化）。

Agent 集成建议：以 `genproto/**/*.swagger.json` 为 schema 权威生成工具 schema（或直接消费 Go `Tool.InputSchema`）；`agentTools` / `Tools` 仅作默认 18 动词的便捷别名。

## 6. 边界与常见问答

**Q: 18 个够用吗？**

够做大部分 Agent 用例（查用户 / 读写文档 / 查集合 / 调函数 / 查文件 / 资产与订单）。其余（如建库 / 建属性 / 删数据 / runtime_vars 管理）走 `InvokeJSON` 逃生舱，无需等工具箱收录。

**Q: 为什么不把 API Key 管理放进来？**

`APIKeysService` 被双重排除：catalog 不收录；`InvokeJSON` 的方法解析也拒绝整个服务。泄露的 Key 若能自铸新 Key，等同永久提权。scope 词表从 PolicySet 派生且不含 `apikeys` 资源（当前 17 个资源，`proto/shared/v1/authz.proto` `SCOPE_RESOURCE_*`），创建携带 `apikeys:write` 的 key 会被词表校验直接拒绝。唯一例外是 `WhoAmI`（自证凭证型 PUBLIC）：只读出调用 key 自身，不经 `InvokeJSON`，走 REST `GET /v1/server/api-keys/whoami`。

**Q: 一个项目跑多个 Agent（多个 API key），数据互相可见吗？**

不（per-key 私有）。每个 key 创建文档时空 ACE 种子绑 `read/update/delete:key:<自身id>`——默认只有创建者 key 可读写删，其他 key 查询不可见（防枚举）。跨 key 协作（如主 Agent 复核子 Agent 产出）需由持有者显式授予对方 `key:<id>` 的文档 ACE；`_created_by` 字段即对方 key 的授予目标。遗留的显式 `keys` 授予仍共享（向后兼容），但默认不再产生。详见 `06-databases.md` §7。

**Q: Agent 如何自举（bootstrap）？**

先 `GET /.well-known/torchwood` 拿算子表 / 域码表 / 动词清单 / scope 词表，按集合调 `:exportSchema` 拿 JSON Schema 合成载荷，再从 `agentTools` / `Tools` 选默认动词；超出 18 动词的需求按 swagger 路径走 HTTP 或 `InvokeJSON`。凭证自检用 `whoami`。

**Q: TS SDK 没有 InvokeJSON 怎么办？**

TS 属 fetch 层：`HttpTransport.request` 已支持 `auth:"apiKey"` 的任意路径，server 服务类覆盖常用面（含 `documents:list` AST 查询）。`agentTools` 只给 `fullMethod` 就是为宿主自选传输。

**Q: 新增 RPC 后要改动哪里？**

- Proto 层：按 `09-api-guide.md` 加 `method_auth`（access + admin_roles / api_key_scope）与 `google.api.http`，字段删除必 `reserved`；
- 注解即策略：无需在任何 Go 侧登记 scope / 角色——启动期从 proto 收集并过语义断言，漏配直接启动失败；矩阵文档由 `mise run gen:authz-matrix` 重新生成；
- 工具层（可选）：仅当产品决定收录为默认动词时，才在 `tools.go` / `tools.ts` 追加；
- 生成物：`mise run generate:proto` 后提交 genproto（含 `buf lint` + `buf breaking`）。

## 7. 本地验证

```bash
mise run generate:proto                            # buf lint + 对 origin/main 的 breaking 检查
mise run gen:authz-matrix                          # 重渲染授权矩阵（漂移即红）
go test ./sdk/go/server -run TestTools -v          # 校验 18 条 catalog 与 FullMethod 存在性
go test ./cmd/server/internal/runtime -run Swagger # OpenAPI 一致性（access 扩展 / snake_case / 错误体）
go test ./internal/api/serverhttp -run WellKnown   # 目录与 pkg/query、proto oneof、错误码目录的防漂移断言
```

## 相关文档

- `12-sdk.md` — InvokeJSON 与 SDK 全景
- `09-api-guide.md` — proto 注解与 OpenAPI 建模
- `05-authentication.md` — 凭证、scope 词表与策略注册表（§6 API Key 与 scope）
- `06-databases.md` — 文档权限模型与 per-key 隔离（§7）
- `16-document-modeling.md` — Agent 写文档的建模规约
- `docs/developer/authz-matrix.md` — 授权矩阵（生成物，计数权威）
