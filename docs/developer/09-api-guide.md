# 09 后端 API 开发指南

以 `ProjectsService` 为范例，走完新增一个 gRPC 方法的完整流程：`proto → genproto → domain → app → infra → api → Wire`，并约定鉴权注解、请求校验、分页、错误映射与 OpenAPI 一致性。

> 源码锚点：`proto/server/v1/projects.proto`、`internal/app/server/projects.go`、`internal/api/servergrpc/projects.go`、`cmd/server/internal/runtime/grpc.go`、`pkg/crud/`、`cmd/server/internal/runtime/grpc_swagger_test.go`。

## 1. 调用链总览

```
gRPC handler (internal/api/*grpc) → app use-case (internal/app/*) → domain port (internal/domain/*, 接口) → infra adapter (internal/infra/*)
```

每层只依赖下层接口；Wire（`cmd/server/provides.go` → `wire_gen.go`）按构造器类型自动装配。

gRPC 一元拦截器链序（装配于 `cmd/server/internal/runtime/grpc.go:120-127`；业务服务无流式 RPC，语义断言直接拒绝 streaming）：

```
clientInfo（trusted-proxy 校验后的客户端 IP）
→ auth（凭证解析 + PolicySet 判定，鉴权拒绝并联审计）
→ rateLimit（通用 API 限频，Redis 固定窗口）
→ audit（审计落库）
→ usage（用量计数）
→ validate（protovalidate 形状校验，链尾）
→ handler
```

`validate` 位于 audit/usage 之后、handler 之前：校验失败的请求照常产生审计行并计入用量（`internal/api/interceptor/validate.go:28-31`）。

## 2. 步骤一：proto 定义

`proto/` 分四组：`server/v1`（管理面，API Key / Console admin）、`client/v1`（终端用户）、`console/v1`（Console 专用）、`shared/v1`（跨面复用消息，现 7 个文件：`authz` 鉴权注解、`common` 分页、`document`/`query` 文档查询 AST、`error` 错误模型、`entities` 会话/订阅等共享基底、`leaderboard` 排行榜载荷）。生成产物在 `genproto/`（禁止手改）。

以 `proto/server/v1/projects.proto` 为模板（节选，完整注解见 §2.1/§10）：

```proto
syntax = "proto3";
package torchwood.server.v1;
import "buf/validate/validate.proto";
import "google/api/annotations.proto";
import "google/protobuf/timestamp.proto";
import "protoc-gen-openapiv2/options/annotations.proto";
import "shared/v1/authz.proto";
import "shared/v1/common.proto";
option go_package = "github.com/torchwoodcloud/torchwood/genproto/server/v1;serverv1";

option (grpc.gateway.protoc_gen_openapiv2.options.openapiv2_swagger) = {
  security_definitions:{ ... apiKey=X-API-Key / Bearer / cookie 三定义 ... }
  security:{security_requirement:{key:"apiKey" value:{}}}
  extensions:{key:"x-torchwood-access" value:{string_value:"server"}}
  // 统一 default 错误响应（运行时错误体即 shared.v1.ErrorResponse），
  // 文件级声明自动填充到本文件全部 operation。新增 service 文件必须携带。
  responses:{key:"default" value:{description:"An unexpected error response."
    schema:{json_schema:{ref:".torchwood.shared.v1.ErrorResponse"}}}}
};
service ProjectsService {
  option (torchwood.shared.v1.service_auth) = {default_access: ACCESS_SERVER};
  rpc CreateProject(CreateProjectRequest) returns (Project) {
    option (google.api.http) = {post: "/v1/server/projects" body: "*"};
    option (torchwood.shared.v1.method_auth) = {access: ACCESS_PERMISSION permissions: ["owner","admin"]};
    // PERMISSION 档必须逐方法声明 operation 级扩展（否则继承顶层 "server"，
    // 被一致性测试拦下）；SERVER 档方法继承顶层即可。
    option (grpc.gateway.protoc_gen_openapiv2.options.openapiv2_operation) = {
      extensions: { key: "x-torchwood-access" value: { string_value: "permission" } }
    };
  }
  rpc ListProjects(shared.v1.ListRequest) returns (ListProjectsResponse) {
    option (google.api.http) = {get: "/v1/server/projects"};
    option (torchwood.shared.v1.method_auth) = {api_key_scope: {resource: SCOPE_RESOURCE_PROJECTS op: SCOPE_OP_READ}};
  }
  rpc UpdateProject(UpdateProjectRequest) returns (Project) {
    option (google.api.http) = {patch: "/v1/server/projects/{id}" body: "*"};
    option (torchwood.shared.v1.method_auth) = {admin_roles: [ADMIN_ROLE_MEMBER, ADMIN_ROLE_ADMIN, ADMIN_ROLE_OWNER]
                                                api_key_scope: {resource: SCOPE_RESOURCE_PROJECTS op: SCOPE_OP_WRITE}};
  }
}
```

