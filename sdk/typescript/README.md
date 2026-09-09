# @torchwood/sdk

Torchwood 的官方 TypeScript SDK，封装 **Client API**（用户 JWT）与 **Server API**（scoped API Key + `X-Torchwood-Project`），以类型安全的方式调用 [Torchwood](https://github.com/torchwooddev/torchwood) 后端——适合前端应用、自动化脚本与 LLM Agent 集成。

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

**Server API**（API Key）：`health`、`projects`、`users`、`groups`、`databases`（库/集合/属性/索引/文档/Bulk）、`apiKeys`、`storage`（Bucket/File）、`functions`、`oauthProviders`、`outbox`、`assets`、`payments`、`subscriptions`、`billing`。

## Agent 工具目录

SDK 内置 18 个 Agent 默认工具映射（动词 → Server RPC），供 LLM Agent / MCP Tool Server 使用：

```typescript
import { agentTools, lookupAgentTool } from "@torchwood/sdk";

const tool = lookupAgentTool("list_users"); // { name, description, method, schema }
```

完整工具清单见 [`docs/developer/14-agent-tools.md`](https://github.com/torchwooddev/torchwood/blob/main/docs/developer/14-agent-tools.md)。

## 更多文档

- SDK 总览与 Web 演示站点：[`sdk/README.md`](https://github.com/torchwooddev/torchwood/blob/main/sdk/README.md)
- SDK 开发指南：[`docs/developer/12-sdk.md`](https://github.com/torchwooddev/torchwood/blob/main/docs/developer/12-sdk.md)
- OpenAPI 定义：`task generate:proto` 后在 `genproto/**/*.swagger.json` 获取

## License

MIT
