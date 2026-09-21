# 10 Console 前端开发指南

面向需要在 Admin Console 新增页面或改动其基础设施的开发者。Console 是 React + Vite + TanStack Query + shadcn/ui 的管理后台 SPA，构建产物经 `go:embed` 打进 Go 二进制，由 `cmd/server/internal/runtime/console.go` 在 `/console/` 路径下 serve。

> 关联：`AGENTS.md`（组件目录约定）、`09-api-guide.md`（后端 RPC 约定）、文末「相关文档」。

## 1. 技术栈

以 `console/package.json` 为准：

| 依赖 | 用途 |
|------|------|
| `react` / `react-dom` 19 | UI 框架 |
| `react-router-dom` 7 | 路由（BrowserRouter + 嵌套路由，页面级 `React.lazy` 分包） |
| `@tanstack/react-query` 5 | 服务端状态（useQuery / useMutation / useQueryClient） |
| `axios` | HTTP 客户端（`console/src/api/client.ts`） |
| `tailwindcss` 3 + `tailwindcss-animate` | 样式 |
| `@radix-ui/react-*` | 无头组件（dialog / select / label / avatar / dropdown-menu / toast 等） |
| `class-variance-authority` + `clsx` + `tailwind-merge` | `cn()`（`console/src/lib/utils.ts`） |
| `sonner` / `lucide-react` | toast / 图标 |
| `recharts` | 图表（Analytics 页面） |
| `typescript` 6 + `vite` 8 | 类型检查（`tsc -b`）/ 构建 |
| `vitest` + `@testing-library/*` + `jsdom` | 前端测试（`pnpm test`，测试与被测文件同目录 `*.test.tsx`） |
| `pnpm` 11.20 | 包管理器（packageManager 字段锁定） |

QueryClient 全局默认 `retry: 1`、`refetchOnWindowFocus: false`（`src/App.tsx`）。

## 2. 目录结构