### 2.1 鉴权注解（强制，策略唯一声明源）

`proto/shared/v1/authz.proto` 定义 `AccessLevel` 与 `MethodAuth{access, permissions, admin_roles, api_key_scope}`（扩展号 52001）、`ServiceAuth{default_access}`（扩展号 52002）：

| Access | 凭证族 | 细粒度门（启动期语义断言强制） |
|--------|--------|----------|
| `ACCESS_PUBLIC` | 匿名可调 | client 面 PUBLIC 方法须显式登记白名单（`clientPublicMethodWhitelist`，防误标） |
| `ACCESS_END_USER` | 端用户会话 / JWT（client 面专属） | `permissions` 必须恰好归一 `["users"]` |
| `ACCESS_SERVER` | admin 会话**或** API key | `admin_roles`（viewer/member/admin/owner，空 = 不限角色）+ `api_key_scope`（**必填**——语义断言拒绝缺省，"不对 key 开放"请改用 PERMISSION 档） |
| `ACCESS_PERMISSION` | admin 会话专属 | `permissions` 必填（console 值域 `[console]`/`[owner]`/`[owner,admin]`，写动词仅 `[owner]` 除自助白名单）；API key 一律拒绝（scope `*` 也不放行） |
| `ACCESS_SYSTEM` | 内部预留 | 当前禁用（启动断言拒绝） |

方法级 `method_auth` 优先，缺省回落服务级 `service_auth.default_access`；细粒度字段仅方法级携带。策略由 `cmd/server/internal/runtime` 启动期经 `ProvideMethodPolicies` → `BuildMethodPolicies` 收集为 `PolicySet`（全进程唯一收集点）并过全量语义断言（档位合法性 / 死 scope / client·console 值域 / 项目寻址不变量 / streaming 禁用——见 `05-authentication.md` §3/§7），未解析出 authz 的方法启动即 `missing auth policy for method`；已注册但不在策略表的方法由 `assertRegisteredMethodsHaveAuthz` 启动失败兜底。每方法须同步 OpenAPI 扩展 `x-torchwood-access`（值域 `public/end_user/server/permission`，§10 的一致性测试锁定）。

**API key scope 语法（key 持有侧）**：`*` / `all`、`<resource>`、`<resource>.read/.write` 为基础形态；词表 17 资源（`authz.proto` `ScopeResource`，databases/users/groups/storage/projects/oauth_providers/functions/payments/assets/subscriptions/billing/outbox/audit_logs/leaderboards/analytics/runbooks/runtime_vars），方向 read/write/admin 三档（`SCOPE_OP_ADMIN` 按资源 opt-in，首个使用方为 leaderboards 的 board 管控）。可寻址资源（`databases` / `storage`）支持实例限定 `databases:<database_id>[.read|.write]`、`storage:<bucket_id>[.read|.write]`——请求按方法声明的资源族提取目标实例强制匹配，无实例寻址的方法（List / CreateBucket 等全集型）对实例限定 scope 一律 403。另有自定义服务标签 `<service>.<name>`（跨系统，TW 只存不解释）与 key 自述端点 `GET /v1/server/api-keys/whoami`。完整语法表与创建校验规则见 `05-authentication.md` §6。

### 2.2 消息约定

- 更新类请求用 `proto3 optional` 表达 presence：`optional string name = 2;` 未传 = 不修改（生成代码 `HasName()` 判别）；空串语义由字段注释显式说明（如 `UpdateVarSet` 只改元数据不入版本链）。
- 删除字段一律 `reserved`（字段号 + 字段名，禁止复用），由 `buf breaking --against '.git#branch=origin/main'` 门禁兜底（`mise run lint:proto`）。现例：`shared.v1.ListRequest` 的 filter/order_by（字段号 3/4）、`ListDocumentsRequest` 的 queries（字段号 3）。
- 时间一律 `google.protobuf.Timestamp`（HTTP JSON 为 RFC3339，`timestamppb.New` / `AsTime` 转换）。
- 列表统一复用 `shared.v1.ListRequest` / `ListResponseMeta`，勿重造分页字段。
- 跨面重复的实体消息逐步收敛进 `shared/v1/entities.proto` 共享基底（TokenBundle/Session/Group/SubscriptionPlan/Subscription），新增跨面实体优先共享。

### 2.3 形状校验注解（protovalidate）

