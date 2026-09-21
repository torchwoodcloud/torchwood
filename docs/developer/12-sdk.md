# 12 SDK 指南

覆盖 TypeScript SDK（`sdk/typescript/`，`@torchwood/sdk`）与 Go SDK（独立 module `github.com/torchwoodcloud/torchwood/sdk/go`，子包 `client` / `server` / `query` / `functions` / `docexamples`，内部包 `internal/conn`）。符号与签名以源码为准：TS 门面见 `sdk/typescript/src/torchwood.ts`，Go Server 见 `sdk/go/server/client.go`，Go Client 见 `sdk/go/client/client.go`。

> 关联：`09-api-guide.md`（API 约定）、`14-agent-tools.md`（Agent 工具箱）、`sdk/README.md`（总览、版本策略与兼容承诺）。

## 1. 定位与选型

SDK 是 Torchwood **AI/Agent-Native** 定位的集成层（`sdk/README.md`）：API 面由 Protobuf + OpenAPI 定义、可机器读取，LLM Agent、自动化脚本与 MCP Tool Server 按下表选型。

| 场景 | 推荐 | 说明 |
|------|------|------|
| 管理面自动化（用户 / 文档 / 存储 / 变量） | **Server API** + API Key | Console 或 `POST /v1/server/api-keys` 创建带 scope 的 Key；scope 词表见 §4.3 |
| 终端用户身份流 | **Client API** + JWT | TS 走 `fetch`，Go 走 TokenStore 自动刷新（§4.2） |
| 函数内执行身份（TS） | **`Torchwood.fromExecution(apiBaseUrl, opts?)`** | 以 execution principal 构造 client，方法面 = server 服务类全量；token 优先级 = 显式 `opts.executionToken` > `TW_EXECUTION_TOKEN`，皆缺抛错（fail-closed）；并发函数必须显式传 token（env 通道仅同步段读取安全，见 `08-functions.md` §4.2/§4.3.1） |
| 函数内回访平台（Go） | **`sdk/go/functions`** | Go 函数运行时 SDK：`FromContext(ctx)` 取执行身份、`NewClient(ctx)` 零配置构造平台 API 客户端（§4.6） |
| Agent 默认工具箱 | **overlay 18 动词** | TS `TOOL_*` / `agentTools` 与 Go `Tool*` / `Tools` 映射到现有 Server RPC（不新增服务、不含 API key 管理），见 `14-agent-tools.md` |
| 逃生舱 | **Go `InvokeJSON`** | `sdk/go/server` 的 `Client.InvokeJSON(fullMethod, protojson)` 覆盖全部 `torchwood.server.v1.*` unary（排除 APIKeysService），proto 新增方法零登记自动可用 |

## 2. 包结构与构建

### 2.1 目录

