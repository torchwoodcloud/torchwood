# Torchwood 开发路线图

> 本文档基于已完成 P0 底座，规划 Torchwood 的短期、中期、长期开发方向。
> 最新更新：2026-08-24（v2 PR1–PR5 与 **P2.5 / v3 经济系统均已实施**：payments/subscriptions/assets/billing 的迁移、proto 与 Console 页面已落地；轻量 Realtime + 事件脊柱已交付。设计文档存档：`docs/design/v3-payments-economy.md`、`docs/design/v3-execution-plan.md`）。

---

## 0. AI / Agent-Native 战略（贯穿各阶段）

Torchwood 将 **AI/Agent-Native** 作为与 BaaS 核心能力并列的产品定位：后端不仅服务人类用户，也原生支持 LLM Agent、自动化脚本与 MCP Tool Server 以可预测、可授权的方式调用。

### 已具备（P0 / P1 及 P2 已交付部分）

| 能力 | 说明 | 关键组件 |
|------|------|----------|
| Protobuf + OpenAPI | API 单一事实来源，自动生成 Swagger | `proto/`、`buf.gen.yaml`、`genproto/**/*.swagger.json` |
| Server API + 细粒度 Scope | Agent/自动化通过 API Key 调用管理面，按 scope 限权 | `proto/server/v1/*`、`api_keys.scopes`、`internal/api/interceptor` |
| 结构化鉴权注解 | 每个 gRPC 方法声明 `method_auth`，便于生成工具 schema | `proto/shared/v1/authz.proto` |
| TypeScript SDK | Client + Server API 封装，便于 Agent 工作流集成 | `sdk/typescript/`、`sdk/demo/` |
| 动态文档层 | Agent 可运行时建库/集合/文档，无需手工迁移 | `internal/infra/documentdb/`、`pkg/query/` |
| 文档级权限 | API Key 以 `keys` 角色参与 `_perms`，不默认 bypass | `internal/infra/documentdb/postgres_permissions.go` |
| 轻量 Realtime | 内置 WebSocket 订阅用户 collection 文档变更；高压走 MessageLoop | `internal/api/realtime/` |
| 事件脊柱 + Outbox | 写路径与事务同 `COMMIT` 落 outbox，经 worker 扇出 Realtime / 队列 | `internal/infra/events/`、`internal/infra/documentdb/`、`cmd/worker/` |

### 规划中

P2 先夯 BaaS 门面（Realtime + 事件 + 事务），Agent 表面后置。P1 已具备的 API Key / OpenAPI / SDK 对内测自动化仍然够用。

| 任务 | 说明 | 目标阶段 |
|------|------|----------|
| Webhooks 出站（用户面 CRUD） | 业务事件推送到 n8n / Temporal 等 | P3（P2 只做内部消费，不先做 Webhook 产品） |
| MCP Server | 暴露 Server API 为 MCP Tools | P3 |
| OpenAPI 聚合与 Tool Schema | 合并各服务 Swagger，导出 Agent 可用的 operation 清单 | P3 |
| Agent 专用 API Key 模板 | Console 一键创建「只读 Agent」「文档读写 Agent」等预设 scope | P3 |
| Functions Tool 运行时 | 函数声明 JSON Schema 入参/出参，Agent 当 Tool 调 | P3 |
| 多语言 SDK 生成 | 从 proto 生成 Python 等 SDK（Go/TS 已有） | P3 |

**验收标准（Agent-Native MVP）**：

- 仅凭 API Key + OpenAPI/Swagger，外部 Agent 框架可完成：列出用户、CRUD 文档、上传文件。
- Console 可创建带 scope 限制的 API Key，且越权调用返回明确 `PermissionDenied`。
- TypeScript SDK 演示站点可端到端验证上述流程。

---

## 1. 版本规划总览

| 阶段 | 目标 | 时间范围 | 状态 |
|------|------|----------|------|
| **P0 底座** | 可运行的工程骨架：动态文档层、Admin Console、基础认证、Storage/Functions 端口 | 已完成 | 完成 |
| **P1 MVP** | Client/Server 核心业务闭环：Account、Users、Groups、Databases Documents、Storage 交付、Functions 真实执行、Health | 短期：1-2 个月 | **完成** |
| **P2 / v2** | 轻量 Realtime、事件脊柱（outbox）；staged 事务已按 D-6 删除；按内测需要补生产底座 | 中期：3-6 个月 | **实施完成（PR1–PR5 已合入），owner 审查中** |
| **P2.5 / v3 经济系统** | 支付（Stripe/微信/支付宝/iOS IAP）、订阅、统一资产系统（代币/物品/权益）、平台用量计费 | 中期 | **已实施**（迁移 000013_payments / 000015_assets / 000016_usage_billing / 000017_subscriptions；`proto/server/v1/{payments,subscriptions,assets,billing}.proto`；Console `console/src/routes/payments/` 等） |
| **P3 生态** | Agent 表面（MCP / Tool Schema / Key 模板）、完整 Messaging、关系/向量、Sites / Proxy / VCS / GraphQL、多区域 | 长期：6-12 个月 | 规划中 |

