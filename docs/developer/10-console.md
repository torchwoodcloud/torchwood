# Torchwood Console 前端开发指南

> 面向需在 Admin Console 新增页面的开发者。Console 为 React 19 + Vite 8 + TanStack Query + shadcn/ui 的管理后台，产物经 `go:embed` 打进二进制，由 `internal/runtime/console.go` 在 `/console/` 下 serve。
> 目标读者：前端开发者。关联：`AGENTS.md`、`docs/developer/09-api-guide.md`。
> 修订记录：2026-08-23 重写（以 `console/package.json`、`console/embed.go`、`console/src/api/client.ts`、`internal/api/consolegrpc/cookies.go` 为准）。

---

## 1. 技术栈（以 `console/package.json` 为准）

| 依赖 | 版本 | 用途 |
|------|------|------|
| `react` / `react-dom` | `^19.2.6` | UI 框架 |
| `react-router-dom` | `^7.18.0` | 路由（`BrowserRouter` + 嵌套路由，见 `console/src/App.tsx:2`） |
| `@tanstack/react-query` | `^5.101.0` | 服务端状态（`useQuery`/`useMutation`/`useQueryClient`） |
| `axios` | `^1.18.0` | HTTP 客户端（`console/src/api/client.ts:11`） |
| `sonner` | `^2.0.7` | toast |
| `lucide-react` | `^1.20.0` | 图标 |
| `tailwindcss` + `tailwindcss-animate` | `^3.4.19` | 样式 |
| `class-variance-authority` + `clsx` + `tailwind-merge` | — | `cn()`（`console/src/lib/utils.ts`） |
| `@radix-ui/react-*` | `^1.x`/`^2.x` | 无头组件（dialog/select/label/avatar 等） |
| `typescript` | `~6.0.2` | 类型检查（`tsc -b`） |
| `vite` | `^8.0.12` | 构建/开发服务器 |
| `pnpm` | `11.20.0` | 包管理器（`packageManager` 字段锁定） |

---

## 2. 目录结构

```
console/
├── embed.go                          # //go:embed dist → console.Dist
├── package.json / vite.config.ts / tailwind.config.js / eslint.config.js
├── dist/                             # pnpm run build 产物（Go embed 打包）
└── src/
    ├── main.tsx                      # createRoot + StrictMode
    ├── App.tsx                       # QueryClientProvider + AuthProvider + BrowserRouter + 路由表
    ├── api/
    │   ├── client.ts                 # axios 实例 + 拦截器（见 §3）
    │   ├── auth.ts                   # sign-in / sign-out / setup-status / sign-up
    │   └── admins.ts / projects.ts / users.ts / groups.ts / databases.ts / storage.ts / functions.ts / oauthProviders.ts
    ├── components/
    │   ├── Layout.tsx                # 侧边栏 + 顶部栏 + 项目选择器 + Outlet
    │   ├── ProjectBootstrap.tsx      # 自动选中默认项目（保证 X-Torchwood-Project）
    │   ├── ProjectSelector.tsx / PageHeader.tsx / EmptyState.tsx / LoadingTable.tsx
    │   ├── ConfirmDialog.tsx / FormPage.tsx / ErrorBoundary.tsx
    │   ├── list/                     # DataTable / ListToolbar / ResourceListPage
    │   ├── resource/                 # PermissionEditor / shared（行操作）
    │   └── ui/                       # shadcn/ui 原语（必须放于此）
    │       ├── button.tsx / card.tsx / input.tsx / label.tsx / select.tsx
    │       ├── dialog.tsx / badge.tsx / table.tsx / checkbox.tsx / skeleton.tsx / progress.tsx
    ├── hooks/
    │   ├── useAuth.tsx               # 会话状态（refresh 探测 + login/logout）
    │   ├── useAdminRole.ts           # 角色守卫（canWrite / isPlatformAdmin）
    │   ├── useListParams.ts          # 列表 URL 参数 q/page/pageSize
    │   └── useRowSelection.ts
    ├── lib/utils.ts                  # cn()
    └── routes/                       # 按资源分目录
        ├── Login.tsx / Dashboard.tsx
        ├── admins/ / api-keys/ / databases/ / functions/ / projects/
        ├── settings/ / storage/ / groups/ / users/ / payments/ / assets/ / subscriptions/
```

