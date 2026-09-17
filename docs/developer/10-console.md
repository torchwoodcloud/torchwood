# 10 Console 前端开发指南

面向需要在 Admin Console 新增页面的开发者。Console 是 React + Vite + TanStack Query + shadcn/ui 的管理后台，构建产物经 `go:embed` 打进 Go 二进制，由 `cmd/server/internal/runtime/console.go` 在 `/console/` 路径下 serve。

> 关联：`AGENTS.md`（组件目录约定）、`09-api-guide.md`（后端 RPC 流程）。

## 1. 技术栈

以 `console/package.json` 为准：

| 依赖 | 用途 |
|------|------|
| `react` / `react-dom` 19 | UI 框架 |
| `react-router-dom` 7 | 路由（BrowserRouter + 嵌套路由） |
| `@tanstack/react-query` 5 | 服务端状态（useQuery / useMutation / useQueryClient） |
| `axios` | HTTP 客户端（`console/src/api/client.ts`） |
| `tailwindcss` 3 + `tailwindcss-animate` | 样式 |
| `@radix-ui/react-*` | 无头组件（dialog / select / label / avatar 等） |
| `class-variance-authority` + `clsx` + `tailwind-merge` | `cn()`（`console/src/lib/utils.ts`） |
| `sonner` / `lucide-react` | toast / 图标 |
| `recharts` | 图表（Analytics 页面） |
| `typescript` 6 + `vite` 8 | 类型检查（`tsc -b`）/ 构建 |
| `vitest` + `@testing-library/*` + `jsdom` | 前端测试（`vitest run`） |
| `pnpm` 11.20 | 包管理器（packageManager 字段锁定） |

## 2. 目录结构

```
console/
├── embed.go                          # //go:embed dist → console.Dist
├── package.json / vite.config.ts / tailwind.config.js / eslint.config.js
├── dist/                             # 构建产物（gitignore），仅 dist/.gitkeep 入库作 embed 占位
└── src/
    ├── main.tsx                      # createRoot + StrictMode
    ├── App.tsx                       # QueryClientProvider + AuthProvider + BrowserRouter + 路由表
    ├── api/
    │   ├── client.ts                 # axios 实例 + 拦截器（§3）
    │   ├── auth.ts                   # sign-in / sign-out / setup-status / sign-up
    │   ├── wellknown.ts              # /.well-known/torchwood 目录
    │   └── admins.ts / projects.ts / users.ts / groups.ts / databases.ts / storage.ts
    │       / functions.ts / oauthProviders.ts / apiKeys.ts / payments.ts / assets.ts
    │       / subscriptions.ts / auditLogs.ts / leaderboards.ts / analytics.ts / realtime.ts
    ├── components/
    │   ├── Layout.tsx                # 侧边栏 + 顶部栏 + 项目选择器 + Outlet
    │   ├── ProjectBootstrap.tsx      # 自动选中默认项目（保证 X-Torchwood-Project）
    │   ├── AdminRoleBadge.tsx        # 管理员角色徽章（账户资料 / 管理员列表共用）
    │   ├── ProjectSelector.tsx / PageHeader.tsx / EmptyState.tsx / LoadingTable.tsx
    │   ├── ConfirmDialog.tsx / FormPage.tsx / ErrorBoundary.tsx
    │   ├── list/                     # DataTable / ListToolbar / ResourceListPage
    │   ├── resource/                 # PermissionEditor / shared（行操作）
    │   └── ui/                       # shadcn/ui 原语（必须放于此）
    ├── hooks/
    │   ├── useAuth.tsx               # 会话状态（refresh 探测 + login/logout）
    │   ├── useAdminRole.ts           # 角色守卫（canWrite / isPlatformAdmin）
    │   ├── useTimezone.ts            # 当前管理员生效时区（§4.3）
    │   ├── useListParams.ts          # 列表 URL 参数 q/page/pageSize
    │   ├── useProjectScopeSync.ts    # 路由 ↔ 项目上下文同步
    │   └── useRowSelection.ts
    ├── lib/
    │   ├── utils.ts                  # cn()
    │   └── datetime.ts               # 按用户时区的 formatDateTime / formatDate
    └── routes/                       # 按资源分目录，一资源一目录
        ├── Login.tsx / Dashboard.tsx
        ├── admins/ / api-keys/ / databases/ / functions/ / projects/
        ├── account/                  # 账户设置区（Profile / Preferences 子页）
        ├── settings/ / storage/ / groups/ / users/ / payments/ / assets/ / subscriptions/
        ├── leaderboards/             # 榜列表 + 建榜/编辑对话框 + 榜详情
        ├── analytics/                # 概览 / 事件字典 / 留存网格 / 单用户行为流
        └── audit-logs/               # 平台审计日志查询页
```

三条约定：

1. 路由页面按 `routes/<resource>/pages.tsx` 组织，一资源一目录；
2. shadcn 风格原语必须放在 `console/src/components/ui/`（`AGENTS.md` 约定）；
3. 页面不得直连 `axios`，一律通过 `src/api/` 封装。