```
console/
├── embed.go                          # //go:embed all:dist → console.Dist
├── package.json / vite.config.ts / tailwind.config.js / postcss.config.js / eslint.config.js / tsconfig*
├── dist/                             # 构建产物（gitignore），仅 dist/.gitkeep 入库作 embed 占位
└── src/
    ├── main.tsx                      # createRoot + StrictMode
    ├── App.tsx                       # QueryClientProvider + AuthProvider + BrowserRouter；
    │                                 #   全部路由 React.lazy 分包 + RouteErrorBoundary（按路径重置）
    ├── api/                          # 一资源一文件，页面不得直连 axios
    │   ├── client.ts                 # axios 实例 + 拦截器 + 项目作用域（§3）
    │   ├── auth.ts                   # sign-in / sign-out / refresh 探测 / setup-status / sign-up
    │   ├── wellknown.ts              # /.well-known/torchwood 目录（裸 axios；API Key scope 字典）
    │   ├── realtime.ts               # /v1/realtime WebSocket 试听客户端（cookie 握手）
    │   └── admins / analytics / apiKeys / assets / auditLogs / databases / functions / groups
    │       / leaderboards / oauthProviders / payments / projects / runtimeVars / storage
    │       / subscriptions / users
    ├── components/
    │   ├── Layout.tsx                # 侧边栏（可收起）+ 面包屑顶栏 + Outlet；菜单数据来自 lib/nav
    │   ├── ProjectBootstrap.tsx      # 兜底选中默认项目（保证 X-Torchwood-Project）
    │   ├── ProjectSelector.tsx       # 项目选择器（支持侧栏收起态）
    │   ├── TimeBadge.tsx             # 顶栏时钟（管理员时区 + UTC 偏移）
    │   ├── AdminRoleBadge.tsx / ConfirmDialog.tsx / EmptyState.tsx / ErrorBoundary.tsx
    │   │   / LoadingTable.tsx / PageCard.tsx / PageHeader.tsx
    │   ├── list/                     # DataTable（ColumnDef）/ ListToolbar（含 SelectionBar、
    │   │                             #   ListPagination）/ ResourceListPage
    │   ├── resource/                 # PermissionEditor / permission-presets / shared（FormPageWrapper
    │   │                             #   / DetailPageWrapper / DetailGrid / FormField / DetailSkeleton
    │   │                             #   / NotFound / DeleteButton / BulkDeleteButton / RowDeleteButton）
    │   └── ui/                       # shadcn/ui 原语（必须放于此）
    ├── hooks/
    │   ├── useAuth.tsx               # 会话状态 + 项目作用域（含跨标签 storage 同步）
    │   ├── useAdminRole.ts           # 角色判定（canWrite / isPlatformAdmin，fail-closed）
    │   ├── useTimezone.ts            # 当前管理员生效时区（§4.3）
    │   ├── useListParams.ts          # 列表 URL 参数 q/page/pageSize（+ 任意扩展键）
    │   ├── useProjectScopeSync.ts    # URL :id ↔ 全局项目选择同步（§7.8）
    │   └── useRowSelection.ts        # 表格行多选
    ├── lib/
    │   ├── utils.ts                  # cn()
    │   ├── datetime.ts               # 用户时区格式化 + datetime-local 互转（§4.3）
    │   ├── nav.ts                    # navSections（侧栏菜单）+ 图标按路径派生
    │   └── routeTitles.ts            # 面包屑文案与路径映射
    └── routes/                       # 按资源分目录，一资源一目录
        ├── Login.tsx / Dashboard.tsx
        ├── account/                  # 账户设置区（AccountLayout + Profile / Preferences 子页）
        ├── admins/ / api-keys/ / assets/ / audit-logs/ / groups/ / leaderboards/ / payments/
        │   / projects/（pages + settings）/ runtime-vars/ / settings/（重定向页）/ storage/
        │   / subscriptions/ / users/
        ├── databases/                # pages + CollectionLayout + ListenPanel（Realtime 试听）
        ├── functions/                # pages + triggers-card 等
        └── analytics/                # 概览 / 事件字典 / 事件详情 / 留存网格 / 单用户行为流
```

四条约定：

1. 路由页面按 `routes/<resource>/pages.tsx` 组织，一资源一目录；
2. shadcn 风格原语必须放在 `console/src/components/ui/`（`AGENTS.md` 约定）；
3. 页面不得直连 `axios`，一律通过 `src/api/` 封装（裸 axios 的合法例外见 §3.2 末）；
4. 测试与被测代码同目录，`pnpm test`（vitest run）执行。

## 3. API Client

### 3.1 实例与项目作用域

```ts
export const api = axios.create({ baseURL: "/v1", headers: { "Content-Type": "application/json" } });

api.interceptors.request.use((config) => {
  const projectID = getProjectID(); // localStorage: TORCHWOOD_console_project
  if (projectID) config.headers["X-Torchwood-Project"] = projectID;
  return config;
});
```

- **会话凭证不在 JS**：由 `TORCHWOOD_session_console` + `TORCHWOOD_console_refresh` 两个 HttpOnly cookie 自动携带（§4.1）；前端不读写 token，模块加载时清理迁移前残留的旧 localStorage token 键。
- 项目上下文：`X-Torchwood-Project` 头由 `setProjectID` / `getProjectID` 读写 localStorage 键 `TORCHWOOD_console_project`（导出常量 `PROJECT_STORAGE_KEY`）；跨标签同步见 §4.2 末条。
- `baseURL` 为 `/v1`，与 proto 的 `google.api.http` 注解一致；JSON 字段为 snake_case（网关 marshaler `UseProtoNames: true`）。
- int64 字段经 grpc-gateway protojson 输出为字符串；`api/runtimeVars.ts` 在数据层统一归一为 number。

### 3.2 401 自动刷新（single-flight）