请求"形状约束"（required / 长度 / 正则 / 枚举 / 范围）用 `buf.validate` 注解声明在 proto 上，由 `ValidateInterceptor`（`internal/api/interceptor/validate.go`，拦截器链尾）统一求值，handler 与 app 层不再重复此类检查。违规 → InvalidArgument，消息为 `字段路径: 文案`（多条以 `; ` 连接）；CEL 编译 / 求值故障 → Internal（fail-closed，注解缺陷属服务端 bug）。跨字段与业务规则仍写在 app 用例层（P3-18 取舍：app 层允许直接使用 `grpc/status`）。health/reflection 框架服务豁免求值。链尾插入保证校验失败的请求照常产生审计行与用量计数。

```proto
// proto/client/v1/account.proto DeleteSessionRequest
message DeleteSessionRequest {
  // proto3 隐式 presence 标量：required 即"非零值"（空串视为未设置）。
  string session_id = 1 [(buf.validate.field).required = true];
}
```

现状使用密度示例：`AuditLogsService.ListAuditLogs` 全字段有界（page_size ≤1000、各 string max_len）、`storage.proto` 的 `expires_in ∈ (0,3600]`、`runtime_vars` client 面集合名正则 `^[a-z_][a-z0-9_]*$` 与 oneof required。

注意：

- 消息字段（如 `google.protobuf.Struct`）的 `required` 为"必须设置"（非 nil）。
- 共享消息（`shared.v1.ListRequest` 等 AIP-132 复用方）加规则会作用于全部复用 RPC，需全量评估影响面。
- `grpc-ecosystem/openapiv2` 插件不把 `buf.validate` 规则映射为 OpenAPI 约束，对外字段约束仍按 §10 手工维护（`openapiv2_field`）。
- 绕过 gRPC 拦截器链的入口（`internal/api/serverhttp` 的 Storage multipart / OAuth / 触发器等自定义 handler、realtime）不经过本拦截器，其入参校验由各自 handler 承担。

## 3. 步骤二：生成

```bash
mise run generate:proto    # buf lint + buf generate
```

`buf.gen.yaml` 声明四个 remote 插件，全部输出 `genproto/`（`paths=source_relative`）：`protocolbuffers/go`、`grpc-ecosystem/gateway`、`grpc/go`、`grpc-ecosystem/openapiv2`。openapiv2 插件两个关键开关：`json_names_for_fields=false`（字段名保持 proto 声明的 snake_case，与运行时 marshaler `UseProtoNames:true` 一致）、`disable_default_errors=true`（关闭生成器自带的 rpcStatus 注入，§10）。

产物：`*_grpc.pb.go`（`XxxServiceServer` + `Register...`）、`*.pb.gw.go`（gateway handler）、`*.swagger.json`、`*.pb.go` 描述符（供 `BuildMethodPolicies` 收集鉴权策略）。

## 4. 步骤三：domain 端口

`internal/domain/projects/project.go` 为纯 struct（string / time.Time / map，无 protobuf 类型），`repository.go` 定义接口（节选）：

```go
type Repository interface {
  CreateProject(ctx context.Context, p *Project) error
  GetProject(ctx context.Context, id string) (*Project, error) // 不存在 → (nil, nil)
  GetProjectByName(ctx context.Context, name string) (*Project, error)
  ListProjects(ctx context.Context) ([]Project, error)
  UpdateProject(ctx context.Context, p *Project) error
  DeleteProject(ctx context.Context, id string) error
  DeleteProjectControlPlaneRows(ctx context.Context, projectID string) error
}
```

`GetProject` 的 `(nil, nil)` 约定：不存在不返回错误，由 handler 层显式映射 NotFound（避免 gRPC OK + 空响应的错误分类，见 §7）。跨资源端口按需新增（如 `APIKeyRepository`、`InviteCodeRepository`）；写路径窄端口独立声明、不向全部消费方扩散方法（如 `SettingsWriter` 单键原子写、`SchemaManager` 数据面 schema 生命周期）。

infra 实现有两种等价绑定形态：构造器直接返回 domain 接口（如 `func NewProjectRepository(db *clients.Database) projects.Repository`，ProviderSet 只登记构造器），或构造器返回具体类型 + `wire.Bind`（`internal/infra/provides.go:123-129` 的 users/session/group/storage 等存量仓储）。新增端口优先用前者。

## 5. 步骤四：app 用例

`internal/app/server/projects.go` 以 `XxxCommand` 解耦 proto：鉴权 → 校验 → 事务 → 错误映射。纵深防御 helper 在 `internal/app/shared/authz.go`（5 个）：`RequirePlatformPrincipal` / `RequireServerPrincipal` / `RequireConsolePrincipal` / `RequireEndUser` / `RequireAnyOf`——即使绕过拦截器直接调用 use-case 也 fail-closed。

