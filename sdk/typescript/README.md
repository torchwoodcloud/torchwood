# @torchwood/sdk

Torchwood 的官方 TypeScript SDK，封装 **Client API**（用户 JWT）与 **Server API**（scoped API Key + `X-Torchwood-Project`），以类型安全的方式调用 [Torchwood](https://github.com/torchwoodcloud/torchwood) 后端——适合前端应用、自动化脚本与 LLM Agent 集成。

## 安装

```bash
npm install @torchwood/sdk
```

要求 Node.js >= 18（ESM-only）。

## 快速开始

```typescript
import { Torchwood } from "@torchwood/sdk";

// Server API：管理面操作（scoped API Key）
const admin = Torchwood.withApiKey("http://localhost:9080", "default", apiKey);
await admin.server.health.check();

// Client API：终端用户身份流（注册后自动保存 access token）
const client = Torchwood.create({ endpoint: "http://localhost:9080", projectId: "default" });
await client.account.signUp({ email: "u@example.com", password: "Pass@123", name: "User" });
await client.databases.createDocument("app", "notes", { data: { title: "Hi" } });
```

已有 access token 时也可以用 `Torchwood.withAccessToken(endpoint, projectId, accessToken)` 直接实例化。

## 函数内使用（execution 身份）

在 Torchwood Functions 的函数代码里，用 `Torchwood.fromExecution()` 直接获得**带函数执行身份**的 client（`Authorization: Bearer <executionToken>`，项目绑定在 token 内，无需 projectId / API Key）。方法面 = Server API 服务类全量（`databases` / `users` / `assets` / `functions` ...），权限由函数的 declared_scopes 约束（fail-closed）：

```typescript
import { Torchwood } from "@torchwood/sdk";

export async function main(ctx) {
  const tw = Torchwood.fromExecution(ctx.apiBaseUrl); // token 取自 TW_EXECUTION_TOKEN
  await tw.server.databases.createDocument("app", "audit", {
    data: { at: new Date().toISOString() },
  });
}
```

execution token 来源优先级：`opts.executionToken` 显式参数 > 环境变量 `TW_EXECUTION_TOKEN`（runner 在函数执行期注入）。**并发（concurrency > 1）函数请走 ctx / 参数通道**，不要读 `process.env.TW_EXECUTION_TOKEN`——async 恢复后可能读到其他请求的 token（凭证串号窗口，见 functions-v3.md 安全声明 §1）。

本地调试可用 `Torchwood.fromExecution("http://127.0.0.1:9080", { executionToken: "..." })` 显式传入；或配合 `torchwood functions dev --token <execution-token>` 起本地 runner。

> 发布形态说明：函数内 SDK 与主包同源（同一个 `@torchwood/sdk`），**暂不拆分独立 npm 包**——避免 monorepo 双包构建（对 functions-v3.md §5.1 的一处已知偏离，拆包后置）。

## OAuth2 浏览器流

浏览器中的 OAuth2 登录（Google / GitHub 等）走网关的 302 发起端点，**整页跳转而非 fetch**——发起时网关在 API 域种回调 nonce cookie，跨源 fetch 会把它丢掉导致回调必败：

```typescript
// 1. 发起：同步拼出 authorize 地址，整页跳转（网关种 nonce cookie 后 302 到 provider 授权页）
window.location.href = client.account.buildOAuth2AuthorizeURL({
  provider: "github",
  success: "https://app.example.com/oauth/callback", // 回调落地点，需在项目回跳白名单内
  failure: "https://app.example.com/login?error=oauth_failed",
});

// 2. 回调页：解析重定向 fragment（#access_token=…&userId=…）建立本地会话
const parsed = parseOAuth2CallbackFragment(window.location.hash);
if (parsed?.type === "signed_in") {
  client.setAccessToken(parsed.accessToken);
  window.history.replaceState(null, "", "/oauth/callback"); // 清掉 fragment，避免 token 留在历史
} else if (parsed?.type === "mfa_required") {
  // 账号启用了 MFA：携带 parsed.challengeToken 走 createMFASession 二次认证
}
```

fragment 刻意不含 refresh_token（安全加固）；OAuth 会话的 access_token 过期请引导用户重新登录。

`createOAuth2Session` / `createOAuth2LinkSession` 已标记 `@deprecated`（保留但不再适用于浏览器流，原因见方法注释）；`createOAuth2TokenSession` 仅适用于 state 未被网关 GET 回调消费的定制流程。

## API surface

**Client API**（Bearer JWT）：`account`（注册/登录/会话/偏好）、`databases`（文档 CRUD + count）、`groups` / memberships、`realtime`（WebSocket 订阅）、`assets`、`payments`、`subscriptions`。

**Server API**（API Key）：`health`、`projects`、`users`、`groups`、`databases`（库/集合/属性/索引/文档/Bulk）、`apiKeys`、`storage`（Bucket/File）、`functions`、`oauthProviders`、`outbox`、`auditLogs`、`assets`、`payments`、`subscriptions`、`billing`。

## Agent 工具目录

SDK 内置 18 个 Agent 默认工具映射（动词 → Server RPC），供 LLM Agent / MCP Tool Server 使用：

```typescript
import { agentTools, lookupAgentTool } from "@torchwood/sdk";

const tool = lookupAgentTool("list_users"); // { name, description, method, schema }
```

完整工具清单见 [`docs/developer/14-agent-tools.md`](https://github.com/torchwoodcloud/torchwood/blob/main/docs/developer/14-agent-tools.md)。

## 更多文档

- SDK 总览与 Web 演示站点：[`sdk/README.md`](https://github.com/torchwoodcloud/torchwood/blob/main/sdk/README.md)
- SDK 开发指南：[`docs/developer/12-sdk.md`](https://github.com/torchwoodcloud/torchwood/blob/main/docs/developer/12-sdk.md)
- OpenAPI 定义：`task generate:proto` 后在 `genproto/**/*.swagger.json` 获取

## License

MIT