| 路径 | 说明 |
|------|------|
| `sdk/typescript/` | `@torchwood/sdk`，`type: module`，`main`/`types` → `dist/`，engines node ≥18，零运行时依赖（devDependencies 仅 `typescript`） |
| `sdk/typescript/src/torchwood.ts` | `Torchwood` 门面：11 个 Client 服务字段 + 20 个 Server 服务字段 + `fromExecution` |
| `sdk/typescript/src/http.ts` | `HttpTransport` + `TorchwoodConfig` + `AuthMode` 四态 + `DEFAULT_TIMEOUT_MS`（30000） |
| `sdk/typescript/src/server/` | 20 个 Server 服务类；`tools.ts` 为 18 动词工具目录 |
| `sdk/typescript/src/client/` | 11 个 Client 服务类（account / analytics / assets / databases / functions / groups / leaderboards / payments / realtime / runtimeVars / subscriptions） |
| `sdk/typescript/src/index.ts` | 包根导出（`AnalyticsEventBuffer`、typed query 构造器、`agentTools`、realtime 类型） |
| `sdk/typescript/src/errors.ts` | `TorchwoodError` 与错误信封解析 |
| `sdk/typescript/src/query.ts` | TS typed AST 查询构造器（`eq` / `gt` / `containsAny` / `and` / `vectorSearch` / …） |
| `sdk/typescript/src/types.ts` | 手写类型（非 proto 生成，snake_case） |
| `sdk/typescript/src/__tests__/` | node:test 测试（contract / http / analytics / realtime / tools 等） |
| `sdk/go/client/` | end-user 客户端（Bearer JWT 自动刷新、TokenStore、`ConnectRealtime`） |
| `sdk/go/server/` | 管理面客户端（`x-api-key`、19 个服务 wrapper、`InvokeJSON`、`Tools`、scope 词表常量、错误分类 helper） |
| `sdk/go/query/` | typed AST 构造器 + 链式 Builder + `FromDSL`（golden 用例与 `pkg/query` 锁对齐） |
| `sdk/go/functions/` | Go 函数运行时 SDK（用户持有 main；`StartInvoke` / `StartCron` / `StartEvent` / `StartHTTP` / `Mux`） |
| `sdk/go/docexamples/` | build tag `docexample`：承载本文档与 `sdk/README.md` 的 Go 示例，与文档一一对应（仅编译期校验） |
| `sdk/go/internal/conn/` | 拨号封装：默认 insecure、30s 兜底超时、UNAVAILABLE 重试 service config、`MaxCallRecvMsgSize` 8MiB（与服务端对齐） |
| `sdk/demo/` | Vite 演示站（端口 5174） |

### 2.2 构建与安装

```bash
mise run sdk:install   # sdk/typescript + sdk/demo 各 npm install
mise run sdk:build     # tsc → dist/
mise run sdk:demo      # vite dev（http://localhost:5174）
mise run sdk:publish   # 发布 npm
mise run test:sdk-go   # sdk/go 全量 go test（bufconn 内存 gRPC，无外部依赖）
mise run test:sdk-ts   # npm ci + node --test
```

- 外部用户直接 `npm install @torchwood/sdk`；TS HTTP 走全局 `fetch`（`TorchwoodConfig.fetch` 可注入）。
- Go SDK 为独立 module，`go.mod` 以 `replace` 指向仓库内 `../../genproto`；`mise run test` / `mise run lint` 已纳入 sdk 测试与 vet。

## 3. TypeScript SDK

### 3.1 配置与传输层

```ts
interface TorchwoodConfig {
  endpoint: string;        // 如 http://localhost:9080
  projectId?: string;      // apiKey 模式经 X-Torchwood-Project 发送；execution 模式可省略
  apiKey?: string;         // Server API（X-Api-Key + X-Torchwood-Project）
  accessToken?: string;    // Client API（Authorization: Bearer）
  executionToken?: string; // 函数执行身份短期凭证（优先经 fromExecution 注入）
  timeoutMs?: number;      // 单请求超时（毫秒）；缺省 30000，显式 0 = 禁用
  fetch?: typeof fetch;    // 可选注入
}
```

- **超时**：缺省 / 显式 `undefined` = 30000ms（`DEFAULT_TIMEOUT_MS`，与 Go SDK `conn.DefaultTimeout` 对齐）；仅显式 `0` 禁用——`undefined` 不作"禁用"解，保证配置展开合并不会意外关掉超时。超时中止转译为 `TorchwoodError`（`code: "timeout"`）；主路径 `AbortSignal.timeout`，缺失该 API 的老运行时降级 AbortController + setTimeout。
- **`AuthMode` 四态**：`"apiKey" | "user" | "execution" | "none"`。**execution 切换**：transport 配置了 executionToken 时，服务类硬编码的 `auth:"apiKey"` 请求自动改走执行身份 Bearer（项目绑定在 token 内，无需 X-Torchwood-Project）——`fromExecution` 返回的 client 复用同一批 server 服务类即全量可用；`"user"` 模式不做该切换（终端用户语义不适用执行身份）。
- **fail-closed**：apiKey / execution 模式凭证缺失直接抛 `TorchwoodError`，不发请求。
- **空体规范化**：`204` / `content-length: 0` / 空文本一律返回 `undefined`，不做 JSON 解析。`request()` 先判空体后判 `!ok`（content-length 为 0 的非 2xx 响应返回 undefined）；`requestForm()`（multipart，如 storage `uploadFile`）先判 `!ok`（非 2xx 一律抛错）。
- 非 2xx 抛 `TorchwoodError`（§5）。

