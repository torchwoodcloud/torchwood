# 09 后端 API 开发指南

> 面向后端开发者：以 `ProjectsService` 为范例，走完 `proto→genproto→domain→app→infra→api→Wire` 全流程，并约定分页、错误与 OpenAPI 一致性。
> 源码：`proto/server/v1/projects.proto`、`internal/api/servergrpc/`、`internal/app/server/`、`pkg/crud/`、`cmd/server/internal/runtime/grpc_swagger_test.go`。

## 1 调用链总览

```
gRPC handler (internal/api/*grpc) → app use-case (internal/app/*) → domain port (internal/domain/*, interface) → infra adapter (internal/infra/{bun,documentdb,storage})
```

每层只依赖下层接口；Wire（`cmd/server/provides.go→wire_gen.go`）按构造器类型自动装配。

## 2 步骤 1：proto 定义

`proto/` 四组：`server/v1`（管理面，API Key / Admin）/`client/v1`（终端用户）/`console/v1`（Console 专用）/`shared/v1`（`authz.proto`/`common.proto`/`document.proto`/`error.proto`/`query.proto`）。生成到 `genproto/`（禁止手改）。

以 `proto/server/v1/projects.proto` 为模板（节选，完整注解见 §2.1）：

```proto
syntax="proto3";
package torchwood.server.v1;
import "google/api/annotations.proto";
import "google/google/protobuf/timestamp.proto";
import "shared/v1/authz.proto";
import "shared/v1/common.proto";
option go_package="github.com/torchwoodcloud/torchwood/genproto/server/v1;serverv1";
option (grpc.gateway.protoc_gen_openapiv2.options.openapiv2_swagger) = {
  security_definitions:{ security:{key:"apiKey" value:{type:TYPE_API_KEY in:IN_HEADER name:"X-API-Key"}}}
  security:{security_requirement:{key:"apiKey" value:{}}}
  extensions:{key:"x-torchwood-access" value:{string_value:"server"}}
};
service ProjectsService {
  option (torchwood.shared.v1.service_auth)={default_access:ACCESS_SERVER};
  rpc CreateProject(CreateProjectRequest) returns (Project){
    option (google.api.http)={post:"/v1/server/projects" body:"*"};
    option (torchwood.shared.v1.method_auth)={access:ACCESS_PERMISSION permissions:["owner","admin"]};
  }
  rpc ListProjects(shared.v1.ListRequest) returns (ListProjectsResponse){
    option (google.api.http)={get:"/v1/server/projects"};
    option (torchwood.shared.v1.method_auth)={api_key_scope:{resource:SCOPE_RESOURCE_PROJECTS op:SCOPE_OP_READ}};
  }
  rpc GetProject(GetProjectRequest) returns (Project){
    option (google.api.http)={get:"/v1/server/projects/{id}"};
    option (torchwood.shared.v1.method_auth)={api_key_scope:{resource:SCOPE_RESOURCE_PROJECTS op:SCOPE_OP_READ}};
  }
  rpc UpdateProject(UpdateProjectRequest) returns (Project){
    option (google.api.http)={patch:"/v1/server/projects/{id}" body:"*"};
    option (torchwood.shared.v1.method_auth)={admin_roles:[ADMIN_ROLE_MEMBER,ADMIN_ROLE_ADMIN,ADMIN_ROLE_OWNER]
                                              api_key_scope:{resource:SCOPE_RESOURCE_PROJECTS op:SCOPE_OP_WRITE}};
  }
}
```

### 2.1 鉴权注解（强制，策略唯一声明源）

`proto/shared/v1/authz.proto`：`AccessLevel = PUBLIC / END_USER / SERVER / PERMISSION / SYSTEM`（凭证族第一维）；`MethodAuth{access, permissions, admin_roles, api_key_scope}` 扩展 `52001`，`ServiceAuth{default_access}` 扩展 `52002`。语义：

| Access | 凭证族 | 细粒度门 |
|--------|--------|----------|
| `ACCESS_PUBLIC` | 匿名可调 | —（client 面 PUBLIC 方法须显式登记白名单，防误标） |
| `ACCESS_END_USER` | 端用户会话/JWT（Client 面专属） | `permissions` 归一 `["users"]` |
| `ACCESS_SERVER` | admin 会话 **或** API key | `admin_roles`（viewer/member/admin/owner；空=不限角色）+ `api_key_scope`（缺省=不对 key 开放） |
| `ACCESS_PERMISSION` | admin 会话专属 | `permissions`（如 `["owner","admin"]`）；API key 一律拒绝 |
| `ACCESS_SYSTEM` | 内部预留 | 当前禁用（启动断言拒绝） |