---

## 2. 短期（Short-term，未来 1-2 个月）

**目标**：让 Torchwood 达到可用的 MVP 状态，支持一个典型应用从注册、项目管理、数据库、文件存储到函数执行的完整闭环。

### 2.1 Client Account / Auth（最高优先级）

Client API 是终端用户直接使用的能力。Sprint 1 已补齐会话与 prefs；其余仍待实现。

| 任务 | 说明 | 关键端点 / 文件 | 状态 |
|------|------|-----------------|------|
| Refresh token | 用 refresh token 换取新的 access token | `POST /v1/account/refresh` | ✅ 完成 |
| 会话列表与删除 | 列出当前用户所有会话，可单独或全部删除 | `GET/DELETE /v1/account/sessions` | ✅ 完成 |
| 更新账号资料 | 修改 name | `PATCH /v1/account` | ✅ 完成（email/password 待扩展） |
| 账号偏好 | 读写用户级 `prefs` JSON | `GET/PATCH /v1/account/prefs` | ✅ 完成 |
| 匿名登录 | 创建无密码匿名用户 | `POST /v1/account/sessions/anonymous` | ✅ 完成 |
| Email OTP | 发送邮箱验证码 + 验证登录 | `POST /v1/account/sessions/email-otp` | ✅ 完成 |
| Phone OTP | 发送短信验证码 + 验证登录 | `POST /v1/account/sessions/phone-otp` | ✅ 完成 |
| Magic URL | 创建确认链接 + 确认登录 | `POST/PUT /v1/account/sessions/magic-url` | ✅ 完成 |
| 邮箱验证 | 发送验证邮件 + 确认 | `POST/PUT /v1/account/verification` | ✅ 完成 |
| 密码找回 | 创建找回链接 + 重置密码 | `POST/PUT /v1/account/recovery` | ✅ 完成 |
| JWT 签发 | 用当前会话换取一次性 JWT | `POST /v1/account/jwt` | ✅ 完成 |
| OAuth2（占位） | Google / GitHub 授权与回调 | `/v1/account/sessions/oauth2/*` | ✅ 完成 |
| MFA（占位） | factors 列表、TOTP 创建/验证/删除 + 登录二次验证 | `/v1/account/mfa/*` | ✅ 完成 |
| 账号日志 | 列出最近登录/操作记录 | `GET /v1/account/logs` | ✅ 完成 |

**验收标准**：

- 注册/登录后可获取 access/refresh token；refresh 成功；登出后 access token 失效且会话删除。
- 更新 email 后需重新验证；密码修改需旧密码验证。
- 所有新增端点均有 proto authz 注解、gRPC handler、use-case 单元测试。

---

### 2.2 Server Users 管理

Server API 当前支持列表/获取/更新/删除，缺少创建用户、会话/令牌管理、labels/prefs 完整映射。

| 任务 | 说明 | 关键端点 | 状态 |
|------|------|----------|------|
| 创建用户 | 服务端创建用户并指定密码哈希 | `POST /v1/server/users` | ✅ 完成 |
| 完整更新字段 | labels、status、emailVerified、prefs 支持 `google.protobuf.Struct` | `PATCH /v1/server/users/{id}` | ✅ 完成 |
| 用户会话管理 | 列出/删除指定用户的 sessions | `GET/DELETE /v1/server/users/{id}/sessions` | ✅ 完成 |
| 用户令牌管理 | 列出/删除指定用户的 tokens（与 sessions 合并：token 生命周期绑定会话） | `GET/DELETE /v1/server/users/{id}/sessions` | ✅ 完成 |
| 密码重置 | 服务端直接重置密码（撤销该用户全部会话） | `PATCH /v1/server/users/{id}/password` | ✅ 完成 |
| 模拟登录 | 生成目标用户的 JWT 用于调试 | `POST /v1/server/users/{id}/tokens` | ✅ 完成 |

**验收标准**：

- 服务端可创建用户并立即用其邮箱+密码登录成功。
- labels/prefs 的 CRUD 在数据库层面正确存储为 JSONB。
- 删除用户级联删除其 sessions/tokens。

---

### 2.3 Groups & Memberships

Sprint 1 已完成成员、邀请、角色与 Client/Console 页面；用户组偏好已交付（2026-08-10，含存量集合 reconcile 自愈）。

| 任务 | 说明 | 关键端点 | 状态 |
|------|------|----------|------|
| 成员 CRUD | 列出、创建、获取、更新、删除成员 | `/v1/server/groups/{id}/memberships` | ✅ 完成 |
| 邀请流程 | 创建邀请 → 被邀请人接受/拒绝 | `POST` + `PATCH .../status` | ✅ 完成 |
| 角色体系 | owner / admin / member → JWT `group:{id}`、`member:{id}` | `PATCH .../memberships/{id}` | ✅ 完成 |
| 用户组偏好 | `GET/PUT /v1/server/groups/{id}/prefs` | 扩展 `groups` 集合 | ✅ 完成 |
| Client Groups API | 当前用户创建/加入/退出用户组 | `/v1/groups/*` | ✅ 完成 |
| Console Groups | 用户组列表、详情、邀请与成员管理 | `/console/groups` | ✅ 完成 |