| 成员 | 说明 |
|------|------|
| `new Torchwood(config)` / `Torchwood.create(config)` | 直接构造 / 静态工厂 |
| `Torchwood.withApiKey(ep, pid, key)` | Server API 工厂 |
| `Torchwood.withAccessToken(ep, pid, token)` | Client API 工厂 |
| `Torchwood.fromExecution(apiBaseUrl, opts?)` | 函数内执行身份工厂（见 §1；token 优先级 `opts.executionToken` > `TW_EXECUTION_TOKEN`，皆缺抛错） |
| `setAccessToken / getAccessToken / getProjectId` | 访存 / 清理 token |
| `setExecutionToken / getExecutionToken` | 执行身份 token |

### 3.2 门面入口

```ts
export class Torchwood {
  readonly account: AccountService;                   // Client
  readonly analytics: ClientAnalyticsService;         // ingest（+ 包根 AnalyticsEventBuffer）
  readonly databases: ClientDatabasesService;
  readonly groups: ClientGroupsService;
  readonly realtime: RealtimeService;
  readonly payments: ClientPaymentsService;
  readonly assets: ClientAssetsService;
  readonly leaderboards: ClientLeaderboardsService;
  readonly subscriptions: ClientSubscriptionsService;
  readonly functions: ClientFunctionsService;         // invokeFunction
  readonly runtimeVars: ClientRuntimeVarsService;     // getRuntimeVars（etag 短路）
  readonly server: { /* 20 个 Server 服务，见 §3.3 */ };
  static fromExecution(apiBaseUrl: string, opts?): Torchwood;
}
```

### 3.3 Server 20 服务

| 字段 | TS 类 | 说明 |
|------|-------|------|
| `health` | `HealthService` | `check()`（匿名）/ `getVersion()` |
| `projects` | `ProjectsService` | 项目 list / get / create / update |
| `users` | `UsersService` | 用户 CRUD / updatePassword / 会话管理 / `createToken`（模拟登录） |
| `groups` | `ServerGroupsService` | 组 CRUD + membership |
| `databases` | `ServerDatabasesService` | 库 / 集合（含 `exportCollectionSchema`）/ 属性（含 restore / retire / migrate）/ 索引 / 文档 CRUD、bulk、`countDocuments`、`aggregateDocuments`、`listChanges`、`executeTransactions` |
| `apiKeys` | `APIKeysService` | list / get / create（`{api_key, secret}` 仅一次）/ update / delete / `whoAmI`；服务端禁止 API Key 凭证调用本服务（管理走 Console / admin session，`InvokeJSON` 亦排除） |
| `oauthProviders` | `OAuthProvidersService` | 提供商 list / upsert / delete |
| `storage` | `StorageService` | bucket / file 元数据管理、`createFileToken`、`getStorageUsage`；`uploadFile` 走 multipart |
| `functions` | `FunctionsService` | 运行时 / 规格 / 函数 CRUD / 部署 / 变量 / `setScopes` / 执行 / 触发器（create / list / delete / rotateTriggerToken） |
| `payments` | `ServerPaymentsService` | 订单查询 / 退款 / 人工履约 |
| `assets` | `ServerAssetsService` | 资产定义 CRUD + 授信动词（终端用户无写入口） |
| `leaderboards` | `ServerLeaderboardsService` | 提交 / 条目 / top / 结算查询（可代任意 subject；`leaderboards.write`/`read`，见 `20-leaderboards.md`） |
| `subscriptions` | `ServerSubscriptionsService` | 计划 CRUD + 强制取消 / 过期 |
| `billing` | `BillingService` | 用量 / rollup / 账单（只读） |
| `outbox` | `OutboxService` | 死信查询与重放（`outbox:read` / `outbox:write`，`owner|admin`；重放幂等） |
| `auditLogs` | `AuditLogsService` | 审计日志查询（admin/owner + `audit_logs.read`） |
| `analytics` | `AnalyticsService` | ingest（可信代报 user_id）+ getOverview / 事件定义 / timeseries / breakdown / retention / 用户事件查询（`18-analytics.md`） |
| `auth` | `AuthService` | `verifyToken`：对外凭证校验（token introspection）；校验失败返回 `valid=false` 而非错误 |
| `runbooks` | `RunbookService` | 版本化资源迁移状态面：getState / recordStep（CAS）/ deleteStep（见 `19-runbook.md`） |
| `runtimeVars` | `RuntimeVarsService` | 运行时变量集合 / 变量 / 版本链管理（三组 13 方法；int64 字段已归一为 number） |