`refreshAuthTokenSingleFlight()` 用裸 `axios` 直调 `POST /v1/console/auth/refresh`（空 body，服务端读 refresh cookie），并发 401 共享同一 Promise，避免刷新雪崩。

响应拦截器流程（`src/api/client.ts`）：

1. 登录请求 `/console/auth/sign-in` 返回 401 → 尊重 `__skipToast`（登录页自渲染错误，全局不重复提示）；
2. 其他 401 且未标记 `__skipAuthRetry` / `__authRetried`、且消息不含 `missing project context` → 刷新一次后带 `__authRetried` 重试原请求；刷新失败 → `forceReLogin()`：toast「会话已过期」、清空项目选择、跳 `/console/login?expired=1`（`authRedirecting` 断路器防重复跳转）；
3. 其余 401（如 sign-out、已重试过）→ 同样 `forceReLogin()`；`missing project context` 的 401 豁免，静默拒绝；
4. 其它 ≥400 且非 `__skipToast` → 从 `error.response.data.error.message` 取后端消息 `toast.error`。

自定义标记（`ApiRequestConfig`）：`__skipAuthRetry`（如 sign-out）、`__authRetried`（防重入）、`__skipToast`（调用方自渲染错误：登录页、改密、初始化等）。

裸 axios 仅两处合法：refresh 调用与 `api/wellknown.ts`（目录挂在根路径，不在 `/v1` 下）。`expired=1` 与改密后的 `reason=password_changed` 都是 Login 页的「禁止探测成功自动跳回 console」标记，必须显式重新登录。

## 4. 认证与偏好

### 4.1 服务端 cookie

| Cookie | Path | 属性 | 说明 |
|--------|------|------|------|
| `TORCHWOOD_session_console` | `/` | HttpOnly + SameSite=Lax | access token，Max-Age = access TTL（配置上限 1h） |
| `TORCHWOOD_console_refresh` | `/v1/console/auth` | HttpOnly + SameSite=Lax | refresh token（默认 7d，`security.jwt.refresh_ttl` 可配），仅发往 console auth 端点 |

- 签发与清除：`internal/api/consolegrpc/cookies.go` 的 `setSessionCookies` / `clearSessionCookies`（sign-in 与 refresh 成功后下发；sign-out 以 Max-Age=0 过期，Path 必须与签发一致）；`Set-Cookie` 经 gateway 的 outgoing header matcher 透传。
- `Secure` 由 `console.Auth.SecureCookies()` 决定：`server.http.public_url` 以 `https://` 开头即启用（`internal/app/console/auth.go`）。
- CSRF：SameSite=Lax 使跨站 POST 不携带 cookie；本服务变更类端点均为 POST，无需额外 CSRF token。
- gateway（浏览器）流的响应体不回传 token（同域 XSS 读不到凭证）；直连 gRPC 客户端仍从 proto 响应体取 token。

### 4.2 前端会话流

- 挂载时 `refreshSession()`（即 single-flight refresh）探测会话：成功 → `isAuthenticated=true`（顺带续期），失败 → 匿名；`loading=true` 期间 `RequireAuth` 返回 null 避免闪屏。
- `login(email, password)` 调 sign-in（`__skipToast` 让登录页自渲染），成功置 authenticated。首管理员未初始化时登录页切换为初始化表单：`getSetupStatus` 返回 `needs_setup` / `setup_token_required`，后者需填部署方 `TORCHWOOD_SECURITY_SETUP_TOKEN` 配置的引导令牌（sign-up 同时创建首个 project/database）。
- `logout()` 调 sign-out（`__skipAuthRetry`）；失败 toast 提示会话可能仍有效，无论成败清空项目选择、本地状态与 `queryClient`，随后 Layout 跳 `/console/login`。
- 路由守卫（`src/App.tsx`）：`RequireAuth` 判登录；`RequireRole` 判写权限（`canWrite`：owner/admin/member）或平台管理员（`isPlatformAdmin`：owner/admin），失败重定向 `/console`；角色查询中返回 null 避免误跳，查询失败（role undefined）fail-closed 拒绝。
- 项目作用域跨标签同步：请求拦截器每次实时读 localStorage，而 React 状态只在挂载时读一次；`AuthProvider` 订阅 `storage` 事件（key 为 `PROJECT_STORAGE_KEY` 或 null=clear）重读项目选择——标签 A 切项目/登出后，标签 B 的展示与写请求 header 保持一致；本标签自身写入不触发 storage 事件（`useAuth.tsx`）。