**验收标准**：

- 邀请被接受后，被邀请人拥有 `group:{groupID}` read 权限。
- owner 可删除用户组；member 只能退出。
- 删除用户组级联删除 memberships。

---

### 2.4 Databases Documents（核心）

Sprint 1 已完成 Server/Client Document CRUD；批量操作与 attribute/index 删除仍待实现。

| 任务 | 说明 | 关键端点 | 状态 |
|------|------|----------|------|
| Document CRUD | 创建、获取、更新、删除文档 | `/v1/server/databases/{db}/collections/{coll}/documents` | ✅ 完成 |
| Document 列表/计数 | 带 Appwrite DSL 查询、权限过滤 | `GET` / `count` | ✅ 完成 |
| Client Database API | 终端用户在授权下读写文档 | `/v1/databases/{db}/collections/{coll}/documents/*` | ✅ 完成 |
| Console 文档编辑器 | collection 下文档列表、新增/编辑/删除 | `/console/databases/.../documents` | ✅ 完成 |
| 批量操作 | 批量更新、删除、upsert | `.../documents:bulkUpdate` / `.../documents:bulkDelete`（REST 自定义动词，R10-P1-3/B3 后旧 `.../documents/bulk`、`.../documents/bulk/delete` 已废弃） | ✅ 完成（Console 文档列表批量更新/删除对话框） |
| 字段自增/自减 | 对数值字段做原子增减 | `PATCH .../documents/{id}` | ✅ 完成（Document 详情页 Δ 增量输入） |
| Attribute 删除 | 删除属性并同步 `ALTER TABLE DROP COLUMN` | `DELETE .../attributes/{key}` | ✅ 完成（Schema tab 行内删除按钮） |
| Index 删除 | 删除索引 | `DELETE .../indexes/{id}` | ✅ 完成（Schema tab 行内删除按钮） |
| Collection 更新 | 修改 name / permissions | `PATCH .../collections/{coll}` | ✅ 完成（集合设置卡片：名称编辑 + 停用开关） |

**验收标准**：

- 可通过 Console 在任意 collection 中增删改查文档。
- `equal`、`greaterThan`、`contains`、`orderDesc`、`limit` 等查询组合返回正确结果。
- 普通用户只能读写自己有权限的文档；admin/key 可绕过。
- 删除 attribute 时同步清理 `document_attributes` 元数据与表结构。

> **Backlog（R10-P1-3，已迁移，B3 + 本批 Client API）**：REST 保留字自定义动词迁移已完成（breaking change）。
> 旧字面量路径 `documents/count`、`documents/bulk`、`documents/bulk/delete`、
> `functions/runtimes`、`functions/specifications` 已废弃，新路径为自定义动词
> `documents:count` / `documents:bulkUpdate` / `documents:bulkDelete` /
> `functions:runtimes` / `functions:specifications`；本批 Client API 同步迁移
> `/v1/databases/{database_id}/collections/{collection_id}/documents/count` → `documents:count`
> （clientDocumentIDReserved 已移除），Client API 的 `count` 同样为合法 document_id；
> `count`/`bulk`/`runtimes`/`specifications` 现为合法 id（Server/Client 保留字校验均已移除），
> 与 `{id}` 通配路由不再冲突。
> **升级指引**：旧 REST 路径请求一律 404，客户端（TS SDK / Console）需随版本升级；
> 自定义动词迁移完成后路由冲突已根除，历史保留字 id（`count`/`bulk`/`runtimes`/
> `specifications`，仅保留字校验上线前创建的数据可能存在）经 REST 的 Get/Update/Delete
> 自动恢复可访问，**无需任何数据清理/重命名**。
>
> **升级自查检测 SQL**（可选，仅当运维怀疑存在此类历史 id 时执行）：
>
> ```sql
> -- bun 静态表：历史保留字 id 的 function（若有则说明该行曾被字面量路由遮蔽，现已自动恢复）
> SELECT id FROM functions WHERE id IN ('runtimes','specifications');
> ```
>
> ```sql
> -- 动态文档层：按 collection 表检查（替换为实际项目的 project.id / database.id / collection 名）
> -- schema = tw_{project.id}_{database.id}
> SELECT _id FROM tw_shop_app.posts WHERE _id IN ('count','bulk');
> ```

---

### 2.5 Storage 文件交付

当前支持上传、下载、查看、预览、公开访问、File Token、元数据更新、Usage 统计、**分片上传（upload session）**。