### 3.4 Client API

- 鉴权：`HttpTransport.request(..., {auth:"user"|"none"})`；有 token 时 `Authorization: Bearer`，无则匿名；`auth:"none"` 不带头（sign-up / health 等）。
- Account：signUp / signIn / refresh / OTP / OAuth / MFA / 匿名会话等成功后自动 `setAccessToken`。
- Databases（签名 `(databaseId, collectionId, ...)`）：`createDocument` / `listDocuments` / `getDocument` / `updateDocument` / `upsertDocument` / `deleteDocument`（version 必填，OCC）/ `countDocuments`；带 typed query（`QueryAst`，§3.1 构造器）时走 POST `:list` / `:count`，无 query 走 GET 简单分页；`listChanges` 拉取已提交事件流（`since_seq` 续传，游标出窗报 `EVENTS.RESUME_EXPIRED`）。
- Functions：`tw.functions.invokeFunction(functionId, input?)`。
- Leaderboards：`submitLeaderboardScore` / `getMyLeaderboardEntry` / `listLeaderboardTop`。
- Analytics：`tw.analytics.ingest(events)` → POST `/v1/analytics/events`；归因（user_id / session_id）由服务端从 principal 落定（含匿名会话），部分接收语义 `{accepted, skipped}`。
- RuntimeVars：`tw.runtimeVars.getRuntimeVars(varSetId, {projectId?, etag?})` 按集合全量快照；public 集匿名可读；etag 命中返回 `unchanged=true`（SDK 不内置 TTL 缓存，轮询口径见源码 JSDoc）。
- **Realtime**：`tw.realtime.connect({projectId?, getAccessToken?})` → WebSocket `/v1/realtime`；hello 携带 access_token（Console 同源 cookie 场景可省略）、subscribe/unsubscribe 帧、服务端 30s ping；`token_expired` 断线后重新调 `getAccessToken` 拿新 token 重连并重订全部频道，不补历史；指数退避（默认 500ms 起、上限 10s）；`accountsChannel(userId)` 为经济事件单一频道。
- **`AnalyticsEventBuffer`**（包根导出，独立生命周期、不在门面自动装配）：size / time 双阈值 flush（`maxBatchSize` 默认 20、按服务端单批上限 100 钳制；`flushIntervalMs` 默认 10s）；浏览器环境自动注册 `visibilitychange`(hidden) / `beforeunload` 尽力 flush；`track` / `flush` 永不抛错——失败按指数退避重试（默认 500ms 基数、最多 2 次；408/429/5xx 与网络错误可重试），耗尽后该批静默丢弃；重试与补发带来 at-least-once。各端（小游戏 / 原生）接入配方见 `18-analytics.md`。
- 传输：fetch + JSON；空体规范化见 §3.1。

## 4. Go SDK

### 4.1 总览