### 4.3 管理员账户设置（资料 / 偏好）

账户设置区 `/console/account/*`（`AccountLayout` 页签布局，侧栏底部邮箱进入，全角色自助面）：

- **Profile**：账户资料（邮箱 / 角色 / ID / 创建 / 更新时间，只读；邮箱与角色由 owner 经 `UpdateAdmin` 管理）+ **修改密码**（须提供当前密码校验；成功后服务端撤销全部凭证，前端清项目选择并跳 `/console/login?reason=password_changed` 重新登录）。
- **Preferences**：偏好行列表（左侧标题/说明，右侧当前值为修改入口）。当前含时区一项：`admins.metadata` JSONB 存 `timezone`（IANA 名），经 `PATCH /console/admins/me`（`UpdateCurrentAdmin`）自助修改；弹层为可搜索 IANA 时区列表，选中只改草稿、显式「保存」才提交；「跟随浏览器」= 提交空串清除该键。

时区基础设施（`lib/datetime.ts`；不引入日期库，纯 `Intl`）：

- `useUserTimezone()` 返回生效时区：`admins/me` 偏好 → 浏览器时区回退（非法偏好经 `Intl` 实测静默回退，绝不抛 RangeError）。与 `useAdminRole` / 账户页 / Layout 共享 `["console-admin-me"]` 查询缓存——偏好保存后 `setQueryData` 该 key，全部消费组件随重渲染拿到新时区。
- **全站时间显示统一走 `formatDateTime` / `formatDate` / `formatTimeOnly`**（固定 `yyyy-MM-dd HH:mm:ss` 形状，与浏览器 locale / 12 小时制设置无关；空值 / 无法解析返回 `—`）；`toDateTimeLocalValue` / `fromDateTimeLocalValue` 负责 `<input type="datetime-local">` 与绝对时刻的按用户时区互转（两段式求偏移收敛 DST 边界）。
- 例外：Analytics 桶标签是刻意的 UTC 口径，不走用户时区（`routes/analytics/shared.ts`）。
- 顶栏 `TimeBadge` 每秒显示当前时刻 + 生效时区 + UTC 偏移。

## 5. 开发与构建

```bash
mise run console:install   # pnpm install
mise run console:dev       # vite dev server
mise run console:build     # pnpm run build = tsc -b && vite build → dist/
mise run build             # depends console:build → go build 五个二进制到 ./bin/
mise run lint:console      # eslint
```

（测试在 `console/` 目录内 `pnpm test`。）

- `vite.config.ts`：`base: '/console/'`；`@` 别名 → `./src`（`tsconfig.app.json` paths + vite resolve 双处声明）；`preserveDistGitkeep` 插件在构建后补回 `dist/.gitkeep` 占位；vitest 配置（jsdom 环境 + `src/test.setup.ts`）同文件。
- dev 代理：`server.proxy['/v1'] → http://localhost:9080`（与 `server.http.addr` 对齐），保证 dev 下 `/v1` 同源、HttpOnly cookie 正常工作。
- `console/embed.go` 的 `//go:embed all:dist` 由 runtime 的 `NewConsoleHandler` 挂载（`cmd/server/internal/runtime/console.go`）：
  - dist 未构建（embed 里只有 `.gitkeep`）时返回提示页，引导先跑 `mise run console:build`；
  - 缓存策略：`index.html` no-cache，`assets/*` `public, max-age=31536000, immutable`；
  - 资源 404 不回退：带扩展名或 `assets/*` 的缺失路径直接 404，仅无扩展名的前端路由回退 `index.html`（SPA fallback）；
  - 安全头：`X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: strict-origin-when-cross-origin`、CSP（`script-src 'self'`；样式需 `unsafe-inline`）。