方法级 `method_auth` 优先，缺省回落服务级 `service_auth.default_access`；细粒度字段仅方法级携带。策略由 `cmd/server/internal/runtime` 启动期收集为 `PolicySet` 并过全量语义断言（档位/死 scope/值域/项目寻址——见 `05-authentication.md` §3/§7），未解析出 authz 的方法启动即 `missing auth policy`。每方法须同步 OpenAPI 扩展 `x-torchwood-access`（值域 `public/end_user/server/permission`，§10 一致性测试锁定）。

**API key scope 语法（key 持有侧）**：`*`/`all`、`<resource>`、`<resource>.read/.write` 为既有形态；T-02 起可寻址资源（`databases`/`storage`）支持实例限定 `databases:<database_id>[.read|.write]`、`storage:<bucket_id>[.read|.write]`——请求按方法声明的资源族提取目标实例强制匹配，无实例寻址的方法（List/CreateBucket 等全集型）对实例限定 scope 一律 403。完整语法表、DDL 归属与创建校验规则见 `05-authentication.md` §6。

### 2.2 消息约定

- 更新类 `optional` 表达 presence：`optional string name=2;` 未传=不修改（`HasName()` 判别）；空串语义由 `UpdateCollectionRequest` 注释显式说明。
- 删除字段一律 `reserved`（字段号+字段名，禁止复用），`buf breaking --against '.git#branch=origin/main'` 门禁。
- 时间 `google.protobuf.Timestamp`（HTTP JSON RFC3339，`timestamppb.New`）。
- 列表统一 `shared.v1.ListRequest`/`ListResponseMeta`（`proto/shared/v1/common.proto:7`），勿重造分页字段。

### 2.3 形状校验注解（protovalidate）

请求"形状约束"（required / 长度 / 正则 / 枚举 / 范围）用 `buf.validate` 注解声明在 proto 上，`ValidateInterceptor`（`internal/api/interceptor/validate.go`，拦截器链尾、audit/usage 之后）统一求值，handler 与 app 层不再重复此类检查。违规 → `InvalidArgument`，消息为 `字段路径: 文案`（多条以 "; " 连接）；CEL 编译/求值故障 → `Internal`（fail-closed，注解缺陷属服务端 bug）。跨字段与业务规则仍写在 app 用例层（P3-18 取舍不变：app 层可用 `status/codes`）。链尾插入保证校验失败的请求照常产生 InvalidArgument 审计行与用量计数，与手写校验时期行为一致。

```proto
import "buf/validate/validate.proto";

message DeleteSessionRequest {
  // proto3 隐式 presence 标量：required 即"非零值"（空串视为未设置）。
  string session_id = 1 [(buf.validate.field).required = true];
}
```

注意：
- 消息字段（如 `google.protobuf.Struct`）的 `required` 为"必须设置"（非 nil）。
- 共享消息（`shared.v1.ListRequest` 等 AIP-132 复用方）加规则会作用于全部复用 RPC，需全量评估影响面。
- `grpc-ecosystem/openapiv2` 插件不把 `buf.validate` 规则映射为 OpenAPI 约束，对外字段约束仍按 §10 手工维护（`openapiv2_field`）。
- 绕过 gRPC 拦截器链的入口（`internal/api/serverhttp`、realtime、console）不经过本拦截器，其入参校验维持原状。

## 3 步骤 2：生成

```bash
task generate:proto # buf lint + buf generate（buf.gen.yaml v2：go/gateway/grpc/openapiv2 → genproto，paths=source_relative）
```

产物：`*_grpc.pb.go`（`XxxServiceServer` + `Register`）、`*.pb.gw.go`（`RegisterXxxHandlerFromEndpoint`）、`*.swagger.json`（`json_names_for_fields`）、`*.pb.go` 描述符（供 `cmd/server/internal/runtime.BuildMethodPolicies` 收集鉴权策略）。

## 4 步骤 3：domain 端口

`internal/domain/projects/project.go` 纯 struct（`string/time.Time/map`，无 protobuf 类型），`repository.go` 接口：

```go
type Repository interface {
  CreateProject(ctx context.Context, p *Project) error
  GetProject(ctx context.Context, id string)(*Project,error) // 不存在→(nil,nil)，由上层映射 NotFound
  ListProjects(ctx context.Context)([]Project,error)
}
```