| 包 | 认证 | 服务 |
|----|------|------|
| `sdk/go/client` | `Authorization: Bearer <JWT>`（TokenStore 自动刷新） | Account / Groups / Databases（UseDatabase 绑定）/ Payments / Assets / Subscriptions / Functions（InvokeFunction / InvokeString）/ RuntimeVars（GetRuntimeVars 全量快照 + 类型化取值 helper）共 8 个；Realtime 经 `ConnectRealtime(ctx, httpEndpoint, opts...)` |
| `sdk/go/server` | `x-api-key` + `x-torchwood-project` | Health / Auth / Users / Groups / Databases / Projects / Storage / Functions / OAuthProviders / Payments / Assets / Leaderboards / Subscriptions / Billing / Outbox / AuditLogs / Analytics / Runbook / RuntimeVars 共 19 个 + `InvokeJSON` + `InvokeTool` |

Options：

- Server：`WithAPIKey` / `WithProjectID` / `WithDatabaseID` / `WithDialOptions` / `WithTLS`（系统根证书，TLS ≥1.2）/ `WithTimeout` / `WithRetryDisabled` / `WithUserAgent`（服务端审计据此把 api_key 调用归类 cli / sdk / api 通道）。
- Client：`WithProjectID` / `WithDatabaseID` / `WithTokenStore` / `WithOnTokensChanged` / `WithInitialTokens` / `WithDialOptions` / `WithTimeout` / `WithRetryDisabled`。
- 两包均有 `UseDatabase(id)` 返回绑定库的 Databases 副本；默认 insecure，生产用 `WithTLS` 或自定义 transport credentials。
- 连接级行为由 `internal/conn` 统一：单次调用 30s 兜底超时（`WithTimeout` 调整；调用方 ctx 已带 deadline 时原样尊重）；默认对 UNAVAILABLE 自动重试（最多 4 次，0.2s 起指数退避 ×1.6、上限 5s），`WithRetryDisabled` 可关闭。
- Go Realtime：断线后强制刷新 token、退避重连并带 last_seq 重订全部频道（窗口内事件由服务端补发）；慢水位 close（`resync:<seq>`）零退避立即重连；游标出窗触发 `WithRealtimeResumeExpired` 回调；`LastSeq(channel)` 查询游标。

### 4.2 自动刷新与 FileTokenStore

