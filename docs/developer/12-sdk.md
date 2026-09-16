# 12 SDK 指南

覆盖 TypeScript SDK（`sdk/typescript/`，`@torchwood/sdk`）与 Go SDK（`sdk/go/client`、`sdk/go/server`）。符号与签名以源码为准：TS 门面见 `sdk/typescript/src/torchwood.ts`，Go Server 见 `sdk/go/server/client.go`，Go Client 见 `sdk/go/client/client.go`。

> 关联：`09-api-guide.md`（API 约定）、`14-agent-tools.md`（Agent 工具箱）、`sdk/README.md`。

## 1. 定位与选型

| 场景 | 推荐 | 说明 |
|------|------|------|
| 管理面自动化（用户 / 文档 / 存储） | **Server API** + API Key | Console 或 `POST /v1/server/api-keys` 创建带 scope 的 Key |
| 终端用户身份流 | **Client API** + JWT | TS 走 `fetch`，Go 走 `FileTokenStore` 自动刷新 |
| 函数内执行身份 | **`Torchwood.fromExecution(apiBaseUrl, opts?)`** | 以 execution principal 构造 client（方法面 = server 服务类全量）；token 优先级 = 显式 `opts.executionToken` > `TW_EXECUTION_TOKEN` 环境变量，皆缺 fail-closed 抛错；并发函数（concurrency>1）应从 `ctx.executionToken` / `env.EXECUTION_TOKEN` 显式传入（见 `08-functions.md` §4.3.1） |
| Agent 默认工具箱 | **overlay 18 动词** | TS `TOOL_*` / `agentTools` 与 Go `Tool*` / `Tools` 映射到现有 Server RPC，见 `14-agent-tools.md` |
| 逃生舱 | **Go `InvokeJSON`** | `sdk/go/server` 的 `InvokeJSON(fullMethod, protojson)` 覆盖全部 `torchwood.server.v1.*` unary（排除 APIKeysService） |

## 2. 包结构与构建

### 2.1 目录

| 路径 | 说明 |
|------|------|
| `sdk/typescript/` | `@torchwood/sdk`，`type: module`，`main` → `dist/index.js` |
| `sdk/typescript/src/torchwood.ts` | `Torchwood` 门面（含 `fromExecution`） |
| `sdk/typescript/src/http.ts` | `HttpTransport` + `TorchwoodConfig`（authMode 含 `"execution"`） |
| `sdk/typescript/src/server/` | 17 个 Server 服务类；另有 `tools.ts` 为 18 动词工具目录 |
| `sdk/typescript/src/client/` | Client 服务（Account / Databases / Groups / Payments / Assets / Subscriptions / Realtime / Functions / Leaderboards / Analytics） |
| `sdk/typescript/src/index.ts` | 包根导出（含 `AnalyticsEventBuffer`、typed query 构造器 re-export） |
| `sdk/typescript/src/errors.ts` | `TorchwoodError` |
| `sdk/typescript/src/query.ts` | TS typed AST 查询构造器（`eq` / `gt` / `contains` / …） |
| `sdk/typescript/src/types.ts` | 手写类型（非 proto 生成） |
| `sdk/go/client/` | end-user 客户端（Bearer JWT + 自动刷新） |
| `sdk/go/server/` | 管理面客户端（`x-api-key` + `InvokeJSON` + `Tools`） |
| `sdk/go/internal/conn/` | 拨号封装 |
| `sdk/demo/` | Vite 演示站（`task sdk:demo`，端口 5174） |

### 2.2 构建与安装

```bash
task sdk:install   # sdk/typescript + sdk/demo 各 npm install
task sdk:build     # tsc → dist/（不含 __tests__）
task sdk:demo      # vite dev（http://localhost:5174）
task sdk:publish   # 发布 npm
```

- 外部用户直接 `npm install @torchwood/sdk`。
- TS SDK 零运行时依赖，仅 `typescript`（dev），HTTP 走全局 `fetch`（`TorchwoodConfig.fetch` 可注入）。
- Go SDK 为独立 module：`github.com/torchwoodcloud/torchwood/sdk/go`（本地开发用 `replace`）。