| 任务 | 说明 | 关键端点 | 状态 |
|------|------|----------|------|
| 文件预览/缩略图 | 图片裁剪/缩放（使用 `disintegration/imaging`） | `GET /v1/storage/buckets/{id}/files/{id}/preview` | ✅ 完成（width/height 缩放、50MiB 上限、webp 解码 JPEG 输出） |
| 公开 bucket | bucket 级 `public` 标志，允许匿名读取 | `CreateBucket`/`UpdateBucket` 的 `public` 字段 | ✅ 完成（匿名读需 `?project=` 参数，文件级 read:any 兜底） |
| File Token | 生成短期文件访问令牌（HMAC 签名，默认 1h、上限 7d） | `POST /v1/storage/buckets/{id}/files/{id}/tokens` | ✅ 完成（下载 URL 携带 `?token=`，过期/篡改 401） |
| 文件元数据更新 | 修改 name、mime_type、metadata | `PATCH /v1/server/storage/buckets/{id}/files/{id}` | ✅ 完成（含 `UpdateBucket`：改名/公开开关） |
| 分片上传 | upload session 生命周期（create/get/chunk/complete/abort），断点续传 + 服务端合并（ComposeObject） | `POST /v1/storage/buckets/{id}/uploads`、`GET .../uploads/{uploadId}`、`POST .../uploads/{uploadId}/chunks/{partNumber}`、`POST .../uploads/{uploadId}/complete`、`DELETE .../uploads/{uploadId}` | ✅ 完成（分片 ≤16MiB、part_count ≤10000（≤156.25GB）、24h 会话 TTL、complete 互斥锁、Console 自动分片+进度+localStorage 续传；端点与早期 `files/{id}/chunks` 占位不同——会话语义见 `docs/implementation-storage-chunked-upload.md`） |
| Usage 统计 | bucket/files 数量与容量统计（`SumDocumentField` 聚合） | `GET /v1/server/storage/usage` | ✅ 完成 |

**验收标准**：

- 上传图片后可通过 `preview?width=200&height=200` 获取缩略图。
- 公开 bucket 中的文件可在无 Authorization 时通过 `view` 访问。
- File token 在过期前允许下载，过期后返回 401。

---

### 2.6 Functions 真实执行器

已交付（2026-08-09）：真实 Docker build/run、同步/异步执行、`cmd/worker` 消费队列。
「构建队列」任务以「CreateDeployment 同步构建」落地（对 roadmap 的 MVP 偏离，见
`docs/implementation-functions-executor.md` §2）。

| 任务 | 说明 | 关键端点 / 组件 | 状态 |
|------|------|-----------------|------|
| Runtime 列表 | 返回支持的运行时（node-18、python-3.11 等） | `GET /v1/server/functions/runtimes` | ✅ 完成 |
| Specification 列表 | 返回 CPU/内存规格 | `GET /v1/server/functions/specifications` | ✅ 完成 |
| Function CRUD | 创建/列表/获取/更新/删除函数 | `/v1/server/functions` | ✅ 完成 |
| Deployment CRUD | 上传代码包、列表、获取、删除 | `/v1/server/functions/{id}/deployments` | ✅ 完成 |
| Variables CRUD | 函数环境变量 | `/v1/server/functions/{id}/variables` | ✅ 完成 |
| Execution CRUD | 同步/异步执行、获取结果 | `POST/GET /v1/server/functions/{id}/executions` | ✅ 完成 |
| Docker build | 解压代码包，按运行时 Dockerfile 构建镜像（防 zip 炸弹/slip） | `internal/infra/functions/docker.go` | ✅ 完成 |
| Docker run | 运行容器，收集 stdout/stderr，超时控制与安全基线 | `internal/infra/functions/docker.go` | ✅ 完成 |
| 异步执行 Worker | `cmd/worker` 消费执行队列（BRPOP、N=4 并发、孤儿对账） | `cmd/worker/` | ✅ 完成 |
| 构建队列 | Redis List 队列 + CreateDeployment 同步构建（MVP 偏离） | `internal/domain/shared/ports.go`、`internal/infra/queue/` | ✅ 完成 |

**验收标准**：

- 上传一个 Node.js 函数后，可同步调用并返回 `console.log` 输出。
- 函数执行超时后返回 500 并清理容器。
- 异步执行可在 Console 中查看 execution 状态与日志。

---

### 2.7 Health & 可观测性

已交付（2026-08-09）：依赖探测健康检查、版本端点（ldflags 注入）、请求日志与统一 logger、慢查询 hook。

| 任务 | 说明 | 关键端点 | 状态 |
|------|------|----------|------|
| 健康检查 | DB、Redis、Storage 健康状态（并行探测 + 依赖明细 + readiness 503） | `GET /v1/health`、`/healthz/readiness` | ✅ 完成 |
| 版本端点 | 返回版本与构建信息 | `GET /v1/server/health/version` | ✅ 完成 |
| 结构化日志 | `slog` 统一 logger + gateway 请求日志（Debug 级） | 全局中间件 | ✅ 完成 |
| 慢查询日志 | 动态文档层记录慢 SQL（bun QueryHook，空值默认 500ms） | `internal/infra/clients/dbhook.go` | ✅ 完成 |

---

### 2.8 Admin Console UI