> **必做**：修改 Console 后先 `mise run console:build` 再 `mise run build`，否则 `go:embed` 打包旧 `dist/`（`mise run build` 已依赖 console:build，直接跑它即可满足顺序）。

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

列表页复用 `ResourceListPage`（搜索 / 分页 / 多选 / 空态 / 骨架内置），仅提供列定义与可选行为：

```tsx
const columns: ColumnDef<Admin>[] = [
  { key: "email", header: "邮箱", cell: (a) => a.email },
  { key: "role",  header: "角色", cell: (a) => <AdminRoleBadge role={a.role} /> },
];
export function AdminsListPage() {
  const { data: admins = [], isLoading } = useQuery({ queryKey: ["console-admins"], queryFn: listAdmins });
  return <ResourceListPage title="系统管理员" isLoading={isLoading} items={admins} columns={columns}
    getSearchText={(a) => `${a.email} ${a.role}`} detailPath={(a) => `/console/admins/${a.id}`} />;
}
```

常用 props：`detailPath` / `editPath`（行内跳转）、`rowActions`、`toolbarActions`、`selectionActions` + `isRowSelectable`（批量操作）、`filters`、`emptyAction`。搜索与分页参数由 `useListParams` 落在 URL（`?q=&page=&pageSize=`，改查询自动回第 1 页）。详情/表单骨架用 `resource/shared` 的 `DetailPageWrapper` / `FormPageWrapper` + `DetailGrid` / `FormField`。

弹窗表单用 `Dialog`（Radix）+ `useMutation`，成功 `toast.success` + `queryClient.invalidateQueries`。时间列用 `formatDateTime`（§4.3）。

### 6.3 注册路由（src/App.tsx）

```tsx
const AdminsListPage = lazy(() =>
  import("@/routes/admins/pages").then((m) => ({ default: m.AdminsListPage }))
);
// ...
<Route path="admins" element={
  <RouteErrorBoundary><AdminsListPage /></RouteErrorBoundary>
} />
```

所有页面 `React.lazy` 分包；业务页挂在 `<RequireAuth><Layout/></RequireAuth>` 下的 `/console` 布局路由，每条路由包 `RouteErrorBoundary`（按 pathname 重置，同时兜住 chunk 加载失败）；写路由外层再包 `<RequireRole>`（`mode="write"` 默认 / `mode="platformAdmin"`）。

### 6.4 接入菜单（src/lib/nav.ts）

在 `navSections` 按分组追加条目（`{ to, label, icon }`）。页头图标（`PageHeader` 未显式传 icon 时）与面包屑由 `lib/nav.ts` / `lib/routeTitles.ts` 按路径前缀自动派生；新路径段在 `routeTitles.ts` 的 `routeNames` 登记文案。

### 6.5 验证

```bash
pnpm test && mise run console:build && mise run build
# 或 mise run console:dev 手工走查：登录 → 切换项目 → 列表/创建/删除 → 刷新鉴权
```

## 7. 常见坑

1. **忘了重构建**：`go:embed` 只打包构建时刻的 `dist/`；
2. **dev cookie 失效**：vite 代理必须与后端 `server.http.addr` 同源（默认 9080）；
3. **空列表兜底**：`res.data.xxx ?? []`；
4. **错误消息**：从 `error.response.data.error.message` 读取；
5. **绕过 `api` 实例**：仅 refresh 与 wellknown 用裸 axios，其余一律走 `api`（带项目头与 401 刷新）；
6. **权限双 gating**：按钮用 `useAdminRole`，写路由用 `RequireRole`；
7. **时间显示**：用 `lib/datetime` 的用户时区格式化，勿本地化（Analytics 桶标签例外，刻意 UTC）；
8. **header 作用域 RPC**：URL 不含 project_id 的接口（如 OAuth providers，项目取自 Principal）所在页面必须经 `useProjectScopeSync` 把全局选择同步到 URL `:id`，并在 `synced` 前不渲染面板——否则直接访问 URL 时会以旧项目身份查询/保存（参照 `routes/projects/settings.tsx` OAuth 页签）；
9. **int64 是字符串**：gateway protojson 输出 `"3"`，数据层先归一为 number（参照 `api/runtimeVars.ts`）。

