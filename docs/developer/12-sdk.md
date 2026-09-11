# Torchwood SDK 指南

> 覆盖 TypeScript `@torchwood/sdk`（`sdk/typescript/`，本文按 **v0.6.0** 复核，见 `CHANGELOG.md`；Go SDK 最新 `sdk/go v0.2.0`）与 Go 双 SDK（`sdk/go/client`、`sdk/go/server`）。符号与签名以源码为准：TS 门面见 `sdk/typescript/src/torchwood.ts:50`，Go Server 见 `sdk/go/server/client.go`，Go Client 见 `sdk/go/client/client.go`。
> 关联：`docs/developer/09-api-guide.md`（API 约定）、`docs/developer/14-agent-tools.md`（Agent 工具箱）、`sdk/README.md`。
> 修订记录：2026-08-23 重写；2026-09-12 按 v0.6.0 复核（补 v0.4.0 `fromExecution` 执行身份入口与 v0.6.0 Leaderboards/Analytics 双面 + `AnalyticsEventBuffer`；Server 服务 13→17、Go server 13→16）。

---

## 1. 定位

| 场景 | 推荐 | 说明 |
|------|------|------|
| 管理面自动化（用户/文档/存储） | **Server API** + API Key | Console 或 `POST /v1/server/api-keys` 创建带 scope 的 Key |
| 终端用户身份流 | **Client API** + JWT | TS 走 `fetch`，Go 走 `FileTokenStore` 自动刷新 |
| 函数内执行身份 | **`Torchwood.fromExecution(apiBaseUrl, opts?)`** | v0.4.0：以 execution principal 构造 client（方法面 = server 服务类全量）；token 优先级 = 显式 `opts.executionToken` > `TW_EXECUTION_TOKEN`，皆缺 fail-closed 抛错（`torchwood.ts:141`，`sdk/typescript/README.md`「函数内使用」） |
| Agent 默认工具箱 | **E-7 overlay 18 动词** | `TOOL_*`/`agentTools`（TS）与 `Tool*`/`Tools`（Go）映射到现有 Server RPC，见 [14-agent-tools.md](14-agent-tools.md) |
| 逃生舱 | **Go `InvokeJSON`** | `sdk/go/server:20` 的 `InvokeJSON(fullMethod, protojson)` 覆盖全部 `torchwood.server.v1.*` unary（排除 `APIKeysService`） |

---

## 2. 包结构与构建

### 2.1 目录

| 路径 | 说明 |
|------|------|
| `sdk/typescript/` | `@torchwood/sdk`，`type: module`，`main` → `dist/index.js` |
| `sdk/typescript/src/torchwood.ts` | `Torchwood` 门面（`export class Torchwood` 在 `:50`；`fromExecution` 在 `:141`） |
| `sdk/typescript/src/http.ts` | `HttpTransport` + `TorchwoodConfig`（authMode 含 `"execution"`） |
| `sdk/typescript/src/server/` | 17 个 Server 服务（见 §3.3）；另有 `tools.ts` 为 18 动词工具目录 |
| `sdk/typescript/src/client/` | Client 服务（Account / Databases / Groups / Payments / Assets / Subscriptions / Realtime / Functions / Leaderboards / Analytics） |
| `sdk/typescript/src/index.ts` | 包根导出（含 `AnalyticsEventBuffer`、typed query 构造器 re-export） |
| `sdk/typescript/src/errors.ts` | `TorchwoodError` |
| `sdk/typescript/src/query.ts` | TS typed AST 查询构造器（`eq`/`gt`/`contains`/…，包根导出） |
| `sdk/typescript/src/types.ts` | 手写类型（非 proto 生成） |
| `sdk/go/client/` | end-user 客户端（Bearer JWT + 自动刷新） |
| `sdk/go/server/` | 管理面客户端（`x-api-key` + `InvokeJSON` + `Tools`） |
| `sdk/go/internal/conn/` | 拨号封装 |
| `sdk/demo/` | Vite 演示站（`task sdk:demo`，端口 5174） |

### 2.2 构建

```bash
task sdk:install   # sdk/typescript + sdk/demo 各 npm install
task sdk:build     # sdk/typescript: tsc -p tsconfig.build.json → dist/（不含 __tests__）
task sdk:demo      # 依赖 sdk:build 后 vite dev（http://localhost:5174）
```