| 任务 | 说明 | 页面 | 状态 |
|------|------|------|------|
| 系统管理员管理 | 管理员 CRUD、角色与密码管理、owner 权限保护；`AdminsService`（`GET/POST/PATCH/DELETE /v1/console/admins`） | `console/src/routes/admins/`、`proto/console/v1/admins.proto` | ✅ 完成 |
| Storage 文件上传 | 在 Storage 页面直接上传文件、展示下载链接 | `console/src/routes/storage/` | ✅ 完成 |
| Databases 文档编辑器 | collection 下文档列表、新增/编辑/删除 | `console/src/routes/databases/` | ✅ 完成 |
| Attributes / Indexes 管理 | 在 collection 详情中增删属性与索引（含 Attribute/Index 行内删除） | Databases 子页面 | ✅ 完成 |
| Groups Memberships | 管理用户组邀请与成员 | `console/src/routes/groups/` | ✅ 完成 |
| Functions 管理 | Functions / Deployments / Executions 页面 | 新增 `Functions.tsx` | 待办 |
| 项目设置区 | 项目配置统一收敛至 `/console/projects/:id/settings`（显式项目作用域：基本信息编辑 `PATCH /v1/server/projects/{id}`、注册与登录（注册策略/邀请码/SMTP 说明）、OAuth（Providers/重定向白名单）、危险区（删除项目））；旧全局 `/console/settings` 重定向退役 | `console/src/routes/projects/settings.tsx` | ✅ 完成 |
| 侧边栏菜单分组 | Dashboard 置顶；Develop（API Keys/Databases/Storage）、Auth（Users/Groups）、System（Projects/Admins）分组；项目设置入口在项目详情页与 Dashboard（侧边栏 Settings 捷径已移除） | `console/src/components/Layout.tsx` | ✅ 完成 |

**验收标准**：

- Console 中可直接完成“创建 project → 创建 database → 创建 collection → 添加文档 → 上传文件”的闭环。
- 401 时自动跳转登录；全局错误 toast 提示。

---

### 2.9 工程化与质量

| 任务 | 说明 | 关键文件 | 状态 |
|------|------|----------|------|
| API Key scope 校验 | 拦截器解析 scopes，对端点做细粒度授权 | `internal/api/interceptor/scope.go` | ✅ 完成 |
| 单元测试补齐 | 每个新增 use-case 至少一个单元测试（Functions use-case 已补） | `internal/app/**/*_test.go` | ✅ 完成 |
| 集成测试 | Account、Databases Documents、Storage 端到端测试；本次补 UpdateCollection/DeleteAttribute/DeleteIndex/increment 路径 | `internal/app/**/*_integration_test.go` | ✅ 完成 |
| Seed 数据增强 | 提供示例 collection、文件、函数 | ~~`cmd/seed/main.go`~~ | 已由首个管理员 bootstrap 取代（`docs/implementation-bootstrap-and-cli.md`），`cmd/seed` 移除 |
| GitHub Actions CI | lint（gofmt/vet/eslint）、test（含 Postgres 集成）、build、console-build | `.github/workflows/ci.yml` | ✅ 完成 |
| 代码格式化 | `gofmt`、`eslint`（prettier 未启用） | `Taskfile.yml`（`task lint`）、`console/eslint.config.js` | ✅ 完成 |

**验收标准**：

- CI 每次 PR 触发 `go test ./...` 和 `task build` 并通过。
- API Key 没有 `users.write` scope 时无法调用 `UpdateUser`。

---

## 3. 中期 P2 / v2（Medium-term，未来 3-6 个月）

**目标**：让自用 / 内测应用能在 Torchwood 上长期跑起来——业务会动（轻量 Realtime + 事件脊柱），文档写走单条 CRUD / Bulk + 内部 `uow.Run`。

v2 **不是**「把 Appwrite 剩下的模块搬过来」。Agent 叙事（MCP、Tool Schema、Key 模板、Functions as Tools）后置到 P3；先把 BaaS 门面夯实。

### 3.0 已锁定边界

| 议题 | 决定 |
|------|------|
| 第一用户 | 自用 / 内测应用（不是外部 Agent 框架） |
| 第一门面 | 轻量 Realtime |
| 高压 Realtime | 不自研集群；通道与 payload 归 Torchwood，投递可换 `D:/Codes/qiulin/messageloop` |
| 事务形态 | **已删除 staged API（D-6，内测无兼容）**；对外 Documents CRUD + Bulk，内部 `uow.Run`；不上 2PC / XA / Saga |
| 事务范围 | 同一 RPC 内用户自建 collection；禁止跨 database / 跨 project |
| 系统资源 | 走 Account / Server Users / Storage / Groups 专用 RPC，不经 Documents 写路径 |
| 事件与写路径 | transactional outbox 与事务同一 `COMMIT`；不和 Redis / S3 / Function 做分布式事务 |
| Bulk API | 保持立即执行、非原子；不与事务混为一谈 |
| Messaging | 现有 OTP / 验证 / 找回够用；完整 Provider + Topic + Push **不做** |
| Agent Identity / Run | API Key 够用；不做独立 Agent Principal，不做 Tool Trace 产品 |
| Console | Databases 详情页「试听」集合频道；不堆新模块页 |
| 关系 / 向量 / Geo | 后置 P3 |
| 设计稿 | `docs/design/v2-events-realtime-transactions.md`（批准）；执行计划 `docs/design/v2-execution-plan.md` |