`TokenStore` 接口：`Load` / `Save` / `Clear`；内置 `MemoryTokenStore` 与 `FileTokenStore`（protojson 格式、0600 权限、临时文件 + rename 原子写（Windows 先移除旧文件再 rename）、`~` / `~/` / `~\` 自动展开、Save 自动建父目录 0700、内置 mutex 并发安全）。

刷新（unary interceptor，对全部调用透明）：

1. **主动**：`expires_at` 距 now 不足 30s 且有 refresh token 时先刷新（互斥串行 + double-check）；
2. **被动**：返回 Unauthenticated 时，若本地 token 未被其他 goroutine 刷新过则刷新一次并重试；
3. 仅当刷新以 Unauthenticated 失败（refresh token 失效）才清空本地 token，临时错误保留；
4. 公开方法（sign-up / OTP / recovery / client 面 ListDocuments / GetDocument / CountDocuments 等 `noRefreshMethods` 集合）与 SignOut 不经刷新拦截；
5. `OnTokensChanged` 在登录 / 刷新 / 清空时回调；SignIn / SignUp 仅非 MFA 且 access token 非空时落盘；SignOut 成功或 Unauthenticated 均清空本地 token。

### 4.3 InvokeJSON、Agent 工具箱与 scope 词表

```go
respJSON, err := c.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/CreateUser", reqJSON)
```

- `findServerMethod` 按 `protoregistry.GlobalFiles` 查 MethodDescriptor，限定 `torchwood.server.v1.*` 且排除 APIKeysService（服务端禁止 API Key 凭证调用）——proto 新增方法自动可用；`reqJSON` 为 protojson（未知字段报错），空请求等价 `{}`；响应为 2 空格缩进 protojson（与 CLI 输出同实现）；未知方法报 `torchwood: unknown method`。
- **覆盖完整性由 `sdk/go/server` 反射测试保证**（`invoke_test.go`）：`TestInvokeJSONCompleteness` 遍历 registry 断言 server 包全部方法（快照 163 个，排除 APIKeysService）可经 `findServerMethod` 解析；`TestTypedWrappersCoverAllServerMethods` 反射断言每个 RPC 在 Client 对应 wrapper 上有同名词导出方法——InvokeJSON 面零登记自动覆盖新 RPC，类型化 wrapper 面由测试强制同步补齐。
- Agent 工具箱：`server.Tools` / `LookupTool` / `Client.InvokeTool`（18 条工具目录，工具名常量 `Tool*`），TS 对等 `agentTools` / `lookupAgentTool` / `TOOL_*`；每条工具的 `InputSchema` 由 input descriptor 自动生成 JSON Schema（type: object）。
- scope 词表常量镜像在 `sdk/go/server/scopes.go`：`*` / `all` 全量 + 17 个资源族（databases / users / groups / storage / projects / oauthproviders / functions / payments / assets / subscriptions / billing / outbox / audit_logs / leaderboards / analytics / runbooks / runtime_vars）的裸资源与 `.read` / `.write` 方向（leaderboards 另有 `.admin`；裸资源名放行该资源全部方向）。scope 只对 Server 面方法生效；语义与服务端 `PolicySet.AllowsAPIKey` 同源。
- 错误 helper（`errors.go`）：`ErrorCode` / `IsPermissionDenied` / `IsUnauthenticated` / `HTTPErrorClass`（0 成功 / 2 客户端错 / 3 服务端错 / 4 限流，对齐 CLI 退出码契约）/ `ExtractRetryAfter`（从 RetryInfo detail 读限流退避秒数）。

### 4.4 查询构造器（sdk/go/query）

- 叶子构造器产出 `*sharedv1.Filter`：`Eq` / `Ne` / `Lt` / `Lte` / `Gt` / `Gte` / `In` / `Contains` / `ContainsAny` / `ContainsAll` / `StartsWith` / `EndsWith` / `Search` / `Between` / `IsNull` / …，组合用 `And` / `Or`。
- `query.New()` 链式 Builder（`Filter` / `VectorSearch` / `OrderAsc` / `OrderDesc` / `Select` / `PageSize` / `PageToken`）产出 `*sharedv1.Query`；向量近邻用 `query.VectorSearch(attr, values...).MetricCosine().EfSearch(n).Build()`。
- `query.FromDSL(parts...)` 在**客户端**把 DSL 串解析为 AST 后发送（服务端零字符串解析）；文法与仓库根 `pkg/query` 同源，golden 用例锁对齐。
- 文档面收 typed AST（`*sharedv1.Query`）；`ListDatabases` 等静态面仍走 `queries` 串参数。

### 4.5 典型用法

```go
import (
	"context"

	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/sdk/go/client"
	"github.com/torchwoodcloud/torchwood/sdk/go/query"
	"github.com/torchwoodcloud/torchwood/sdk/go/server"
)

// Server API（管理面）
srv, _ := server.New("127.0.0.1:9060",
	server.WithAPIKey(os.Getenv("TORCHWOOD_API_KEY")),
	server.WithDatabaseID("app"))
user, _ := srv.Users.CreateUser(ctx, "agent@example.com", "Pass@123", "Agent", "active", nil, nil)
tok, _ := srv.Users.CreateUserToken(ctx, user.Id)
doc, _ := srv.Databases.UpsertDocument(ctx, "members", "m1",
	map[string]any{"channel_id": "ch1"}, []string{"channel_id", "user_id"}, nil)
n, _ := srv.Databases.CountDocuments(ctx, "messages",
	&sharedv1.Query{Filter: query.Eq("channel_id", "ch1")})
raw, _ := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{"pageSize":10}`))
letters, _ := srv.Outbox.ListDeadLetters(ctx, &serverv1.ListDeadLettersRequest{ProjectId: "app", PageSize: 20})
_, _, _, _, _ = tok, doc, n, raw, letters

// Client API（自动刷新）
store := client.NewFileTokenStore("~/.torchwood/tokens.json")
c, _ := client.New("127.0.0.1:9060", client.WithProjectID("app"), client.WithTokenStore(store))
_, _ = c.Account.SignIn(ctx, "u@example.com", "Pass@123")
me, _ := c.Account.Me(ctx)
_ = me
```