## 8. 项目治理面板

项目级配置统一收敛在设置区 `/console/projects/:id/settings`（项目详情页为只读概览 + 「项目设置」入口；旧全局 `/console/settings` 重定向到当前选中项目的设置页）。四个页签，写操作仅平台管理员（owner/admin）可执行：

| 页签 | 能力 | 后端 |
|------|------|------|
| **基本信息** | 只读 `DetailGrid`（ID/状态/时间）+ 名称/描述编辑 | `UpdateProject` |
| **注册与登录** | ① 注册策略 `open`（默认）/ `invite_only` / `closed` 三档切换，即改即存；② 邀请码：生成（次数 1..10000 或不限、可选过期时间；明文仅创建时完整显示一次）、列表回显（明文 + 已用次数 + 有效/用尽/过期/吊销状态）、复制、吊销；③ Messaging 说明卡（Email/SMS OTP 目前为平台级配置，卡内列环境变量） | `UpdateProject`（registration_policy）；`CreateInviteCode` / `ListInviteCodes` / `DeleteInviteCode` |
| **OAuth** | ① OAuth Providers：预置 provider 下拉 + enabled / client_id / client_secret（留空保留原值）/ scopes 编辑，回调地址回显，已配置列表编辑/删除；② Redirect Allowlist 整表编辑（≤100 条，http/https 绝对地址校验，空列表 = 回落默认白名单） | `UpsertOAuthProvider` / `DeleteOAuthProvider`（header 作用域，须过 §7.8 的 synced 门）；`PUT /v1/server/projects/{id}/oauth-redirect-allowlist` |
| **危险区** | 删除项目（不可撤销，连带项目全部数据） | `DeleteProject` |

API Keys 页详情另提供**编辑**（name / scopes / enabled / expire_at，proto3 optional 只提交变更字段）与**轮换**引导（三步：新建同 scope 的 `-rotated` key → 应用切换 → 旧 key 设过期）。scope 多选数据源来自 `/.well-known/torchwood` 的 `api_key_scopes` 目录（`api/wellknown.ts`）；资源级 scope 语法（如 `databases:blog.read`）见 `05-authentication.md` §6。

## 9. 页面全景

侧边栏分组（`lib/nav.ts`）：Dashboard 置顶；Develop（API Keys / Databases / Storage / Functions / Runtime Vars / Analytics）、Auth（Users / Groups）、Economy（Orders / Assets / Subscriptions / Leaderboards）、System（Projects / Admins / Audit Logs）。

桌面侧栏可收起为图标栏（`PanelLeftClose` / `PanelLeftOpen` 按钮）：收起态仅保留图标与项目/头像首字母，悬浮以 `title` 提示；状态持久化在 `localStorage.TORCHWOOD_console_sidebar_collapsed`（"1"/"0"），刷新后保持；移动端抽屉不受影响。顶栏为面包屑导航（`routeTitles.breadcrumbsFor`，长 ID 截断为 20 字符）+ `TimeBadge` 时钟。未匹配路由重定向 `/console`。