```go
// CreateProject：PERMISSION [owner,admin]（proto 声明）+ 平台 admin 纵深防御。
func (s *Projects) CreateProject(ctx context.Context, cmd CreateProjectCommand) (*projects.Project, error) {
    if err := appshared.RequirePlatformPrincipal(ctx); err != nil { return nil, err }
    return s.CreateProjectInternal(ctx, cmd) // bootstrap 等系统路径复用，调用方自责授权
}

func (s *Projects) CreateProjectInternal(ctx context.Context, cmd CreateProjectCommand) (*projects.Project, error) {
    if err := ident.ValidateSchemaResourceID(cmd.ID); err != nil { return nil, appshared.MapIdentError(err) }
    if cmd.Name == "" { return nil, status.Error(codes.InvalidArgument, "name is required") }
    ...
    err := s.tx.Run(ctx, func(txCtx context.Context) error {   // uow.Runner 端口编排事务
        if err := s.projectRepo.CreateProject(txCtx, p); err != nil { return err }
        if err := s.schema.Ensure(txCtx, p.ID); err != nil { return err }   // 幂等建 tw_<id> schema + 静态表迁移
        return s.docDB.CreateDatabase(txCtx, p.ID, firstDBID, firstDBID)     // 缺省第一业务库 "app"
    })
    ...
}
```

约定：

- 项目是平台级资源：proto `ACCESS_PERMISSION [owner,admin]` 之外，use-case 入口再加 `RequirePlatformPrincipal`（纵深防御分层，authorization matrix 只反映 proto 声明，不反映 app 层收窄）。
- 越权返回 `NotFound` 防枚举（`GetProject`/`UpdateProject` 对非绑定项目伪装；Outbox 死信同理）。
- 撞名先查后返回 `InvalidArgument`（`GetProjectByName` 命中即拒），勿依赖裸 unique_violation → 500。
- "nothing to update" 前置检查放在取数之前，避免"资源不存在 + 全空请求"返回 NotFound 的语义歧义。
- 所有预期错误用 `status.Error(codes.X, msg)`（裸 `errors.New` 会被 gateway 包为 Internal）。

## 6. 步骤五：infra 适配

元数据走 bun：`internal/infra/bun/model/project.go` + `bunrepo/project_repo.go`（`NewSelect().Model(m).Where(...).Scan`，`sql.ErrNoRows → (nil, nil)`），构造器形如 `func NewProjectRepository(db *clients.Database) projects.Repository`。repo 感知调用方事务（ctx 携带事务时自动并入，§5 的 `tx.Run` 内调用即同事务）。

**bun 更新写规范（护栏 `bunrepo/update_guard_test.go`，详见 `17-update-write-guard.md`）**：UPDATE 一律显式声明写入列——struct 模型走 `.Column(白名单)`，nil 模型走 `.Set(...)`；禁止裸全模型覆盖 UPDATE（零值/nil 字段会被渲染成 `SET col = DEFAULT`，identity 列 `projects.internal_id` 会烧号并改写数据面租户号）：

```go
_, err := r.db.NewUpdate().Model(m).
    Column("name", "description", "registration_policy", "updated_at").
    WherePK().Exec(ctx)
```

新可变列必须显式登记进白名单（漏登记 = "改不动"，不是静默清零）。静态扫描覆盖 `internal/infra` 全部非测试 Go 文件的 `NewUpdate` 调用链。

动态文档仅业务集合走 `internal/infra/documentdb`：schema-per-database + `_tenant` + `_acl` 内嵌（判定执行点在 RLS policy，`SET LOCAL ROLE` + `app.roles` GUC 注入，见 `06-databases.md` §7），查询走 `pkg/query` typed AST（DSL 串仅是 SDK / CLI 客户端糖），字段白名单 + 敏感黑名单，未声明列 → InvalidArgument；端口错误经 `internal/app/shared.MapDocumentDBError` 映射。

## 7. 步骤六：api handler

`internal/api/servergrpc/projects.go`：

```go
type ProjectsService struct {
  serverv1.UnimplementedProjectsServiceServer
  projects *appserver.Projects
  invites  *appserver.InviteCodes
}
func (s *ProjectsService) CreateProject(ctx context.Context, req *serverv1.CreateProjectRequest) (*serverv1.Project, error) {
  p, err := s.projects.CreateProject(ctx, appserver.CreateProjectCommand{ID: req.GetId(), Name: req.GetName(), Description: req.GetDescription()})
  if err != nil { return nil, err }
  return mapProject(p), nil // timestamppb.New 转换时间
}
```