- 对外用户直接安装已发布包：`npm install @torchwood/sdk`（npm 公共 registry，发布走 `task sdk:publish`）；
- TS SDK 零运行时依赖，仅 `typescript`（dev），HTTP 走全局 `fetch`（`TorchwoodConfig.fetch` 可注入）；
- Go SDK 为独立 module：`github.com/torchwoodcloud/torchwood/sdk/go`（`require` + `replace` 本地开发）。

---

## 3. TypeScript SDK（`@torchwood/sdk`）

### 3.1 配置与工厂

```ts
import { Torchwood, TorchwoodError } from "@torchwood/sdk";
import type { TorchwoodConfig } from "@torchwood/sdk";

interface TorchwoodConfig {
  endpoint: string;      // 如 http://localhost:9080
  projectId?: string;    // 如 "default"（可选：execution 模式可省略）
  apiKey?: string;       // Server API（X-Api-Key + X-Torchwood-Project）
  accessToken?: string;  // Client API（Authorization: Bearer）
  executionToken?: string; // 函数执行身份（Authorization: Bearer <executionToken>，v0.4.0）
  fetch?: typeof fetch;  // 可选注入
}
```

`AuthMode` 四态：`"apiKey" | "user" | "execution" | "none"`（`src/http.ts:16-30`）。

| 成员 | 签名 | 说明 |
|------|------|------|
| `new Torchwood(config)` | 构造器 | 直接构造 |
| `Torchwood.create(config)` | 静态工厂 | 等价构造器 |
| `Torchwood.withApiKey(ep, pid, key)` | 静态工厂 | Server API |
| `Torchwood.withAccessToken(ep, pid, token)` | 静态工厂 | Client API |
| `setAccessToken(token?)` / `getAccessToken()` / `getProjectId()` | 实例方法 | 访存/清理 token |
| `setExecutionToken(token?)` / `getExecutionToken()` | 实例方法 | 执行身份 token（`torchwood.ts:167-173`） |
| `Torchwood.fromExecution(apiBaseUrl, opts?)` | 静态工厂 | 函数内执行身份：`opts.executionToken` 显式参数 > `TW_EXECUTION_TOKEN` 环境变量，皆缺抛错（`torchwood.ts:141-151`）；并发函数（concurrency>1）应从 `ctx.executionToken`/`env.EXECUTION_TOKEN` 显式传入 |

### 3.2 入口：`torchwood.ts:50`

```ts
// sdk/typescript/src/torchwood.ts:50
export class Torchwood {
  readonly account: AccountService;          // Client
  readonly databases: ClientDatabasesService;
  readonly groups: ClientGroupsService;
  readonly realtime: RealtimeService;
  readonly payments: ClientPaymentsService;
  readonly assets: ClientAssetsService;
  readonly subscriptions: ClientSubscriptionsService;
  readonly functions: ClientFunctionsService;      // invokeFunction
  readonly leaderboards: ClientLeaderboardsService; // submit/me/top
  readonly analytics: ClientAnalyticsService;       // ingest（+ 包根 AnalyticsEventBuffer）
  readonly server: {
    health: HealthService;
    projects: ProjectsService;
    users: UsersService;
    groups: ServerGroupsService;
    databases: ServerDatabasesService;
    apiKeys: APIKeysService;
    oauthProviders: OAuthProvidersService;
    storage: StorageService;
    functions: FunctionsService;
    payments: ServerPaymentsService;
    assets: ServerAssetsService;
    subscriptions: ServerSubscriptionsService;
    billing: BillingService;
    outbox: OutboxService;
    leaderboards: ServerLeaderboardsService;  // 提交/条目/top/结算查询
    auditLogs: AuditLogsService;              // list / listWithMeta
    analytics: ServerAnalyticsService;        // ingest + 固定形状查询
  };
  static fromExecution(apiBaseUrl: string, opts?: { executionToken?: string; fetch?: typeof fetch }): Torchwood;
}
```

### 3.3 Server 17 服务（`sdk/typescript/src/server/index.ts`）