| 路由 | 内容 | 守卫 |
|------|------|------|
| `/console` | Dashboard：项目 / API Key / Bucket / 库数量卡 + 当前项目快捷入口 | 登录 |
| `/console/account`（index → profile） | `profile` = 资料只读 + 修改密码（成功后全凭证撤销并回登录页）；`preferences` = 偏好列表（当前仅时区：可搜索弹层，选中暂存 + 显式保存，清除 = 跟随浏览器） | 全角色（自助面） |
| `/console/projects`（+ new / :id / :id/settings） | 项目列表 / 创建 / 只读概览；设置区四页签见 §8 | new 路由 platformAdmin；设置区写操作按 `editable` 收口 |
| `/console/api-keys`（+ new / :id） | Key 列表 / 创建（scope 目录多选）/ 详情（编辑 + 轮换引导） | new 路由 platformAdmin；详情写按钮 platformAdmin |
| `/console/users`（+ new / :id / :id/edit） | 用户 CRUD；详情头部直达入口：行为轨迹（全角色）、用户资产（`assets/users?owner=`）、用户订阅（`subscriptions?q=`）、模拟登录、重置密码（后四者 platformAdmin） | new / edit `RequireRole`（member 可写） |
| `/console/groups`（+ new / :id） | 用户组 CRUD | new / edit `RequireRole` |
| `/console/storage`（+ new / :bucketId / :bucketId/files/:fileId） | Bucket / 对象列表、上传（小文件 multipart 直传；>16MiB 走分片上传会话，上限 156.25GB）、文件详情 | new `RequireRole` |
| `/console/functions`（+ :functionId） | 函数列表 / 详情（部署 zip/git/image、触发器、执行记录） | 路由不设守卫；写按钮按 `canWrite` / platformAdmin 收口 |
| `/console/runtime-vars`（+ :varSetId） | 变量集合（可见性 / 描述 / 限额足迹）/ 类型化变量 CRUD / 版本链（50 版窗口）+ 回滚 | 读全角色；写按钮 platformAdmin 收口 |
| `/console/databases`（+ new / :dbId / :dbId/collections/…） | 库 / collection / 文档 CRUD；collection 内嵌 documents 与 listen 页签（Realtime 试听，cookie 握手 WebSocket，断线先续期再重订） | 库/collection 新建 platformAdmin；文档写 `RequireRole`；listen platformAdmin |
| `/console/analytics`（+ events / events/:name / retention / users/:userId） | 事件分析：概览 / 事件字典 / 事件详情 / 留存网格 / 单用户行为流（recharts；UTC 桶口径） | 全角色 |
| `/console/orders`（+ :id） | 订单列表 / 详情 | platformAdmin |
| `/console/assets`（+ defs/new / defs/:id / users） | 资产定义 CRUD + 用户资产查询；定义详情页下方列出该定义的用户持有（UserID 过滤 + keyset 分页） | platformAdmin |
| `/console/subscriptions`（plans / plans/new / plans/:id / :id） | 套餐 CRUD + 订阅列表 / 详情 | platformAdmin |
| `/console/leaderboards` + `/:boardId` | 榜列表 + 建榜 / 编辑（周期口径 / 时区 / subject 限频 / 保留期 / 奖励规则编辑器）/ 删除；详情 = 期下拉 + top 表（rank/position/subject/value/updated_at，行删条目）+ 按 subject 查条目 | 页面无前端角色门，权限由服务端策略收口 |
| `/console/audit-logs` | 平台审计日志查询（结构化过滤 + metadata 视图） | platformAdmin |
| `/console/settings` | 兼容重定向 → 当前选中项目的设置页 | — |

## 相关文档

- `AGENTS.md` — 目录与组件约定
- `docs/developer/09-api-guide.md` — RPC / protovalidate / OpenAPI 约定
- `docs/developer/05-authentication.md` §6 — API Key 与 scope
- `docs/developer/06-databases.md` — Databases 页面背后的数据面
- `docs/developer/08-functions.md` — Functions 领域
- `docs/developer/18-analytics.md` — Analytics 口径
- `docs/developer/20-leaderboards.md` — Leaderboards 领域
- `docs/design/runtime-vars.md`、`docs/design/analytics.md` — RuntimeVars / Analytics 设计文档