跨资源端口按需新增（如 `APIKeyRepository`）。所有 infra 实现以 `wire.Bind(new(domainauth.SessionService), new(*auth.SessionService))` 绑定（`internal/infra/provides.go`）。

## 5 步骤 4：app 用例

`internal/app/server/projects.go` 以 `XxxCommand` 解耦 proto：鉴权→校验→事务→错误映射。

```go
principal, ok := contexts.Principal(ctx) // interceptor 注入
if principal.ActorKind!=shared.ActorKindAdmin || !principal.IsPlatformAdmin { return PermissionDenied }
if cmd.Name=="" { return InvalidArgument }
err := db.RunInTx(ctx, func(txCtx context.Context) error {
  if err:=projectRepo.CreateProject(txCtx,p); err!=nil{return err}
  schema,_:=ident.ProjectSchemaName(p.ID)
  conn.ExecContext(txCtx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quoteIdent(schema)))
  return projectschema.Apply(txCtx, db, p.ID)
})
```

- 越权返回 `NotFound` 防枚举（`GetProject` 对非绑定项目伪装）。
- 撞名先查后返回 `InvalidArgument`/`AlreadyExists`，勿依赖裸 `unique_violation→500`。
- 所有预期错误 `status.Error(codes.X, msg)`（裸 `errors.New` 会被 gateway 包为 `Internal`）。

## 6 步骤 5：infra 适配

元数据 `bun`：`internal/infra/bun/model/project.go` + `bunrepo/project_repo.go:NewSelect().Where("id=?").Scan`（`sql.ErrNoRows→nil`），构造器 `func NewXxxRepository(db *clients.Database) xxx.Repository`。

动态文档仅业务集合走 `internal/infra/documentdb/postgres.go:NewPostgresDocumentDB(db,pub)`：`schema-per-database + _tenant + _acl`（文档 ACE 内嵌，判定执行点在 RLS policy——见 `06-databases.md` §7），查询走 `pkg/query` typed AST（DSL 串仅是 SDK/CLI 客户端糖），字段白名单+敏感黑名单，未声明列→`InvalidArgument`；端口错误经 `internal/app/shared.MapDocumentDBError` 映射。

## 7 步骤 6：api handler

`internal/api/servergrpc/projects.go`：

```go
type ProjectsService struct{ serverv1.UnimplementedProjectsServiceServer; projects *appserver.Projects }
func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest)(*serverv1.Project,error){
  p, err:=s.projects.CreateProject(ctx, appserver.CreateProjectCommand{ID:req.GetId(), Name:req.GetName()})
  if err!=nil{return nil,err}
  return mapProject(p), nil // timestamppb.New 转换时间
}
```

职责：嵌 `Unimplemented`、参数→Command、用例→`map` 回 proto；更新类 `ctx=contexts.WithAuditResource(ctx,req.GetId())`；列表编码 token（下）。

### 7.1 列表分页（`shared.v1.ListRequest` + `pkg/crud`）

`proto/shared/v1/common.proto:7`：`page_size/page_token/queries`（`filter`/`order_by` 字段号 3/4 已 reserved——W-K 终结：静态表面从未实现 AIP-160/132，POC 无兼容义务）；响应 `ListResponseMeta{page_size,next_page_token,prev_page_token,total_count}`（AIP-132/158/160），其中 `total_count ≤0` 表示总数未知（keyset 分页下 0 与空集合不可区分，需以 `next_page_token` 是否为空判定是否还有更多）。

`pkg/crud/list.go:57` `ParseListParams(pageSize,pageToken,filter,orderBy)`：校验 `page_size∈[1,1000]`（默认 50）、`page_token` 解码得 `Offset`；`pagination.go:360` `BuildPaginationInfo(params,totalCount,hasMore)` 产出 `HasNext/NextOffset/HasPrevious/PreviousOffset`，`EncodePageToken(offset)`（`v1` base64 JSON，`DefaultTokenTTL=24h`）。

**页 token 安全（R4-J2-4）**：生产进程启动时经 `crud.InitPageTokenSigning(jwtSecret)` 启用 HMAC-SHA256 签名（purpose 派生密钥，与 JWT/OAuth 域隔离），此后：