| 服务 | 访问路径 | 典型方法 |
|------|----------|----------|
| `health` | `tw.server.health` | `check()`、`getVersion()` |
| `projects` | `tw.server.projects` | `list`/`get`/`create`/`update` |
| `users` | `tw.server.users` | `create`/`list`/`get`/`update`/`delete`/`listSessions`/`createToken` |
| `groups` | `tw.server.groups` | `create`/`list`/`get`/`delete` + `createMembership`/`listMemberships`/`updateMembership` |
| `databases` | `tw.server.databases` | `createDatabase`/`createCollection`/`createAttribute`/`createIndex`/`createDocument`/`countDocuments`/`bulkUpdateDocuments` |
| `apiKeys` | `tw.server.apiKeys` | `create`（`{api_key,secret}` 仅一次）/`list`/`get`/`delete` |
| `oauthProviders` | `tw.server.oauthProviders` | `list`/`upsert`/`delete` |
| `storage` | `tw.server.storage` | `createBucket`/`listBuckets`/`uploadFile`/`listFiles`/`getFile` |
| `functions` | `tw.server.functions` | `listRuntimes`/`listSpecifications`/`create`/`list`/`createExecution`/`getExecution` |
| `payments` | `tw.server.payments` | `listOrders`/`getOrder`/`refund`/`manualFulfill` |
| `assets` | `tw.server.assets` | `createAssetDef`/`listAssetDefs`/`grant`/`consume`/`listUserAssets` |
| `subscriptions` | `tw.server.subscriptions` | `createPlan`/`listPlans`/`cancelSubscription`/`expireSubscription` |
| `billing` | `tw.server.billing` | `getUsage`/`listRollups`/`listStatements` |
| `outbox` | `tw.server.outbox` | `listDeadLetters(projectId, params?)` / `replayDeadLetter(eventId, projectId)`（见 §3.4） |
| `leaderboards` | `tw.server.leaderboards` | `submitLeaderboardScore` / `getLeaderboardEntry` / `listLeaderboardTop` / `getLeaderboardSettlement` / `listLeaderboardSettlements`（可代任意 subject；`leaderboards.write`/`read`，见 `18-leaderboards.md`） |
| `auditLogs` | `tw.server.auditLogs` | `list` / `listWithMeta`（admin/owner + `audit_logs.read`） |
| `analytics` | `tw.server.analytics` | `ingest`（可信代报 user_id，`analytics:write`）+ `getOverview` / `listEventDefinitions` / `queryTimeseries` / `queryBreakdown` / `queryRetention` / `listUserEvents`（`analytics:read`），响应带 `source: rollup|raw` 口径标注（`src/server/analytics.ts:136-231`，见 `18-analytics.md`） |

### 3.4 Outbox（W-J）

`sdk/typescript/src/server/outbox.ts:18`：

```ts
await tw.server.outbox.listDeadLetters("default", { pageSize: 20 });
await tw.server.outbox.replayDeadLetter("01H...", "default");
// → { event_id, available_at }
```

`auth: "apiKey"`，需 `outbox:read` / `outbox:write`，`owner|admin` 可操作；`replayDeadLetter` 幂等。

### 3.5 Client API（`tw.account` / `tw.databases` / `tw.groups` / `tw.functions` / `tw.leaderboards` / `tw.analytics`）

- 鉴权：`HttpTransport.request(..., {auth:"user"|"none"})`，有 token 时 `Authorization: Bearer`，无则匿名；`auth:"none"` 不带头（如 sign-up/health）；
- Account 成功后自动 `setAccessToken`：`signUp`/`signIn`/`refresh`/`createEmailOTPSession` 等；
- Databases：`createDocument`/`listDocuments`/`getDocument`/`updateDocument`/`upsertDocument`/`deleteDocument`/`countDocuments`（签名 `(databaseId, collectionId, ...)`）；typed 查询构造器（`src/query.ts`，包根导出）与 `QueryAst` 走 POST `:list`（`src/client/databases.ts:11`）；`queries[]` DSL 字符串仍兼容；
- Functions：`tw.functions.invokeFunction(functionId, input?)`（`src/client/functions.ts:29`）；
- Leaderboards：`tw.leaderboards.submitLeaderboardScore(board, value, {tiebreakValue?, period?, requestId?})` / `getMyLeaderboardEntry` / `listLeaderboardTop`（`src/client/leaderboards.ts:15-34`）；
- Analytics：`tw.analytics.ingest(events)` → POST `/v1/analytics/events`，归因（user_id）由服务端从 principal 落定（含匿名会话），部分接收语义 `{accepted, skipped}`（`src/client/analytics.ts:36-40`）；**`AnalyticsEventBuffer`**（包根导出，`src/client/analytics.ts:104`）size/time 双阈值 flush（默认 20 条或 10s，单批上限 100），浏览器环境自动注册 `visibilitychange`(hidden)/`beforeunload` 尽力 flush，失败静默 + 有界指数退避重试，`track`/`flush` 永不抛错；各端（小游戏/原生）接入配方见 `18-analytics.md` §7；
- 传输：`fetch` + JSON，支持 `queries[]` 数组展开（Appwrite DSL），204 返回 `undefined`，非 2xx 抛 `TorchwoodError`（`status` + `code` + `body`）。