- 错误：`status.Code(err)` 判 `codes.NotFound` / `PermissionDenied` 等（或用 §4.3 的 helper）；限流响应可用 `server.ExtractRetryAfter(err)` 读出建议退避秒数。
- 文档：入参 `map[string]any` → structpb，读回数值多为 `float64`。
- 示例可编译性：Go 示例以 `sdk/go/docexamples`（build tag `docexample`）镜像，与本文档一一对应；`go vet -tags docexample ./sdk/...` 校验。

### 4.6 Go 函数运行时 SDK（sdk/go/functions）

- 模型：用户持有 main + SDK——用户代码编译为根 main 包，经本 SDK 承接平台执行协议（:18080 runner 契约的自包含参考实现，仅依赖标准库 + genproto，不 import internal）。
- 入口：`StartInvoke[Req, Resp]`（TW_DATA → 类型化请求）/ `StartCron` / `StartEvent` / `StartHTTP`（还原真 `*http.Request`）以 main 风格启动并阻塞；多触发源用 `NewMux()` + `Invoke` / `Cron` / `Event` / `Fetch` + `Listen(mux)`。
- 执行身份：`FromContext(ctx)` 取身份（executionToken / apiBaseUrl / executionId 等）；`NewClient(ctx)` 零配置构造平台 API 客户端（`Authorization: Bearer twx_…` + `TW_API_BASE_URL`，非 2xx 返回 `*APIError`）。
- 运行契约与协议演进宪法（未知 `x-tw-*` header 一律忽略）见 `08-functions.md` §3.1 与包文档。

### 4.7 CLI 边界

- `cmd/torchwood`（实现随仓库根 `cli/` 包）**仅依赖 `sdk/go/server`** 的 `InvokeJSON` 与错误 helper，源码不直接 import genproto / grpc / protobuf；`cli/import_guard_test.go` 递归扫描 `cli/` 与 `cmd/torchwood/` 全部非测试 Go 文件（含嵌套子包），命中禁用 import 前缀即失败。
- 方法覆盖完整性由 `sdk/go/server` 反射测试保证（§4.3）：新增 RPC 无需在 CLI 登记即自动可用。
- runbook 命令组（`cli/runbook.go`）：生产 RPC 通道适配为 `internal/pkg/runbook.Caller`；一次复合动作（up / down / status / forgive）的全部 RPC 贯穿同一条 `server.Client`（`server.New` 一次，结束 cleanup 关闭），不做逐条 RPC 一建一断。

## 5. 错误与类型

- TS：`TorchwoodError { status, code?, body? }`，解析 `{error:{message,code}}` 信封；无内置重试；请求超时 `code: "timeout"`（§3.1）。
- Go：gRPC `status` 错误（§4.3 / §4.5）。
- TS 类型为手写（`src/types.ts`），字段 snake_case 与 HTTP JSON 一致（网关 protojson `UseProtoNames: true` + `EmitUnpopulated: false`，见 `cmd/server/internal/runtime/errors.go`），时间一律 RFC3339；以 `genproto/**/*.swagger.json` 为权威契约（TS 合同测试锁形状）。
- SDK demo：`sdk/demo`（Vite，端口 5174），`VITE_TORCHWOOD_ENDPOINT` / `VITE_TORCHWOOD_PROJECT_ID` 覆盖默认（`sdk/demo/.env.example`），设置页填 API Key 后可跑通 Server / Client 全链路。

## 相关文档

- `09-api-guide.md` — API 约定与 OpenAPI 建模
- `14-agent-tools.md` — Agent 18 动词工具箱
- `18-analytics.md` — Analytics 端接入配方（`AnalyticsEventBuffer` 各端型态）
- `19-runbook.md` — runbook 引擎与 CLI 命令（Server 状态面见 §3.3）
- `20-leaderboards.md` — 排行榜语义
- `08-functions.md` §4.2/§4.3 — 函数执行身份与执行器（`fromExecution` 的上下文来源）