- 签发侧 `EncodePageToken` 自动附加签名；解码侧伪造、篡改 offset 的 token 一律 `InvalidArgument`（历史 `v1:offset` 未签名简单格式已退役，不再解析）；
- token 结构保留 `order_by`/filter digest 绑定字段，`ParseListParams`/`ValidatePageTokenForRequest` 在 token 携带时校验跨页一致性；当前唯一签发方 `EncodePageToken` 仅写 offset，不携带绑定；
- offset 上限 `MaxQueryOffset=10000`，超出拒绝（防伪造超深分页拖垮数据库）；
- 未启用签名的进程保持历史行为（接受未签名 token），便于灰度。

Handler：

```go
list, info, _ := s.projects.ListProjects(ctx, req.GetPageSize(), req.GetPageToken())
meta := &sharedv1.ListResponseMeta{PageSize: info.PageSize, TotalCount: int32(info.TotalCount)}
if info.HasNext { meta.NextPageToken = crud.EncodePageToken(info.NextOffset) }
if info.HasPrevious { meta.PrevPageToken = crud.EncodePageToken(info.PreviousOffset) }
```

- 勿手拼 SQL `filter/order`；动态文档查询一律走 `pkg/query` typed AST（见 `06-databases.md` §6）。
- `pkg/crud/filter.go`/`order.go` 供静态表列表复用；动态文档过滤载体唯一是 `shared.v1.Query`。

`shared.v1.ListRequest.queries`（Appwrite 风格 DSL 串）是**静态表面遗留通道**，按面分化：仅 `ListUsers` 经 `ParseUserList` 白名单解析（`equal/greaterThan/lessThan` + 白名单属性）；storage buckets/files 与 groups 携带即 `InvalidArgument`（显式拒绝）；documents 已将 `queries` **reserved**（服务端零字符串解析，DSL 只存活为 SDK/CLI 客户端糖）。

文档列表：GET 面保留 `page_size/page_token`，过滤/排序/投影一律 `POST .../documents:list`，body 即 Query JSON：

```bash
curl -X POST -H 'X-API-Key: <key>' -H 'Content-Type: application/json' \
  -d '{"filter":{"eq":{"attribute":"status","values":["published"]}},"orders":[{"attribute":"$createdAt","desc":true}],"pageSize":20}' \
  'http://127.0.0.1:9080/v1/server/databases/default/collections/<collection_id>/documents:list'
# 响应 {documents:[...], meta:{page_size:20,next_page_token:"...",total_count:42}}
```

## 8 步骤 7：Wire

`internal/{api,app,infra}/provides.go` 各自 `ProviderSet`，汇总于 `cmd/server/provides.go`。

```go
// api/provides.go
wire.Bind(new(serverv1.ProjectsServiceServer), new(*servergrpc.ProjectsService))
// app/provides.go
wire.NewSet(server.NewProjects)
// infra/bun/provides.go
wire.Bind(new(projects.Repository), new(*bunrepo.ProjectRepo))
```

改构造器签名后 `task wire:all`（含 `wire:server` + `wire:worker`）重生成 `cmd/server/wire_gen.go`。

### 注册

业务 proto 文件清单单一登记在 `cmd/server/internal/runtime/grpc.go` 的 `authzFileDescriptors()`（新增服务文件只登记此处）；`ProvideMethodPolicies` → `BuildMethodPolicies` 启动期收集策略并过语义断言，`assertRegisteredMethodsHaveAuthz` fail-closed（已注册方法必须命中 PolicySet）。gateway 侧在 `cmd/server/internal/runtime/grpc_gateway.go` 登记 `RegisterXxxHandlerFromEndpoint`。

## 9 错误与网关

用例层 `codes.Unauthenticated/PermissionDenied/NotFound/InvalidArgument/AlreadyExists`；`FailedPrecondition/OutOfRange` 用于 `version_*`/`超限`。

`cmd/server/internal/runtime/errors.go:HTTPErrorHandler` 统转 JSON：

```json
{"error":{"type":"invalid_request_error","code":"InvalidArgument","message":"...","error_id":"<uuid>","error_code":"ERROR_CODE_INVALID_REQUEST"}}
```

映射：`InvalidArgument→400/ERROR_CODE_INVALID_REQUEST`、`Unauthenticated→401/INVALID_CREDENTIALS`、`PermissionDenied→403/PERMISSION_DENIED`、`NotFound→404/RESOURCE_NOT_FOUND`、`AlreadyExists/Aborted→409/RESOURCE_CONFLICT/CONCURRENT_MODIFICATION`、`ResourceExhausted→429/QUOTA_EXCEEDED`、`DeadlineExceeded→504/TIMEOUT`。

## 10 OpenAPI 与一致性断言