---

## 4. Go SDK

### 4.1 总览

| 包 | 认证 | 服务 |
|----|------|------|
| `sdk/go/client`（`client.go:68`） | `Authorization: Bearer <JWT>`（`client.TokenStore` 自动刷新） | `Account` / `Groups` / `Databases`（`UseDatabase` 绑定）/ `Payments` / `Assets` / `Subscriptions` / `Functions`（`InvokeFunction/InvokeString`）；Realtime 不在 struct，经 `ConnectRealtime(ctx, httpEndpoint, opts...)`（`realtime.go:138`，断线重连 + last_seq 补发） |
| `sdk/go/server`（`server/client.go:76`） | `x-api-key` + `x-torchwood-project`（`authInterceptor:165`） | `Health` / `Users` / `Groups` / `Databases` / `Projects` / `Storage` / `Functions` / `OAuthProviders` / `Payments` / `Assets` / `Subscriptions` / `Billing` / `Outbox` / `Leaderboards` / `AuditLogs` / `Analytics` 共 16 个 + `InvokeJSON` |

Options：

- Server：`WithAPIKey` / `WithProjectID` / `WithDatabaseID` / `WithDialOptions` / `WithTLS`（系统根证书）/ `WithTimeout` / `WithRetryDisabled` / `WithUserAgent`（审计通道归类 cli/sdk/api）；
- Client：`WithProjectID` / `WithDatabaseID` / `WithTokenStore` / `WithOnTokensChanged` / `WithInitialTokens` / `WithDialOptions` / `WithTimeout` / `WithRetryDisabled`；
- 均有 `UseDatabase(id)` 返回绑定库的 Databases 副本；默认 `insecure`，生产用 `WithTLS` 或 `WithDialOptions(grpc.WithTransportCredentials(...))`。

### 4.2 自动刷新与 `FileTokenStore`（`sdk/go/client/token.go`）

`TokenStore` 接口：`Load() (*TokenBundle,error)` / `Save` / `Clear`；内置 `MemoryTokenStore` 与 `FileTokenStore`（`token.go:55`，JSON + `protojson`，`0600`，临时文件 + `rename` 原子写，`0600`/`0700` 目录，`sync.Mutex` 并发安全，`~`/`~/` 自动展开）。

刷新（unary interceptor，对全部调用透明）：

1. **主动**：`expires_at` 距 `now` 不足 `refreshSkew=30s`（`client.go:15`）且有 refresh token 时先刷新；
2. **被动**：返回 `Unauthenticated` 时刷新一次并重试；
3. 并发去重：`sync.Mutex` 串行 + double-check；
4. 仅当 RPC 明确 `Unauthenticated` 才清空本地 token，临时错误保留；
5. `OnTokensChanged` 在登录/刷新/清空（`nil`）时回调；`SignOut` 或 `Unauthenticated` 清空；`SignIn`/`SignUp` 仅非 MFA 且 `access_token` 非空时落盘。

### 4.3 `InvokeJSON` 动态分发（`sdk/go/server/invoke.go:20`）

```go
respJSON, err := c.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/CreateUser", reqJSON)
```