约定：

- 路由页面按 `routes/<resource>/pages.tsx` 组织，一资源一目录；
- shadcn 风格原语必须放在 `console/src/components/ui/`（见 `AGENTS.md`）；
- 页面不得直连 `axios`，一律通过 `src/api/` 封装。

---

## 3. API Client（`console/src/api/client.ts`）

### 3.1 实例与请求头

```ts
export const api = axios.create({ baseURL: "/v1", headers: { "Content-Type": "application/json" } });

api.interceptors.request.use((config) => {
  const projectID = getProjectID(); // localStorage: TORCHWOOD_console_project
  if (projectID) config.headers["X-Torchwood-Project"] = projectID;
  return config;
});
```

- 会话凭证**不在 JS**：由 `TORCHWOOD_session_console` + `TORCHWOOD_console_refresh` 两个 HttpOnly cookie 自动携带（见 §4），`client.ts:8` 会清理迁移前残留的旧 token；
- 项目上下文：`X-Torchwood-Project` 头由 `setProjectID`/`getProjectID` 读写；
- `baseURL` 为 `/v1`，与 `google.api.http` 注解一致。

### 3.2 401 自动刷新（single-flight）

`refreshAuthTokenSingleFlight()`（`client.ts:36`）用裸 `axios` 直调 `POST /v1/console/auth/refresh`（空 body，服务端读 cookie），并发 401 共享同一 Promise，避免雪崩。

响应拦截器流程（`client.ts:82`）：

1. 登录请求 `/console/auth/sign-in` 401 → 直接 `toast.error`；
2. 其他 401 且未标记 `__skipAuthRetry`/`__authRetried` 且非 `missing project context` → 刷新一次后带 `__authRetried` 重试原请求；刷新失败 → `forceReLogin()` 跳 `/console/login`；
3. 其它 `>=400` 且非 `__skipToast` → 从 `error.response.data.error.message` 取后端消息 `toast.error`。

自定义标记（`ApiRequestConfig`）：`__skipAuthRetry`（如 sign-out）、`__authRetried`（防重入）、`__skipToast`（调用方自渲染错误）。

---

## 4. 认证：`TORCHWOOD_session_console` HttpOnly Cookie

### 4.1 服务端（`internal/api/consolegrpc/`）

| Cookie | Path | 属性 | 说明 |
|--------|------|------|------|
| `TORCHWOOD_session_console` | `/` | `HttpOnly` + `SameSite=Lax` | access token（`consolegrpc/cookies.go:25`） |
| `TORCHWOOD_console_refresh` | `/v1/console/auth` | `HttpOnly` + `SameSite=Lax` | refresh token，仅发往刷新端点（`cookies.go:28`） |

- 签发：`POST /v1/console/auth/sign-in`（`auth.go:23`）与 `POST /v1/console/auth/refresh` 成功后 `setSessionCookies`；
- `Set-Cookie` 经 `internal/runtime/grpc_gateway.go` 的 `authOutgoingHeaderMatcher` 透传；
- CSRF 防护：`SameSite=Lax` 使跨站 POST 不携带 cookie；本服务变更类端点均为 POST，故无需额外 CSRF token（`console.go:32` 注释）；
- `Secure` 由 `console.Auth.SecureCookies()` 决定（非本地环境自动启用）。

### 4.2 前端（`console/src/hooks/useAuth.tsx` + `console/src/api/auth.ts`）

- 挂载时 `refreshSession()`（即 single-flight refresh）探测会话：成功 → `isAuthenticated=true`（顺带续期），失败 → 匿名；`loading=true` 期间 `RequireAuth` 返回 `null` 避免闪屏；
- `login(email,password)` 调 `POST /console/auth/sign-in`，成功后置 `authenticated`，`__skipToast` 让登录页自渲染错误；
- `logout()` 调 `POST /console/auth/sign-out`（`__skipAuthRetry`），无论成败清空 `TORCHWOOD_console_project` 与 `queryClient`；
- 路由守卫：`RequireAuth` 判登录；`RequireRole`（`App.tsx:167`）判写权限（`canWrite`）或平台管理员（`isPlatformAdmin`），失败重定向 `/console`。