每服务文件声明 `openapiv2_swagger`：`security_definitions{apiKey(X-API-Key),Bearer(Authorization: Bearer),cookie(Cookie: TORCHWOOD_session_console)}` + `security{apiKey}` + `extensions{x-torchwood-access: public/end_user/server/permission（与 AccessLevel 对应，§2.1）}` + `responses{default → .torchwood.shared.v1.ErrorResponse}`。

`method_auth` 与 `x-torchwood-access` 必须一致：未显式声明的 operation 继承 swagger 顶层（服务默认），`ACCESS_PUBLIC` 需 `security:[]`。

default 错误响应建模为声明式：`buf.gen.yaml` 对 openapiv2 插件设置 `disable_default_errors=true` 关闭生成器自带的 `rpcStatus` 注入（该结构与运行时错误体不符）；每个 service proto 文件级声明的 `responses.default` 会自动填充到该文件全部 operation，并使 `Error/ErrorResponse/ErrorCode` 定义经 customRefs 原生产出（legacy 命名，当前为 `v1Error/v1ErrorCode/v1ErrorResponse`）。新增 service 文件必须携带同一段 `responses.default`，否则该文件的 operation 将缺失错误契约，测试即红。

`cmd/server/internal/runtime/grpc_swagger_test.go` `TestSwaggerAccessExtensionMatchesCollectMethodsByAccess` 逐 `genproto/**/*.swagger.json` 断言：`businessFileDescriptors()`（复用 `grpc.go` 的 `authzFileDescriptors` 单一清单）→ `BuildMethodPolicies` 推导 access → 比对 `doc.XAccess`（顶层=服务默认）与每 `operation.x-torchwood-access`（继承或显式）完全一致，同时断言每个 operation 的 default 响应引用 `v1ErrorResponse`；`TestSwaggerNoRpcStatus` 断言全部 swagger 无 `rpcStatus` 残留（`disable_default_errors` 回退即红）。新增服务后 file 清单同步一处即可（swagger 测试复用同一清单），否则测试失败（≥14 文件、≥140 operation）。

## 11 OutboxService 示例（新增服务的完整参照）

`proto/server/v1/outbox.proto`（`ACCESS_SERVER` 默认，顶层 `x-torchwood-access=server`；方法级 `admin_roles:[ADMIN_ROLE_ADMIN,ADMIN_ROLE_OWNER]` + `api_key_scope` 写门）：

```proto
service OutboxService {
  rpc ListDeadLetters(ListDeadLettersRequest) returns (ListDeadLettersResponse){
    option (google.api.http)={get:"/v1/server/outbox/dead-letters"};
  }
  rpc ReplayDeadLetter(ReplayDeadLetterRequest) returns (ReplayDeadLetterResponse){
    option (google.api.http)={post:"/v1/server/outbox/dead-letters/{event_id}:replay" body:"*"};
  }
}
```

步骤复盘：`proto` 定义→`task generate:proto`→`internal/domain/shared/ports.go:OutboxRepository` 扩展→`internal/app/events/outbox_admin.go` 用例（`5s` per-statement 超时）→`internal/infra/events/outbox.go` 适配（`document_events_outbox_dead`）→`internal/api/servergrpc/outbox.go` handler（`ListRequest→crud.ParseListParams`）→`grpc.go`/`grpc_gateway.go` 注册→`task wire:all`。

CLI 调用：`torchwood outbox list-dead --project <id>` / `torchwood rpc /torchwood.server.v1.OutboxService/ListDeadLetters --data '{"project_id":"shop","pageSize":20}'`。

## 12 自检清单

1. `task generate:proto && go build ./...` 通过，`genproto/` 无手改；2. `task wire:all` 已重生成；3. `go vet` + `gofmt -l` 空；4. 错误码/分页符合 §7/§9；5. `TestSwaggerAccessExtensionMatches...` 通过；6. 集成测试参照 `internal/api/servergrpc/projects_test.go`（`stub repo + contexts.WithPrincipal`）与 `pkg/testutil` 真库。

## 13 参考

- `AGENTS.md` §编辑遵循模式（端口/适配器、`reserved`/`optional`/`Timestamp`、`pkg/crud/pkg/query`）、`README.md` §Architecture。
- `docs/developer/06-databases.md`（三层与 `pkg/query`）、`07-storage.md`（File Token 与 multipart）、`08-functions.md`（信号量与 Trim）。
- `sdk/README.md` 与 `sdk/go/server`（`InvokeJSON` 动态分发，CLI `import_guard_test.go`）。