## 3. API Client

### 3.1 实例与请求头

```ts
export const api = axios.create({ baseURL: "/v1", headers: { "Content-Type": "application/json" } });

api.interceptors.request.use((config) => {
  const projectID = getProjectID(); // localStorage: TORCHWOOD_console_project
  if (projectID) config.headers["X-Torchwood-Project"] = projectID;
  return config;
});
```

- **会话凭证不在 JS**：由 `TORCHWOOD_session_console` + `TORCHWOOD_console_refresh` 两个 HttpOnly cookie 自动携带（§4）；前端不使用 localStorage 存 token。
- 项目上下文：`X-Torchwood-Project` 头由 `setProjectID` / `getProjectID` 读写。
- `baseURL` 为 `/v1`，与 proto 的 `google.api.http` 注解一致；JSON 字段为 snake_case（后端 marshaler `UseProtoNames: true`）。

### 3.2 401 自动刷新（single-flight）

`refreshAuthTokenSingleFlight()` 用裸 `axios` 直调 `POST /v1/console/auth/refresh`（空 body，服务端读 refresh cookie），并发 401 共享同一 Promise，避免刷新雪崩。

响应拦截器流程：

1. 登录请求 `/console/auth/sign-in` 返回 401 → 直接 `toast.error`；
2. 其他 401 且未标记 `__skipAuthRetry` / `__authRetried` 且非 `missing project context` → 刷新一次后带 `__authRetried` 重试原请求；刷新失败 → `forceReLogin()` 跳 `/console/login`；
3. 其它 ≥400 且非 `__skipToast` → 从 `error.response.data.error.message` 取后端消息 `toast.error`。

自定义标记（`ApiRequestConfig`）：`__skipAuthRetry`（如 sign-out）、`__authRetried`（防重入）、`__skipToast`（调用方自渲染错误）。

## 4. 认证与偏好

### 4.1 服务端 cookie

| Cookie | Path | 属性 | 说明 |
|--------|------|------|------|
| `TORCHWOOD_session_console` | `/` | HttpOnly + SameSite=Lax | access token |
| `TORCHWOOD_console_refresh` | `/v1/console/auth` | HttpOnly + SameSite=Lax | refresh token，仅发往刷新端点 |

- 签发：sign-in 与 refresh 成功后 `setSessionCookies`；`Set-Cookie` 经 gateway 的 outgoing header matcher 透传。
- CSRF 防护：SameSite=Lax 使跨站 POST 不携带 cookie；本服务变更类端点均为 POST，无需额外 CSRF token。
- `Secure` 由 `console.Auth.SecureCookies()` 决定（非本地环境自动启用）。

### 4.2 前端会话流

- 挂载时 `refreshSession()`（即 single-flight refresh）探测会话：成功 → `isAuthenticated=true`（顺带续期），失败 → 匿名；`loading=true` 期间 `RequireAuth` 返回 null 避免闪屏。
- `login(email, password)` 调 sign-in，成功置 authenticated；登录错误 `__skipToast` 让登录页自渲染。
- `logout()` 调 sign-out（`__skipAuthRetry`），无论成败清空项目选择与 `queryClient`。
- 路由守卫：`RequireAuth` 判登录；`RequireRole` 判写权限（`canWrite`）或平台管理员（`isPlatformAdmin`），失败重定向 `/console`。

### 4.3 管理员时区偏好

`admins.metadata` JSONB 存管理员自助偏好（首键 `timezone`，IANA 时区名），经 `UpdateCurrentAdmin` 自助 RPC 修改（Console 账户设置页 `/console/account/preferences`，侧栏底部账户入口进入）。

- `useUserTimezone()`（`console/src/hooks/useTimezone.ts`）返回当前管理员生效时区：`admins/me` 的偏好 → 浏览器时区回退；与 `useAdminRole` 共享 `["console-admin-me"]` 查询缓存——偏好保存后 `setQueryData` 该 key，全部消费组件随重渲染拿到新时区。
- **全站时间显示统一走 `lib/datetime.ts` 的 `formatDateTime` / `formatDate`**（按用户时区格式化；空值 / 无法解析返回 `—`）。新页面不要用 `dayjs(...).format()` 之类的本地时区格式化。

## 5. 开发与构建

```bash
task console:install   # pnpm install
task console:dev       # vite dev server
task console:build     # tsc -b && vite build → dist/
task build             # 依赖 console:build → go build 四二进制
```

- `vite.config.ts`：`base: '/console/'`，`@` 别名 → `./src`（tsconfig + vite 双处声明）。
- dev 代理：`server.proxy['/v1'] → http://localhost:9080`（与 `server.http.addr` 对齐），保证 dev 下 `/v1` 同源、HttpOnly cookie 正常工作。
- `console/embed.go` 的 `//go:embed dist` 由 runtime 的 `NewConsoleHandler` 挂载：SPA fallback（未知路径回 index.html）+ 安全头（X-Frame-Options / CSP / X-Content-Type-Options）。