- 按 `protoregistry.GlobalFiles` 查 `MethodDescriptor`，限定 `torchwood.server.v1.*` 且排除 `APIKeysService`（`findServerMethod:40`），proto 新增方法自动可用；
- `reqJSON` 为 `protojson`（camelCase，未知字段报错），响应为缩进 `protojson`（`MarshalOptions{Multiline:true,Indent:"  "}`）；
- `reqJSON` 为空等价 `{}`；未知方法 `torchwood: unknown method "<method>"`；
- Agent 工具箱：`server.Tools` / `LookupTool` / `InvokeTool`（`tools.go:46` 的 18 条 `Tool{ Name, FullMethod, InputNotes }`），TS 对等 `agentTools`/`lookupAgentTool`/`TOOL_*`，catalog 只读。

### 4.4 典型用法

```go
import ("context"; "github.com/torchwoodcloud/torchwood/sdk/go/client"; "github.com/torchwoodcloud/torchwood/sdk/go/server"; serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1")

// Server API（管理面）
srv, _ := server.New("127.0.0.1:9060", server.WithAPIKey(os.Getenv("TORCHWOOD_API_KEY")), server.WithDatabaseID("app"))
user, _ := srv.Users.CreateUser(ctx, "agent@example.com", "Pass@123", "Agent", "active", nil, nil)
tok, _  := srv.Users.CreateUserToken(ctx, user.Id)
doc, _  := srv.Databases.UpsertDocument(ctx, "members", "m1", map[string]any{"channel_id":"ch1"}, []string{"channel_id","user_id"}, nil)
n, _    := srv.Databases.CountDocuments(ctx, "messages", &sharedv1.Query{
  Filter: query.Eq("channel_id", "ch1"),
})
raw, _  := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{"pageSize":10}`))
letters,_ := srv.Outbox.ListDeadLetters(ctx, &serverv1.ListDeadLettersRequest{ProjectId: "default", PageSize: 20})
_ = tok; _ = doc; _ = n; _ = raw; _ = letters

// Client API（自动刷新）
store := client.NewFileTokenStore("~/.torchwood/tokens.json")
c, _ := client.New("127.0.0.1:9060", client.WithProjectID("default"), client.WithTokenStore(store))
_, _ = c.Account.SignIn(ctx, "u@example.com", "Pass@123")
me, _ := c.Account.Me(ctx)
_ = me
```

- 错误：`status.Code(err)` 判 `codes.NotFound`/`PermissionDenied` 等；限流响应可用 `server.ExtractRetryAfter(err)` 读出建议退避秒数；
- 超时与重试：SDK 默认单次调用 30s 超时（`WithTimeout` 调整；调用方 ctx 已带 deadline 时尊重调用方），默认对 `Unavailable` 自动重试（最多 4 次指数退避），`WithRetryDisabled` 可关闭；
- 文档：入参 `map[string]any` → `structpb`，读回数值多为 `float64`；
- 查询：文档面收 typed AST（`*sharedv1.Query`，`sdk/go/query` 提供 `Eq/Gt/...` 构造器与链式 `Builder`）；Appwrite DSL 串经 `query.FromDSL` 在**客户端**解析为 AST 后发送（服务端零字符串解析）；`ListDatabases/ListCollections` 等静态面仍走 `queries` 串参数；
- `cmd/torchwood`（`bin/torchwood`）**仅依赖 `sdk/go/server`** 的 `InvokeJSON`，源码不直连 `genproto/grpc`（`import_guard_test` 兜底），新增 RPC 无需 CLI 登记；
- 测试：`bufconn` 内存 gRPC，无外部依赖，已纳入 `task test`（`test:sdk-go`）与 `task lint`（`lint:sdk-go`）；文档示例可编译性由 `sdk/go/docexamples`（build tag `docexample`，`go vet -tags docexample ./sdk/...`）保证。

---

## 5. 错误与类型

- TS：`TorchwoodError { status:number, code?:string, body?:unknown }`，`{ error:{message,code} }` 信封解析，无内置重试；
- Go：gRPC `status` 错误；
- TS 类型为手写（`src/types.ts`），字段 snake_case 与 `UseProtoNames: true` 的 HTTP JSON 一致，时间一律 RFC3339（`google.protobuf.Timestamp`），以 `genproto/**/*.swagger.json`（`buf.gen.yaml:24` 的 `openapiv2`）为权威；
- SDK demo：`sdk/demo`（Vite 5174），`VITE_TORCHWOOD_ENDPOINT`/`VITE_TORCHWOOD_PROJECT_ID` 覆盖默认，设置页填 API Key 后可跑通 Server/Client 全链路。