## 3. TypeScript SDK

### 3.1 配置与工厂

```ts
interface TorchwoodConfig {
  endpoint: string;        // 如 http://localhost:9080
  projectId?: string;      // 如 "app"（execution 模式可省略）
  apiKey?: string;         // Server API（X-Api-Key + X-Torchwood-Project）
  accessToken?: string;    // Client API（Authorization: Bearer）
  executionToken?: string; // 函数执行身份
  fetch?: typeof fetch;    // 可选注入
}
```

`AuthMode` 四态：`"apiKey" | "user" | "execution" | "none"`。

| 成员 | 说明 |
|------|------|
| `new Torchwood(config)` / `Torchwood.create(config)` | 直接构造 / 静态工厂 |
| `Torchwood.withApiKey(ep, pid, key)` | Server API 工厂 |
| `Torchwood.withAccessToken(ep, pid, token)` | Client API 工厂 |
| `Torchwood.fromExecution(apiBaseUrl, opts?)` | 函数内执行身份工厂（见 §1） |
| `setAccessToken / getAccessToken / getProjectId` | 访存 / 清理 token |
| `setExecutionToken / getExecutionToken` | 执行身份 token |

### 3.2 门面入口

```ts
export class Torchwood {
  readonly account: AccountService;                   // Client
  readonly databases: ClientDatabasesService;
  readonly groups: ClientGroupsService;
  readonly realtime: RealtimeService;
  readonly payments: ClientPaymentsService;
  readonly assets: ClientAssetsService;
  readonly subscriptions: ClientSubscriptionsService;
  readonly functions: ClientFunctionsService;         // invokeFunction
  readonly leaderboards: ClientLeaderboardsService;   // submit/me/top
  readonly analytics: ClientAnalyticsService;         // ingest（+ 包根 AnalyticsEventBuffer）
  readonly server: { /* 17 个 Server 服务，见 §3.3 */ };
  static fromExecution(apiBaseUrl: string, opts?): Torchwood;
}
```

### 3.3 Server 17 服务

| 服务 | 访问路径 | 典型方法 |
|------|----------|----------|
| `health` | `tw.server.health` | `check()`、`getVersion()` |
| `projects` | `tw.server.projects` | list / get / create / update |
| `users` | `tw.server.users` | create / list / get / update / delete / listSessions / createToken |
| `groups` | `tw.server.groups` | create / list / get / delete + membership 三件 |
| `databases` | `tw.server.databases` | createDatabase / createCollection / createAttribute / createIndex / createDocument / countDocuments / bulkUpdateDocuments |
| `apiKeys` | `tw.server.apiKeys` | create（`{api_key, secret}` 仅一次）/ list / get / delete |
| `oauthProviders` | `tw.server.oauthProviders` | list / upsert / delete |
| `storage` | `tw.server.storage` | createBucket / listBuckets / uploadFile / listFiles / getFile |
| `functions` | `tw.server.functions` | listRuntimes / listSpecifications / create / list / createExecution / getExecution |
| `payments` | `tw.server.payments` | listOrders / getOrder / refund / manualFulfill |
| `assets` | `tw.server.assets` | createAssetDef / listAssetDefs / grant / consume / listUserAssets |
| `subscriptions` | `tw.server.subscriptions` | createPlan / listPlans / cancelSubscription / expireSubscription |
| `billing` | `tw.server.billing` | getUsage / listRollups / listStatements |
| `outbox` | `tw.server.outbox` | listDeadLetters / replayDeadLetter（§3.4） |
| `leaderboards` | `tw.server.leaderboards` | submit / getEntry / listTop / getSettlement / listSettlements（可代任意 subject；`leaderboards.write`/`read`，见 `20-leaderboards.md`） |
| `auditLogs` | `tw.server.auditLogs` | list / listWithMeta（admin/owner + `audit_logs.read`） |
| `analytics` | `tw.server.analytics` | ingest（可信代报 user_id，`analytics.write`）+ getOverview / listEventDefinitions / queryTimeseries / queryBreakdown / queryRetention / listUserEvents（`analytics.read`），响应带 `source: rollup\|raw` 口径标注（见 `18-analytics.md`） |