依赖顺序：**事件脊柱（最小 outbox）→ 轻量 Realtime**。文档写与 outbox 同 `COMMIT`（`uow.Run`），否则 Realtime 只能 fire-and-forget。

### 3.1 事件脊柱（最小）

现有 `Queue` 端口只服务 Functions 执行；`EventPublisher` / outbox **尚未落地**。v2 先做平台能力，不做 Webhook 产品。

| 任务 | 说明 | 关键组件 |
|------|------|----------|
| 事件目录 | 仅用户 collection 文档：`databases.documents.create` / `update` / `delete`。Increment 与 Update 同为 `update`。目录可扩展，P2 不实现 storage / functions 生产者 | `internal/domain/events/` |
| 生产路径 | 单条 CRUD、Increment、Bulk（每文档一条） | use-case 写路径 |
| Payload | create/update 带全文档（含 `version`）；delete 只带 id | 事件信封 |
| 推送资格 | create 按写后 `_perms`；update/delete 按**写前**能 read 的 principal | 与 documentdb 权限同一套角色 |
| Outbox | 写库与发事件同 `COMMIT` 落入 PG；at-least-once，信封含 `event_id` | 元数据表 + 现有 `cmd/worker` |
| 发布端口 | use-case 写路径统一 `Publish`，禁止 handler 直推 WS | `internal/domain/shared` 新增 Event / Realtime 端口 |
| 内部消费者 | 第一消费者是内置 Realtime；Webhook HTTP 投递不做用户面 CRUD | worker registry |
| Worker 扩展 | 在现有 Functions worker 上注册 outbox 消费者；重试计数写入 payload（P1 遗留 B2） | `cmd/worker`、`internal/domain/shared/ports.go` |

### 3.2 轻量 Realtime（第一门面）

内置版只服务内测连接数。Presence、历史回放、多节点会话归属不做——那些是 MessageLoop 的职责。

| 任务 | 说明 | 关键组件 |
|------|------|----------|
| WebSocket 接入 | 复用现有 HTTP mux，路径 `/v1/realtime` | `internal/api/realtime/` |
| 连接握手 | 一条连接绑定一个 project。SDK 首帧带 access token；Console same-origin 走 session cookie。无 API Key、无 guest。JWT 过期断开，SDK 自行 refresh 再握手 | 握手协议 |
| 频道 | `databases.{db}.collections.{coll}` 与 `...documents.{id}`。无通配、无系统/停用集合 | Channel manager |
| 订集合频道 | 集合存在即可（非系统、未停用），**不查** collection 级 read | — |
| 订文档频道 | 文档必须已存在且当前可读；失败统一 `NotFound` | — |
| 逐条鉴权 | Client JWT 每条事件过 `_perms`；Console platform admin 绕过 `_perms` | 与 Q3/Q17/Q21 一致 |
| 扇出 | 单实例进程内 hub；需要跨进程时再接 Redis Pub/Sub 或 MessageLoop broker | Hub + `RealtimePublisher` |
| 心跳 / 配额 | Ping/Pong；每用户最多 4 连，每连最多 32 订阅 | 协议层 |
| SDK | TS / Go Client 必须能订 | `sdk/typescript`、`sdk/go` |
| Console | Databases 详情「试听」该集合（管理员旁路） | `console/src/routes/databases/` |
| MessageLoop 适配器 | **非 P2 必达**。通道名与 payload 先按本文稳定下来 | 日后 `internal/infra/realtime/messageloop.go` |

**明确不做**：Presence、频道历史、Survey、集群接管、用户级 `users.{id}` 频道、连接内换证。重连不补历史。

### 3.3 单库事务

**已删除（D-6，内测无兼容）。** 对外只保留 Documents CRUD（含 BulkUpdate/BulkDelete）；每次 RPC 一条内部 `uow.Run`。不保留 staged API、暂存表、信封 `transaction_id`，也不新造跨文档原子 API。

`_version` OCC 仍对用户集合 Update / Delete / Increment 强制；Bulk / Upsert / Create 不带。历史 staged 设计见 `docs/design/v2-events-realtime-transactions.md`（§5 已过期）。

### 3.4 生产底座（按内测需要，不挡第一门面）

上内测前建议至少有限流。其余可并行、可后置，不计入「Realtime 能用」的必达。