职责：嵌 `Unimplemented`、请求 → Command（`proto3 optional` 字段经 `req.Name != nil` 判别透传指针）、用例结果 → map 回 proto；repo 返回 `(nil, nil)` 时显式转 `status.Error(codes.NotFound, ...)`；更新/删除/敏感方法 `ctx = contexts.WithAuditResource(ctx, req.GetId())` 供审计行携带资源实例 ID；列表方法编码 page token（下）。

### 7.1 列表分页（shared.v1.ListRequest + pkg/crud）

`shared.v1.ListRequest` 携带 `page_size / page_token / queries / sort_order`（`filter` / `order_by` 字段号已 reserved——未实现的静默 no-op 一律消灭；`sort_order` 是 2026-09 新增的时间列方向枚举，见下）；响应 `ListResponseMeta{page_size, next_page_token, prev_page_token, total_count}`（AIP-132/158/160），其中 `total_count ≤0` 表示总数未知（keyset 分页下 0 与空集合不可区分，需以 `next_page_token` 是否为空判定是否还有更多）。

**时间列方向排序（`SortOrder`，2026-09）**：Console 列表页「按创建时间正/倒序」走 `sort_order`（`UNSPECIFIED`=历史默认 DESC / `ASC` / `DESC`）——固定排序键 = 各端点时间列（通常 `created_at`），**不是** AIP-160 任意列 `order_by`（W-K 终结裁决维持）。消费面以各 handler 显式行为为准：payments 订单 / subscriptions 订阅（各自有请求消息，方向与 `OrderListFilter`/`SubscriptionListFilter.Ascending` 合流）/ subscriptions 计划 / assets 定义与定义维度持有（时间 keyset：游标编码方向前缀 `a:`/`d:`，携带异向游标即 InvalidArgument，换向必须从第一页重来；ListUserLedger 的 `ascending` bool 为既有通道，行为一致）与 audit-logs（offset token，token 不编码方向，换向由调用方回第一页）。自有请求消息（`ListOrdersRequest`/`ListSubscriptionsRequest`/`ListAuditLogsRequest`/`ListDefAssetsRequest`）各自带同语义 `sort_order` 字段。

`pkg/crud`：

- `ParseListParams(pageSize, pageToken, filter, orderBy)`：`page_size∈[1,1000]`（默认 50，超上限收敛为 1000）、解码 page_token 得 offset、order_by/filter 与 token 内记录一致性校验、offset 上限 `MaxQueryOffset=10000`。
- `BuildPaginationInfo(params, totalCount, hasMore)`：产出 `HasNext/NextOffset/HasPrevious/PreviousOffset`。
- `EncodePageToken(offset) (string, error)`：`v1` base64 JSON，TTL 24h（marshal 失败返回 error，handler 转 Internal）。

**页 token 安全**：生产进程启动时经 `bootkit.InitPageTokenSigning`（`cmd/server/provides.go`，worker 同）启用 HMAC-SHA256 签名（`crud.InitPageTokenSigning(jwtSecret)`，purpose 派生密钥，与 JWT / OAuth 域隔离）。此后签发侧自动附加签名；解码侧对无签名 / 伪造 / 篡改 / 跨环境 token 一律拒绝（未签名的简单格式已退役不再解析）。token 结构保留 `order_by` / filter digest 绑定字段（跨页一致性校验）。

Handler 侧：

```go
list, info, err := s.projects.ListProjects(ctx, req.GetPageSize(), req.GetPageToken())
meta := &sharedv1.ListResponseMeta{PageSize: info.PageSize, TotalCount: int32(info.TotalCount)}
if info.HasNext {
    meta.NextPageToken, err = crud.EncodePageToken(info.NextOffset) // 两返回值，错误转 Internal
}
if info.HasPrevious {
    meta.PrevPageToken, err = crud.EncodePageToken(info.PreviousOffset)
}
```

原则：勿手拼 SQL filter / order；`pkg/crud/filter.go` / `order.go` 供静态表列表复用；动态文档过滤载体唯一是 `shared.v1.Query` typed AST。

`shared.v1.ListRequest.queries`（DSL 串）是**静态表面遗留通道**，按面分化：

- `ListUsers`：经 `ParseUserList`（`internal/domain/users/list.go`）白名单解析（equal/greaterThan/lessThan + 白名单属性）。
- storage buckets / files 与 groups（ListGroups/ListMemberships）：`rejectListQueries` 显式拒绝——携带即 InvalidArgument，不静默忽略。
- documents：`queries` 已 **reserved**（服务端零字符串解析）；`ListDocumentsRequest.query`（`shared.v1.Query`）是唯一过滤载体，与顶层 `page_size/page_token` 同名字段同时设置且不等 → InvalidArgument。