### 3.4 Outbox 示例

```ts
await tw.server.outbox.listDeadLetters("app", { pageSize: 20 });
await tw.server.outbox.replayDeadLetter("01H...", "app");
// → { event_id, available_at }
```

需 `outbox:read` / `outbox:write` scope，`owner|admin` 可操作；`replayDeadLetter` 幂等。

### 3.5 Client API

- 鉴权：`HttpTransport.request(..., {auth:"user"|"none"})`，有 token 时 `Authorization: Bearer`，无则匿名；`auth:"none"` 不带头（sign-up / health 等）。
- Account 成功后自动 `setAccessToken`（signUp / signIn / refresh / createEmailOTPSession 等）。
- Databases：`createDocument` / `listDocuments` / `getDocument` / `updateDocument` / `upsertDocument` / `deleteDocument` / `countDocuments`（签名 `(databaseId, collectionId, ...)`）；typed 查询构造器与 `QueryAst` 走 POST `:list`。
- Functions：`tw.functions.invokeFunction(functionId, input?)`。
- Leaderboards：`tw.leaderboards.submitLeaderboardScore(board, value, {tiebreakValue?, period?, requestId?})` / `getMyLeaderboardEntry` / `listLeaderboardTop`。
- Analytics：`tw.analytics.ingest(events)` → POST `/v1/analytics/events`，归因（user_id）由服务端从 principal 落定（含匿名会话），部分接收语义 `{accepted, skipped}`。**`AnalyticsEventBuffer`**（包根导出）：size / time 双阈值 flush（默认 20 条或 10s，单批上限 100），浏览器环境自动注册 `visibilitychange`(hidden) / `beforeunload` 尽力 flush，失败静默 + 有界指数退避重试，`track` / `flush` 永不抛错；各端（小游戏 / 原生）接入配方见 `18-analytics.md`。
- 传输：fetch + JSON，204 返回 undefined，非 2xx 抛 `TorchwoodError`。

## 4. Go SDK

### 4.1 总览

| 包 | 认证 | 服务 |
|----|------|------|
| `sdk/go/client` | `Authorization: Bearer <JWT>`（TokenStore 自动刷新） | Account / Groups / Databases（UseDatabase 绑定）/ Payments / Assets / Subscriptions / Functions（InvokeFunction / InvokeString）。Realtime 经 `ConnectRealtime(ctx, httpEndpoint, opts...)`（断线重连 + last_seq 补发） |
| `sdk/go/server` | `x-api-key` + `x-torchwood-project` | Health / Users / Groups / Databases / Projects / Storage / Functions / OAuthProviders / Payments / Assets / Subscriptions / Billing / Outbox / Leaderboards / AuditLogs / Analytics 共 16 个 + `InvokeJSON` |

Options：

- Server：`WithAPIKey` / `WithProjectID` / `WithDatabaseID` / `WithDialOptions` / `WithTLS`（系统根证书）/ `WithTimeout` / `WithRetryDisabled` / `WithUserAgent`（审计通道归类 cli / sdk / api）。
- Client：`WithProjectID` / `WithDatabaseID` / `WithTokenStore` / `WithOnTokensChanged` / `WithInitialTokens` / `WithDialOptions` / `WithTimeout` / `WithRetryDisabled`。
- 均有 `UseDatabase(id)` 返回绑定库的 Databases 副本；默认 insecure，生产用 `WithTLS` 或自定义 transport credentials。

### 4.2 自动刷新与 FileTokenStore

`TokenStore` 接口：`Load` / `Save` / `Clear`；内置 `MemoryTokenStore` 与 `FileTokenStore`（JSON + protojson，0600 权限，临时文件 + rename 原子写，`~` 自动展开，并发安全）。

刷新（unary interceptor，对全部调用透明）：

1. **主动**：`expires_at` 距 now 不足 30s 且有 refresh token 时先刷新；
2. **被动**：返回 Unauthenticated 时刷新一次并重试；
3. 并发去重：互斥串行 + double-check；
4. 仅当 RPC 明确 Unauthenticated 才清空本地 token，临时错误保留；
5. `OnTokensChanged` 在登录 / 刷新 / 清空时回调；SignIn / SignUp 仅非 MFA 且 access token 非空时落盘。