| 任务 | 说明 | 关键组件 |
|------|------|----------|
| 速率限制 | 按 IP / user / API Key | `pkg/ratelimit` + Redis |
| 审计日志 | 已落地：拦截器按噪声治理范围写入（管理面写操作 + client 安全动作；读浏览/探针/数据面高频不记）+ 结构化变更记录（client/request/changes）+ `AuditLogsService` 查询（Console/CLI/SDK 消费），见 `docs/developer/09-api-guide.md` §12 | `internal/api/interceptor/audit.go` + `proto/server/v1/audit_logs.proto` |
| API Key 轮换 | secret 重新生成 | `/v1/server/api-keys/{id}/rotate` |
| 邮箱变更 staging | P1 遗留 B1：新邮箱验证前旧邮箱仍可用 | Account use-case |
| Worker 重试持久化 | P1 遗留 B2：attempt 写入 payload | `cmd/worker` |
| 全文检索收口 | `search` 仅允许 fulltext 索引列（已有算子，收紧以免 CPU DoS） | `buildAppwriteQuery` |
| DeleteProject | 已落地：平台 admin；级联清理动态 schema | `DELETE /v1/server/projects/{id}` |

项目设置（平台 origin、密码策略、SMTP 模板、30+ OAuth）**不作为 v2 核心**。OAuth 维持已有 Google / GitHub / 微信；SMTP 维持现有发信。

### 3.5 明确移出 v2

| 模块 | 去向 |
|------|------|
| 用户面 Webhook CRUD、HMAC 投递产品 | P3 |
| MCP / OpenAPI 聚合 / Agent Key 模板 / Functions as Tools | P3（§0） |
| 完整 Messaging（Topic / SMS / Push） | P3 |
| Relationships、Vectors（pgvector）、Geo（PostGIS） | P3 |
| Presence、Realtime 集群 | MessageLoop，需要时再接 |
| Sites / Proxy / VCS / GraphQL / Avatars / Locale / Advisor | P3（§4） |
| 独立 Agent Identity、Agent Run / Tool Trace | 不做，直到有明确用户 |

---

## 4. 长期（Long-term，未来 6-12 个月）

**目标**：补齐 Agent-Native 表面与完整 BaaS 生态（从 P2 明确移出的模块 + 站点托管 / CI/CD / GraphQL / 水平扩展）。

### 4.0 从 P2 移入

| 任务 | 说明 |
|------|------|
| Webhook 产品 | Webhook CRUD、HMAC 签名、重试与死信、投递 Worker |
| MCP + Tool Schema + Agent Key 模板 | 见 §0 |
| Functions as Tools | 函数 JSON Schema I/O |
| Messaging | Providers / Topics / Subscribers / SMS / Push |
| Relationships | 一对一、一对多、多对多 |
| VectorsDB | `vector` attribute + pgvector 相似度查询 |
| 全文检索增强 | `pg_trgm` / 索引策略（P2 只做「仅索引列可 search」收口） |
| Geo | PostGIS `point` / `polygon` |
| 项目设置补齐 | Platforms、密码/会话策略、邮件模板、DeleteProject |
| MessageLoop 投递 | 内置 Realtime 协议不变，broker 换成 MessageLoop |

### 4.1 Sites（静态/SSR 站点托管）

| 任务 | 说明 | 关键端点 |
|------|------|----------|
| Sites CRUD | 创建/列表/获取/更新/删除站点 | `/v1/server/sites` |
| Frameworks 列表 | 支持的框架模板 | `/v1/server/sites/frameworks` |
| Deployments | 上传构建产物、激活 deployment | `/v1/server/sites/{id}/deployments` |
| 静态文件托管 | 从 Storage 或专用 bucket serve | Storage adapter 扩展 |
| SSR 运行时 | 边缘/容器 SSR 执行 | Functions executor 扩展 |

---

### 4.2 Proxy（域名与路由）

| 任务 | 说明 | 关键端点 |
|------|------|----------|
| Rules CRUD | API rule、site rule、function rule、redirect rule | `/v1/server/proxy/rules` |
| 自定义域名 | CNAME 校验、TLS 证书自动申请 | Certificate worker |
| 路由分发 | 根据域名/路径分发到对应服务 | Reverse proxy layer |

---

### 4.3 VCS（Git 集成）

| 任务 | 说明 | 关键端点 |
|------|------|----------|
| GitHub OAuth | 授权、callback、installation | `/v1/server/vcs/github/authorize` |
| Repositories | 列出仓库、分支、文件内容 | `/v1/server/vcs/github/repositories` |
| 自动部署 | GitHub webhook 触发 Functions/Sites 构建 | Webhook handler |

---

### 4.4 GraphQL

| 任务 | 说明 | 关键组件 |
|------|------|----------|
| Schema 生成 | 从 collection/attribute 自动生成 GraphQL schema | `internal/api/graphql/schema.go` |
| Query/Mutation | 复用 REST 用例层 | `internal/api/graphql/resolver.go` |
| 订阅 | 基于 Realtime 的事件订阅 | GraphQL subscriptions |

---

### 4.5 周边服务

| 任务 | 说明 | 关键端点 |
|------|------|----------|
| Avatars | 头像、浏览器图标、favicon、国旗、QR 码、首字母头像 | `/v1/avatars/*` |
| Locale | 国家、货币、语言、电话代码等静态数据 | `/v1/locale/*` |
| Advisor | 项目诊断报告与建议 | `/v1/server/advisor/*` |

---

### 4.6 扩展性与平台化