文档列表：GET 面保留 `page_size/page_token`（无过滤计数），过滤 / 排序 / 投影一律 `POST .../documents:list`，body 即 Query JSON：

```bash
curl -X POST -H 'X-API-Key: <key>' -H 'Content-Type: application/json' \
  -d '{"filter":{"eq":{"attribute":"status","values":["published"]}},"orders":[{"attribute":"$createdAt","desc":true}],"pageSize":20}' \
  'http://127.0.0.1:9080/v1/server/databases/app/collections/<collection_id>/documents:list'
# 响应 {documents:[...], meta:{page_size:20,next_page_token:"...",total_count:42}}
```

`distances`（与 documents 平行的第 i 项距离）仅 vector_search（KNN）查询时回传；普通查询不出现该字段。自定义动词惯例：`documents:list` / `:count` / `:bulkUpdate` / `:bulkDelete` / `:aggregate` / `documents:execute-tx` / `:exportSchema`——网关按路径路由的自定义方法段，不占用查询参数。

## 8. 步骤七：Wire 与注册

各层 `provides.go` 维护自己的 `ProviderSet`，汇总于 `cmd/server/provides.go`。repo 构造器直接返回 domain 接口（§4），无需逐一 Bind；需显式绑定的是组合根特有端口：

```go
// cmd/server/provides.go
wire.Bind(new(projects.SchemaManager), new(*projectschema.SchemaManager)) // 桥接 internalIDCache 失效回调
// internal/infra/provides.go（横切端口）
wire.Bind(new(uow.Runner), new(*clients.Database))
```

改构造器签名后 `mise run wire:all` 重生成**四份** `wire_gen.go`（server / worker / dispatcher / packer）。

**注册**：业务 proto 文件清单单一登记在 `cmd/server/internal/runtime/grpc.go` 的 `authzFileDescriptors()`（33 个文件：client 面 10 + server 面 20 + console 面 3，新增服务文件只登记此处）；`ProvideMethodPolicies` → `BuildMethodPolicies` 启动期收集策略并过语义断言，`assertRegisteredMethodsHaveAuthz` 对已注册方法 fail-closed。gateway 侧在 `cmd/server/internal/runtime/grpc_gateway.go` 登记 `RegisterXxxHandlerFromEndpoint`。

**授权矩阵**：`docs/developer/authz-matrix.md` 是生成物——`mise run gen:authz-matrix`（`go run ./cmd/server/internal/runtime/cmd/genauthzmatrix`）从 `PolicySet` 渲染；策略变更后重新生成，漂移由 `authz_matrix_doc_test.go` 字节级锁定（重渲染 ≠ 磁盘即红），勿手改。

## 9. 错误与网关映射

用例层常用码：`Unauthenticated / PermissionDenied / NotFound / InvalidArgument / AlreadyExists`；`FailedPrecondition / OutOfRange` 用于 version_* 与超限。

`cmd/server/internal/runtime/errors.go` 的 `HTTPErrorHandler` 统一转 JSON：

```json
{"error":{"type":"invalid_request_error","code":"InvalidArgument","message":"...","error_id":"<uuid>","error_code":"ERROR_CODE_INVALID_REQUEST"}}
```

- `type`（Stripe 风格）按码推导：InvalidArgument/FailedPrecondition/OutOfRange → `invalid_request_error`；Unauthenticated → `authentication_error`；PermissionDenied → `permission_error`；NotFound → `not_found_error`；AlreadyExists/Aborted → `conflict_error`；ResourceExhausted → `rate_limit_error`；其余 → `server_error`。
- **Internal/Unknown 统一脱敏**：对外 message 恒为 `internal server error`，原始消息只进日志（附 error_id 便于对账）——用例层不得依赖 Internal 传可读文案给客户端。
- 429 携带 `Retry-After`：从 status 的 `RetryInfo` detail 提取建议退避（整秒向上取整，至少 1s）。

| gRPC code | HTTP | error_code | type |
|---|---|---|---|
| InvalidArgument | 400 | ERROR_CODE_INVALID_REQUEST | invalid_request_error |
| FailedPrecondition | 400 | ERROR_CODE_PRECONDITION_FAILED | invalid_request_error |
| OutOfRange | 400 | ERROR_CODE_INTERNAL_ERROR（未单列，走默认） | invalid_request_error |
| Unauthenticated | 401 | ERROR_CODE_INVALID_CREDENTIALS | authentication_error |
| PermissionDenied | 403 | ERROR_CODE_PERMISSION_DENIED | permission_error |
| NotFound | 404 | ERROR_CODE_RESOURCE_NOT_FOUND | not_found_error |
| AlreadyExists | 409 | ERROR_CODE_RESOURCE_CONFLICT | conflict_error |
| Aborted | 409 | ERROR_CODE_CONCURRENT_MODIFICATION | conflict_error |
| ResourceExhausted | 429 | ERROR_CODE_QUOTA_EXCEEDED | rate_limit_error |
| DeadlineExceeded | 504 | ERROR_CODE_TIMEOUT | server_error |
| Internal / Unknown | 500 | ERROR_CODE_INTERNAL_ERROR | server_error（message 脱敏） |