### 4.3 InvokeJSON 动态分发

```go
respJSON, err := c.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/CreateUser", reqJSON)
```

- 按 `protoregistry.GlobalFiles` 查 MethodDescriptor，限定 `torchwood.server.v1.*` 且排除 APIKeysService——proto 新增方法自动可用（覆盖完整性由 sdk 测试遍历 registry 断言）。
- `reqJSON` 为 protojson（camelCase，未知字段报错），响应为缩进 protojson；空请求等价 `{}`；未知方法报 `torchwood: unknown method`。
- Agent 工具箱：`server.Tools` / `LookupTool` / `InvokeTool`（18 条工具目录），TS 对等 `agentTools` / `lookupAgentTool` / `TOOL_*`。

### 4.4 典型用法

```go
import (
  "context"
  "github.com/torchwoodcloud/torchwood/sdk/go/client"
  "github.com/torchwoodcloud/torchwood/sdk/go/server"
  serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// Server API（管理面）
srv, _ := server.New("127.0.0.1:9060",
  server.WithAPIKey(os.Getenv("TORCHWOOD_API_KEY")),
  server.WithDatabaseID("app"))
user, _ := srv.Users.CreateUser(ctx, "agent@example.com", "Pass@123", "Agent", "active", nil, nil)
tok, _ := srv.Users.CreateUserToken(ctx, user.Id)
doc, _ := srv.Databases.UpsertDocument(ctx, "members", "m1",
  map[string]any{"channel_id": "ch1"}, []string{"channel_id", "user_id"}, nil)
n, _ := srv.Databases.CountDocuments(ctx, "messages", &sharedv1.Query{
  Filter: query.Eq("channel_id", "ch1"),
})
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

- 错误：`status.Code(err)` 判 `codes.NotFound` / `PermissionDenied` 等；限流响应可用 `server.ExtractRetryAfter(err)` 读出建议退避秒数。
- 超时与重试：SDK 默认单次调用 30s 超时（`WithTimeout` 调整；调用方 ctx 已带 deadline 时尊重调用方），默认对 Unavailable 自动重试（最多 4 次指数退避），`WithRetryDisabled` 可关闭。
- 文档：入参 `map[string]any` → structpb，读回数值多为 `float64`。
- 查询：文档面收 typed AST（`*sharedv1.Query`，`sdk/go/query` 提供 `Eq/Gt/...` 构造器与链式 Builder）；DSL 串经 `query.FromDSL` 在**客户端**解析为 AST 后发送（服务端零字符串解析）；`ListDatabases` 等静态面仍走 `queries` 串参数。
- CLI：`cmd/torchwood`（实现随仓库根 `cli/` 包）**仅依赖 `sdk/go/server`** 的 InvokeJSON，源码不直连 genproto（`cli/import_guard_test.go` 兜底），新增 RPC 无需 CLI 登记。
- 测试：bufconn 内存 gRPC 无外部依赖，纳入 `task test` 与 `task lint`；文档示例可编译性由 `sdk/go/docexamples`（build tag `docexample`）保证。

## 5. 错误与类型

- TS：`TorchwoodError { status, code?, body? }`，解析 `{error:{message,code}}` 信封，无内置重试。
- Go：gRPC `status` 错误。
- TS 类型为手写（`src/types.ts`），字段 snake_case 与 HTTP JSON（`UseProtoNames: true`）一致，时间一律 RFC3339；以 `genproto/**/*.swagger.json` 为权威契约。
- SDK demo：`sdk/demo`（Vite，端口 5174），`VITE_TORCHWOOD_ENDPOINT` / `VITE_TORCHWOOD_PROJECT_ID` 覆盖默认，设置页填 API Key 后可跑通 Server / Client 全链路。

## 相关文档

- `14-agent-tools.md` — Agent 18 动词工具箱
- `18-analytics.md` — Analytics 接入配方
- `20-leaderboards.md` — 排行榜语义
- `08-functions.md` §4.2/§4.3 — 函数内 `fromExecution` 的执行身份上下文
