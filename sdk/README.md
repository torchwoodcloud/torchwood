# Torchwood TypeScript SDK

`@torchwood/sdk` 封装 Torchwood **Client API**（用户 JWT）与 **Server API**（API Key + `X-Torchwood-Project`），是 Torchwood **AI/Agent-Native** 能力的前端集成层 —— 便于 LLM Agent、自动化脚本与 MCP Tool Server 以类型安全的方式调用后端。

## Agent / 自动化集成要点

| 场景 | 推荐方式 | 说明 |
|------|----------|------|
| 管理面自动化（建用户、管文档、Storage） | **Server API** + API Key | 在 Console 或通过 `POST /v1/server/api-keys` 创建带 scope 的 Key |
| 终端用户身份流（注册/登录/会话） | **Client API** + JWT | SDK 自动持久化 access token |
| Agent 工具 schema 来源 | **OpenAPI** | `task generate:proto` 后在 `genproto/**/*.swagger.json` 获取 |
| Agent 默认工具箱 | **E-7 overlay** | 18 个动词映射现有 Server RPC（`sdk/go/server/tools.go` / `agentTools`）；完整 API 仍是 188 RPC（Client 61 + Server 117 + Console 10），不含 API key 管理，见 `docs/developer/14-agent-tools.md` |
| 快速验证 | **Web 演示** | `task sdk:demo`，设置页填入 Console API Keys 页面创建的 API Key |

典型 Agent 工作流：用 scoped API Key 实例化 `Torchwood.withApiKey()` → 读取 OpenAPI 或 SDK 类型 → 调用 Server Databases/Users/Storage API → 将结构化响应回传给 LLM。

## 目录

| 路径 | 说明 |
|------|------|
| `typescript/` | SDK 包 `@torchwood/sdk`（TypeScript） |
| `go/` | 官方 Go SDK（模块 `github.com/torchwoodcloud/torchwood/sdk/go`） |
| `demo/` | Web 演示站点（注册/登录 + SDK 功能演示） |

## Go SDK

Go SDK 是 gRPC 直连的薄封装（默认本地走 insecure，标准 TLS 用 `WithTLS`（系统根证书），
自定义 CA/mTLS 用 `WithDialOptions` 配置），
拆分为 `sdk/go/client`（Client API，Bearer JWT，自动刷新 token）与
`sdk/go/server`（Server API，API Key，含 InvokeJSON 动态分发）两个子包。

```go
import (
    "context"

    "github.com/torchwoodcloud/torchwood/sdk/go/client"
    "github.com/torchwoodcloud/torchwood/sdk/go/server"
)

// Server API：以 API Key 管理用户/用户组/文档库
srv, err := server.New("127.0.0.1:9060",
    server.WithAPIKey(apiKey),
    server.WithDatabaseID("app"),
)

// 为 Agent 账号签发 client token
tok, err := srv.Users.CreateUserToken(ctx, "user-1")

// 文档 upsert（按唯一索引冲突列）
doc, err := srv.Databases.UpsertDocument(ctx, "members", "m1",
    map[string]any{"channel_id": "ch1", "last_read_seq": 42},
    []string{"channel_id", "user_id"}, nil)

// 逃生舱：按方法名 + JSON 调用任意 Server API unary 方法
respJSON, err := srv.InvokeJSON(ctx, "/torchwood.server.v1.UsersService/ListUsers", []byte(`{"pageSize":10}`))

// Agent 默认工具箱：工具名 → 已有 RPC（不含 API key 管理）
respJSON, err = srv.InvokeTool(ctx, "list_users", []byte(`{"pageSize":10}`))

// Client API：注册/登录自动保存 token，过期自动刷新
store := client.NewFileTokenStore("~/.torchwood/tokens.json")
c, err := client.New("127.0.0.1:9060",
    client.WithProjectID("default"),
    client.WithTokenStore(store),
)
_, err = c.Account.SignIn(ctx, "u@example.com", "Pass@123")
me, err := c.Account.Me(ctx)
```

- 文档 API 默认绑定 `WithDatabaseID` 指定的库，可用 `c.UseDatabase(id)` 切换副本。
- client 包 TokenStore 支持内存与 JSON 文件两种实现；`OnTokensChanged` 回调感知登录/刷新/清空。
- 所有调用返回 gRPC status 错误，用 `status.Code(err)` 判别（`codes.NotFound` 等）。
- SDK 测试通过 bufconn 内存 gRPC 服务运行，无需外部依赖；`task test` / `task lint:go` 已覆盖 `sdk/go`。

## 快速开始

对外用户安装已发布的 npm 包：

```bash
npm install @torchwood/sdk
```

仓库内开发（含 Web 演示站点）：

```bash
# 安装依赖并编译 SDK
task sdk:install
task sdk:build

# 启动本地 Torchwood（另开终端）
task docker:up
task db:migrate
task dev:server
# 全新数据库上打开 http://127.0.0.1:9080/console/ 完成首次部署引导，
# 注册第一个管理员（填写 project id / database id），登录后到 API Keys 页面创建密钥

# 启动 Web 演示（默认 http://localhost:5174）
task sdk:demo
```

复制 `sdk/demo/.env.example` 为 `sdk/demo/.env` 可调整默认 Endpoint / Project ID。

## Web 演示站点

演示站点提供完整的前端体验：