| 任务 | 说明 | 关键组件 |
|------|------|----------|
| 水平扩展 | gRPC 服务多实例、负载均衡 | Deployment / Helm chart |
| 多区域存储 | S3 跨区域复制、就近读取 | Storage adapter |
| 只读副本 | 查询路由到 PostgreSQL 只读副本 | `internal/infra/clients/database.go` |
| SDK 生成 | 根据 proto 生成 Go/JS/Flutter/Python SDK | `cmd/gensdk` |
| 计费/用量 | 按 API 调用、存储、函数执行时长计费 | Usage aggregator worker |
| 高级可观测性 | OpenTelemetry、分布式追踪、告警 | `telemetry` config |

---

## 5. 里程碑与验收标准

### M1：P1 MVP 可用（短期结束）✅ 全部完成（2026-08-10）

- [x] Client Account 核心会话与 prefs（Refresh / Sessions / UpdateAccount / Prefs）。
- [x] Client Account 完整能力（密码重置、邮箱验证、OAuth、MFA、Magic URL、JWT、账号日志、匿名登录）。
- [x] Server Groups / Memberships 管理可用。
- [x] Server Users 创建与会话/令牌管理。
- [x] Databases Documents CRUD、Client API 权限可用。
- [x] Databases 批量操作、attribute/index 删除。
- [x] Storage preview、公开 bucket、file token 可用。
- [x] Functions 可上传代码、构建、同步/异步执行。
- [x] Admin Console 覆盖 Database 文档编辑、Groups 页面。
- [x] Admin Console 覆盖系统管理员管理（Admins 页面，owner 权限保护）。
- [x] Admin Console 覆盖 Functions 页面。
- [x] Admin Console 覆盖项目设置区（项目基本信息编辑、OAuth Providers、Messaging 只读说明；2026-09 起统一收敛至 `/console/projects/:id/settings`）。
- [x] CI 绿，集成测试覆盖核心流程。

### M2：P2 / v2 内测可用（中期结束）

- [x] 用户 collection 的文档写路径经 outbox 发布事件（与写同一 `COMMIT`）。
- [x] 轻量 Realtime：已鉴权客户端可订阅文档变更并收到提交后的事件。
- [x] staged transaction API：**已删除（D-6，内测无兼容）**。
- [ ] Bulk API 行为不变（立即、非原子）。
- [x] Worker 能消费 outbox（Functions 队列仍可用）；重试计数不因进程重启丢失。
- [x] 内测前上线按 IP / user / API Key 的速率限制。
- [ ] 官方 Client SDK 可按 §3.2 频道与握手建立订阅；Console 集合详情可试听。
- [x] 用户 collection 文档带 `_version`；Update / Delete / Increment 强制 OCC（Bulk / Upsert 除外）。

**不计入 M2**：Webhook 用户面、Messaging 产品、MCP、关系/向量、Presence、负载与混沌测试（内测不卡这个）。

### M3：P3 生态完整（长期结束）

- [ ] Agent 表面：MCP / Tool Schema / scoped Key 模板可用。
- [ ] Webhooks 可创建并成功投递；Messaging 可发送邮件/SMS/Push。
- [ ] Relationships / Vectors 可用。
- [ ] Sites / Proxy / VCS / GraphQL 上线。
- [ ] 多区域部署与水平扩展方案稳定运行。
- [ ] 官方 SDK 发布到包管理器。
- [ ] 完整的运营仪表盘与 SLA 监控。

---

## 6. 风险与依赖

| 风险 | 影响 | 缓解措施 |
|------|------|----------|
| Docker executor 安全隔离复杂 | Functions 执行可能威胁主机 | 使用 gVisor/Firecracker 或限制容器资源与网络 |
| 内置 Realtime 单实例上限 | 内测后连接数打满 | 通道/payload 与投递解耦；高压接 MessageLoop，不自研集群 |
| Outbox 与 Realtime 时序 | 客户端先收到事件再读仍是旧值，或事件丢失 | 写路径与 outbox 同 `COMMIT`；投递 at-least-once，客户端按 id 去重 |
| 文件预览性能 | 大图缩放消耗 CPU/内存 | 限制最大尺寸、异步生成、可选外部 CDN |
| 动态 schema 迁移 | attribute/index 变更可能影响大数据量表 | 使用 `ALTER TABLE` 时加锁评估、提供异步迁移 |

---

## 7. 参考

- `docs/archived/appwrite-go-migration-modules.md`：Appwrite 功能迁移全景（已归档）。
- `docs/archived/p0-foundation-design.md`：P0 底座设计（已归档）。
- `docs/archived/completed-tasks.md`：已完成任务清单（已归档）。
- `docs/design/v2-events-realtime-transactions.md`：v2 批准设计（事件 / Realtime / 事务）。
- `docs/design/v2-execution-plan.md`：五张 PR 的执行计划。
- `docs/prompts/implement-v2.md`：派给第三方实施 agent 的 prompt。
- `docs/tech-decision.md`：技术选型决策。
- `README.md`：快速开始。
- `AGENTS.md`：开发约定。