## 10. OpenAPI 与一致性断言

每个服务 proto 文件声明 `openapiv2_swagger`：`security_definitions`（apiKey = X-API-Key、Bearer、cookie）、`security{主凭证}`（server 面选 apiKey、client 面选 Bearer）、`extensions{x-torchwood-access}`（等于服务默认 access）、`responses.default → shared.v1.ErrorResponse`。

`method_auth` 与 `x-torchwood-access` 必须一致：未显式声明的 operation 继承 swagger 顶层（服务默认）；与服务默认不同的档（典型为 SERVER 默认下的 `ACCESS_PERMISSION` 方法）必须逐方法声明 operation 级扩展；`ACCESS_PUBLIC` 方法另须 `security: []`（清空凭证要求，见 `proto/client/v1/runtime_vars.proto` GetRuntimeVars）。

**default 错误响应是声明式的**：`buf.gen.yaml` 对 openapiv2 插件设置 `disable_default_errors=true`，关闭生成器自带的 rpcStatus 注入（与运行时错误体不符）；每个 service proto 文件级声明的 `responses.default` 自动填充到该文件全部 operation。**新增 service 文件必须携带同一段 `responses.default`**，否则该文件的 operation 缺失错误契约，测试即红。

`cmd/server/internal/runtime/grpc_swagger_test.go` 逐 `genproto/**/*.swagger.json` 断言，清单复用 `authzFileDescriptors()` 单一来源：

1. **顶层扩展 = 服务默认 access**（`resolveServiceDefaultAccess` 推导）；
2. **每 operation 的有效 `x-torchwood-access`（显式或继承）与 `BuildMethodPolicies` 推导的 access 完全一致**；
3. **default 响应引用 `#/definitions/v1ErrorResponse`**（`disable_default_errors` 回退 / 漏声明 `responses.default` 即红）；
4. **反向覆盖率**：策略表登记的每个方法必须在 swagger paths 出现 ≥1 次（漏配 `google.api.http` 注解即红）；
5. **definitions 属性名全 snake_case**（`json_names_for_fields=false` 回退即红）；
6. **全量 swagger 无 rpcStatus 残留**。

新增服务后 file 清单同步一处即可（swagger 测试与启动期策略收集共用同一清单）。

## 11. OutboxService 示例（新增服务完整参照）

`proto/server/v1/outbox.proto`：服务默认 `ACCESS_SERVER`；两方法均 `admin_roles:[ADMIN_ROLE_ADMIN, ADMIN_ROLE_OWNER]`，ListDeadLetters 开 `api_key_scope` 读门（outbox:read）、ReplayDeadLetter 开写门（outbox:write）：

```proto
service OutboxService {
  option (torchwood.shared.v1.service_auth) = {default_access: ACCESS_SERVER};
  rpc ListDeadLetters(ListDeadLettersRequest) returns (ListDeadLettersResponse) {
    option (google.api.http) = {get: "/v1/server/outbox/dead-letters"};
    option (torchwood.shared.v1.method_auth) = {admin_roles: [ADMIN_ROLE_ADMIN, ADMIN_ROLE_OWNER]
                                                api_key_scope: {resource: SCOPE_RESOURCE_OUTBOX op: SCOPE_OP_READ}};
  }
  rpc ReplayDeadLetter(ReplayDeadLetterRequest) returns (ReplayDeadLetterResponse) {
    option (google.api.http) = {post: "/v1/server/outbox/dead-letters/{event_id}:replay" body: "*"};
    option (torchwood.shared.v1.method_auth) = {admin_roles: [ADMIN_ROLE_ADMIN, ADMIN_ROLE_OWNER]
                                                api_key_scope: {resource: SCOPE_RESOURCE_OUTBOX op: SCOPE_OP_WRITE}};
  }
}
```

步骤复盘（各层落点）：