---

## 5. 开发与构建

```bash
task console:install   # pnpm install（锁定 pnpm@11.20.0）
task console:dev       # pnpm run dev → vite dev server
task console:build     # pnpm run build → tsc -b && vite build → dist/
task build             # 依赖 console:build → go build ./cmd/server ./cmd/worker ./cmd/client
```

- `vite.config.ts:8`：`base: '/console/'`，`@` → `./src`（tsconfig + vite 双别名）；
- `vite.config.ts:20`：`server.proxy['/v1'] → http://localhost:9099`（与 `configs/config.yaml.template` 的 `server.http.addr` 对齐），保证 dev 下 `/v1` 同源，HttpOnly cookie 正常工作；
- `console/embed.go:8`：`//go:embed dist` → `console.Dist`，由 `internal/runtime/console.go` 的 `NewConsoleHandler` 挂载，SPA fallback（未知路径回 `index.html`）+ 安全头（`X-Frame-Options: DENY` / CSP / `X-Content-Type-Options`）。

> **必做**：修改 Console 后先 `task console:build` 再 `task build`，否则 `go:embed` 打包旧 `dist/`。

---

## 6. 新增页面流程（以 `admins` 为模板）

### 6.1 API 封装（`src/api/<resource>.ts`）

```ts
export async function listAdmins(): Promise<Admin[]> {
  const res = await api.get<ListAdminsResponse>("/console/admins");
  return res.data.admins ?? [];
}
```

路径与后端 `google.api.http` 一致（`/server/projects`、`/console/admins`），JSON 字段为 snake_case（后端 `CustomMarshaler` 设 `UseProtoNames: true`）。

### 6.2 页面（`src/routes/<resource>/pages.tsx`）

复用 `ResourceListPage`（搜索/分页/空态/骨架内置），仅提供列定义与行操作：

```tsx
const columns: ColumnDef<Admin>[] = [
  { key: "email", header: "邮箱", cell: (a) => a.email },
  { key: "role",  header: "角色", cell: (a) => <Badge>{a.role}</Badge> },
];
export function AdminsListPage() {
  const { data: admins = [], isLoading } = useQuery({ queryKey: ["console-admins"], queryFn: listAdmins });
  return <ResourceListPage title="系统管理员" isLoading={isLoading} items={admins} columns={columns}
    getSearchText={(a) => `${a.email} ${a.role}`} />;
}
```

弹窗表单用 `Dialog`（Radix）+ `useMutation`，成功 `toast.success` + `queryClient.invalidateQueries`。

### 6.3 注册路由（`src/App.tsx`）

```tsx
import { AdminsListPage } from "@/routes/admins/pages";
<Route path="admins" element={<RouteErrorBoundary><AdminsListPage /></RouteErrorBoundary>} />
```

所有业务页挂在 `<RequireAuth><Layout/></RequireAuth>` 下的 `/console` 布局路由；写路由外层再包 `<RequireRole>`。

### 6.4 接入菜单（`src/components/Layout.tsx:14`）

在 `navSections` 按分组追加：

```ts
{ title: "System", items: [
  { to: "/console/admins", label: "Admins", icon: ShieldCheck },
]}
```

### 6.5 验证

```bash
task console:build && task build
# 或 task console:dev 手工走查：登录 → 切换项目 → 列表/创建/删除 → 刷新鉴权
```

---

## 7. 常见坑

1. **忘了重构建**：`go:embed` 只打包构建时刻的 `dist/`；
2. **dev cookie 失效**：`vite.config.ts` 代理须与后端 `server.http.addr` 同源（默认 `9099`）；
3. **空列表兜底**：`res.data.xxx ?? []`，否则空态崩溃；
4. **错误消息**：从 `error.response.data.error.message` 读取；
5. **绕过 `api` 实例**：仅 refresh 用裸 `axios`，其余一律走 `api` 以带 `X-Torchwood-Project` 与 401 刷新；
6. **权限在页面/路由双 gating**：按钮用 `useAdminRole`，写路由用 `RequireRole`。