> **必做**：修改 Console 后先 `task console:build` 再 `task build`，否则 `go:embed` 打包旧 `dist/`。

## 6. 新增页面流程（以 admins 为模板）

### 6.1 API 封装（src/api/&lt;resource&gt;.ts）

```ts
export async function listAdmins(): Promise<Admin[]> {
  const res = await api.get<ListAdminsResponse>("/console/admins");
  return res.data.admins ?? [];
}
```

路径与后端 `google.api.http` 一致；**空列表兜底 `?? []`**，否则空态崩溃。

### 6.2 页面（src/routes/&lt;resource&gt;/pages.tsx）

复用 `ResourceListPage`（搜索 / 分页 / 空态 / 骨架内置），仅提供列定义与行操作：

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

弹窗表单用 `Dialog`（Radix）+ `useMutation`，成功 `toast.success` + `queryClient.invalidateQueries`。时间列用 `formatDateTime`（§4.3）。

### 6.3 注册路由（src/App.tsx）

```tsx
import { AdminsListPage } from "@/routes/admins/pages";
<Route path="admins" element={<RouteErrorBoundary><AdminsListPage /></RouteErrorBoundary>} />
```

所有业务页挂在 `<RequireAuth><Layout/></RequireAuth>` 下的 `/console` 布局路由；写路由外层再包 `<RequireRole>`。

### 6.4 接入菜单（src/components/Layout.tsx）

在 `navSections` 按分组追加条目（title + items[{to, label, icon}]）。

### 6.5 验证

```bash
task console:build && task build
# 或 task console:dev 手工走查：登录 → 切换项目 → 列表/创建/删除 → 刷新鉴权
```

## 7. 常见坑

1. **忘了重构建**：`go:embed` 只打包构建时刻的 `dist/`；
2. **dev cookie 失效**：vite 代理必须与后端 `server.http.addr` 同源（默认 9080）；
3. **空列表兜底**：`res.data.xxx ?? []`；
4. **错误消息**：从 `error.response.data.error.message` 读取；
5. **绕过 `api` 实例**：仅 refresh 用裸 axios，其余一律走 `api`（带项目头与 401 刷新）；
6. **权限双 gating**：按钮用 `useAdminRole`，写路由用 `RequireRole`；
7. **时间显示**：用 `lib/datetime` 的用户时区格式化，勿本地化。

## 8. 项目治理面板

项目详情页（`/console/projects/:id`）在基础信息之下提供两块治理面板：

| 面板 | 能力 | 后端 |
|------|------|------|
| **注册策略** | `open`（默认）/ `invite_only` / `closed` 三档切换，即改即存 | `UpdateProject`（registration_policy） |
| **邀请码** | 生成（次数 1..10000 或不限、可选过期时间）、列表回显（`twi_` 明文 + 已用次数 + 过期 / 吊销状态）、复制、吊销 | `CreateInviteCode` / `ListInviteCodes` / `DeleteInviteCode`（owner/admin） |
| **Redirect Allowlist** | OAuth 重定向白名单整表编辑 | `PUT /v1/server/projects/{id}/oauth-redirect-allowlist`（owner/admin） |

API Keys 页详情提供**编辑**（name / scopes / enabled / expire_at，proto3 optional 只提交变更字段）与**轮换**引导（三步：新建同 scope 的 `-rotated` key → 应用切换 → 旧 key 设过期）。scope 输入支持资源级语法（如 `databases:blog.read`，见 `05-authentication.md` §6）。

## 9. 页面全景

侧边栏分组：Dashboard 置顶；Develop（API Keys / Databases / Storage / Functions / Analytics）、Auth（Users / Groups）、Economy（Orders / Assets / Subscriptions / Leaderboards）、System（Projects / Admins / Audit Logs）。

| 路由 | 内容 | 守卫 |
|------|------|------|
| `/console/account`（index → profile） | 账户设置区（侧栏底部邮箱进入）：`profile` = 账户资料（邮箱 / 角色 / 创建时间，只读）；`preferences` = 时区偏好（选中即暂存 + 显式保存，清除 = 跟随浏览器） | 全角色开放（自助面） |
| `/console/leaderboards` + `/:boardId` | 榜列表 + 建榜 / 编辑 / 删除；详情 = 期下拉 + top 表（rank/position/subject/value/updated_at，行删条目）+ 按 subject 查条目；board 表单含 Phase 2 rewards 编辑器 | 写操作 owner |
| `/console/analytics`（+ events / events/:name / retention / users/:userId） | 事件分析：概览 / 事件字典 / 事件详情 / 留存网格 / 单用户行为流（recharts） | 全角色开放 |
| `/console/audit-logs` | 平台审计日志查询（结构化过滤 + metadata 视图） | platformAdmin |

用户详情页头部提供直达入口：行为轨迹（analytics）、用户资产（`/console/assets/users?owner=`）、用户订阅（`/console/subscriptions?q=`）——资产 / 订阅入口仅 platformAdmin 可见。