1. proto 定义 + 文件级 swagger 块（§10）；
2. `mise run generate:proto`；
3. `internal/domain/events/outbox.go` 端口（`OutboxRepository.ListDeadLetters/ReplayDeadLetter`）；
4. `internal/app/events/outbox_admin.go` 用例：`RequireServerPrincipal` 二道防线 → 项目上下文必填（FailedPrecondition）→ 越权 NotFound 防枚举 → `ensureProjectActive`（项目存在且 active，5s 超时）；
5. `internal/infra/bun/bunrepo/outbox_repo.go` 适配：`crud.ParseListParams` + `BuildPaginationInfo` + `EncodePageToken`（list 5s 超时；replay 走 `RunInTx` 10s 超时）；
6. `internal/api/servergrpc/outbox.go` handler：项目寻址一律来自凭证（API key = 密钥行绑定，admin = `X-Torchwood-Project` 头，缺失即 FailedPrecondition；请求体不做寻址回退——项目寻址不变量）；`ReplayDeadLetterRequest.event_id` 的 required 由 buf.validate 承担（§2.3）；
7. `grpc.go` 的 `authzFileDescriptors()` 登记 + `grpc_gateway.go` 注册 handler；
8. `mise run wire:all`，随后 `mise run gen:authz-matrix` 刷新授权矩阵。

CLI 调用验证：`torchwood outbox list-dead`（项目上下文取凭证）或逃生舱 `torchwood rpc /torchwood.server.v1.OutboxService/ListDeadLetters --data '{"pageSize":20}'`（`sdk/go/server.InvokeJSON` 动态分发，camelCase 字段）。

## 12. 审计日志

写入侧在 gRPC 审计拦截器（`internal/api/interceptor/audit.go`，`auditRowEligible` 噪声治理准入门）：server / console 面规则是"非读动词默认落审计、豁免必须显式登记"——`auditSilentServerMethods` 显式静默清单（首例 AnalyticsService/IngestEvents），新增高频写方法需同步该清单（护栏测试同步）；client 面仅 AccountService 安全动作落库；框架服务（health/reflection）不记；拒绝与限速审计不经此门、全部保留。交付语义为 **best-effort**（`internal/api/interceptor` 包级注释）：审计行在业务提交后落库（3s 超时、不重试、不落死信），失败仅记 Warn——审计查询结果不承诺与业务操作一一对应，窗口内崩溃可能丢行。

`AuditLogsService`（`proto/server/v1/audit_logs.proto`）只提供 `ListAuditLogs` 一个读取方法：

- **鉴权**：`admin_roles:[ADMIN,OWNER]` + `audit_logs.read` scope。项目上下文来自凭证（admin 需 `X-Torchwood-Project`，否则 FailedPrecondition）；`include_platform`（并入平台级行）与 `all_projects`（跨项目视图）仅平台 admin。
- **形状校验全在 proto**：`page_size ∈ [0,1000]`、`page_token ≤4096`、各过滤字段 max_len、时间闭区间 `created_after/created_before`（§2.3）。
- **结构化 metadata（机器可读）**：`client`（调用通道 cli/sdk/console/function/api + product/version——CLI / SDK 经 `WithUserAgent("torchwood-cli/<ver>")` 注入）；`request`（管理面非读方法的脱敏请求摘要：protojson presence 语义使更新类请求只含被改字段；敏感字段名打码 `[REDACTED]`、超长截断、整体 ≤8KB）；`changes`（app 用例经 `contexts.SetAuditMetadata` 回填的 `{"字段":{from,to}}` diff）。
- 查询：结构化过滤（actor_id / actor_kind / action / status / resource_id 精确匹配 + 时间闭区间）+ `pkg/crud` offset 分页；索引见迁移 000008。
- 消费端：Console `/console/audit-logs`；CLI `torchwood audit-logs list`。

## 13. 自检清单

1. `mise run generate:proto && go build ./...` 通过，`genproto/` 无手改；
2. `mise run wire:all` 已重生成四份 `wire_gen.go`；
3. `mise run lint` 干净（go vet + gofmt + golangci-lint）；
4. `mise run gen:authz-matrix` 已执行、矩阵无漂移（字节级门禁）；
5. 错误码与分页符合 §7 / §9；bun UPDATE 已显式声明列（`update_guard_test` 静态扫描）；
6. swagger 一致性测试通过（§10 六项断言）；
7. 集成测试参照 `internal/api/servergrpc/projects_test.go`（stub repo + `contexts.WithPrincipal`）与 `internal/pkg/testutil` 真库。

## 相关文档

- `05-authentication.md` — authz 注解语义、scope 语法与策略注册表
- `06-databases.md` — 三层 schema 与 `pkg/query` typed AST
- `04-codegen.md` — 生成流程与漂移门禁
- `17-update-write-guard.md` — bun 更新写规范与存量漂移修复 runbook
- `authz-matrix.md` — 全方法授权矩阵（生成物，勿手改）
- `sdk/README.md` — SDK 侧方法映射（`InvokeJSON` 动态分发）