| 页面 | 说明 |
|------|------|
| `/register` `/login` | 用户注册与登录（Client Account SDK） |
| `/app/account` | me / prefs / sessions / refresh |
| `/app/databases` | Server + Client Databases API 全功能验证 |
| `/app/groups` | 创建用户组、刷新 Token、邀请成员 |
| `/app/server` | Health / Projects / Users / Groups / Databases |
| `/app/settings` | Endpoint、Project ID、API Key 配置 |

Server API 相关功能需在设置页填写 Console API Keys 页面创建的 API Key。

## SDK 用法

```typescript
import { Torchwood } from "@torchwood/sdk";

// Server API
const admin = Torchwood.withApiKey("http://localhost:9080", "default", apiKey);
await admin.server.health.check();

// Client API（注册后自动保存 access token）
const client = Torchwood.create({ endpoint: "http://localhost:9080", projectId: "default" });
await client.account.signUp({ email: "u@example.com", password: "Pass@123", name: "User" });
await client.databases.createDocument("app", "notes", { data: { title: "Hi" } });
```

## 已实现 API surface

**Client：** Account（注册/登录/会话/偏好）、Databases（文档 CRUD + count）、Groups 与 Memberships、Realtime（WebSocket 订阅）、Assets、Payments、Subscriptions。

**Server：** Health、Projects、Users、Groups、Databases（库/集合/属性/索引/文档/Bulk）、API Keys、OAuthProviders、Storage（Bucket/File）、Functions、Payments、Assets、Subscriptions、Billing、Outbox，以及 18 个 Agent 默认工具目录（`agentTools` / `lookupAgentTool`）。

**事务提示（Functions 开发者）**：函数代码运行在外部容器，不与服务端共享事务——函数内的多写原子批请调用 `documents:execute-tx`（`ExecuteTransactions`，Server 面；ATOMIC 模式任一失败整批回滚，批内事件序 = op 序，详见 `docs/developer/06-databases.md` §8.1 与 `08-functions.md` §4.1）。

## 版本策略与兼容承诺（A10 决策 memo，2026-09-05 成文；2026-09-06 维护者拍板：按推荐三项执行，决议回写 `docs/developer/15-exit-poc.md` A10）

**现状（契约已分叉）**：npm `@torchwood/sdk` v0.1.0（2026-09-01）与 `sdk/go` v0.1.2（2026-08-26，genproto v0.1.2）已对外发布；此后 POC 期 proto 发生 reserved 级断裂——`queries` 双栈退役（client/server databases 两面 `reserved 3`）、`ListRequest.filter/order_by` reserved、错误码直换（域码体系、不可见文档 403→404 翻转、`expected_version=0` 改 InvalidArgument）、分页 token 换 keyset（`ka:/kb:`），并新增 execute-tx / aggregate / `:changes`（ListChanges）/ vectorSearch。**已发布 SDK 与服务端契约已分叉**（TS 合同测试在 R17 前红为证，`ee0d9ea` 转绿）。分叉中最危险的一档：reserved 前字段被 proto 运行时**静默忽略**——旧 SDK 的过滤条件失效、结果集语义错误而非法错（逐项矩阵见 `docs/design/poc-to-release-migration.md` §7）。

**版本号规则（推荐决议）**：

1. **转出前维持 0.x：minor 版本携带破坏性变更**。0.x 本身就是"契约未冻结"的机器可读表达；下一版 `@torchwood/sdk` **0.2.0** + `sdk/go` **v0.2.0** + `genproto` **v0.2.0**（同一 release train，`release.yml` 一次发布三件套）收拢全部已发生断裂，不做"半冻结"版本。
2. **1.0.0 与转出门禁绑定**：`docs/developer/15-exit-poc.md` A 区清零、首次对外发布的那次 release 即 1.0.0——自此同 major 内契约冻结，破坏性变更只进 major。1.0 前不设额外冻结等待期：断裂已全部由 0.2.0 承载，门禁期间契约面的小改动随 0.x minor 走。
3. **migration note 义务**：0.2.0 起，凡 minor 含破坏性变更，`CHANGELOG.md` 该版本小节必须附**迁移说明**，内容对照 A5 §7 客户端契约升级矩阵逐项给出（旧形态 / 新形态 / 客户端症状 / 改法）；纯新增 minor 豁免。0.2.0 的迁移说明以该矩阵为唯一底稿，不再另行盘点。
4. **兼容矩阵承诺**：**不承诺"旧 SDK × 新服务端"组合**（分叉教训：静默忽略比显式报错危害大，与其承诺兼容不如要求 SDK 与服务端同批升级）；服务端承诺支持**最近两个 SDK minor**（N 与 N-1，同 major），SDK 侧在 CHANGELOG 声明"最低服务端版本"。Go SDK 走 nested-module tag，SDK↔genproto 的版本对应由发布流水线的 require rewrite 锁定（v0.1.1 流水线修复后既有保证）。

**决议（2026-09-06，维护者，按推荐三项）**：① 0.2.0（minor 携带破坏性 + migration note）先行收拢分叉——**已于 2026-09-07 发布**（三面同 train，migration note 见 `CHANGELOG.md` 各 0.2.0 小节）；② 1.0.0 与转出门禁绑定、自此冻结契约（尚未触发）；③ 兼容矩阵 = 不承诺旧 SDK × 新服务端、服务端支持最近两个 SDK minor。
