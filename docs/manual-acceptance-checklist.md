# Torchwood P0 人工验收清单

> 基于 `docs/archived/completed-tasks.md` 及近期提交整理。
> 对应提交：`11af6f8`（P1 Sprint 1：Account 会话、Document CRUD、Groups Memberships、Console Groups）。
>
> **用法**：逐项执行操作，在 `[ ]` 中打 `x` 标记通过；未通过项在「备注」列记录现象与复现步骤。

---

## 验收信息

| 项目 | 内容 |
|------|------|
| 验收人 | |
| 验收日期 | |
| 分支 / 提交 | `main` @ |
| 环境 | 本地 / 其他： |
| HTTP 基址 | 默认 `http://localhost:8088` |
| Console 地址 | 默认 `http://localhost:8088/console/` |
| Metrics | 默认 `http://localhost:9100` |

---

## 0. 环境准备（必做）

| # | 验收项 | 操作步骤 | 预期结果 | 通过  |
|---|--------|----------|----------|-----|
| 0.1 | 基础设施启动 | `task up` | Postgres / Redis / MinIO 容器健康 | [x] |
| 0.2 | 环境变量 | 复制 `.env.example` → `.env`，设置 `TORCHWOOD_SECURITY_JWT_SECRET` 等非空值 | 服务可读取配置 | [x] |
| 0.3 | 数据库迁移 | `task migrate` | `000001`、`000002` 迁移成功；存在 `audit_logs`、`admin_projects` 表 | [x] |
| 0.4 | 首次部署引导（bootstrap） | 全新数据库启动 server 后打开 `/console/`，按「初始化设置」表单填写 email/password/project id/database id 注册第一个管理员 | 注册成功即进入 Console；指定 project 与 database 已创建；未生成 API Key；`GET /v1/console/auth/setup-status` 返回 `{"needs_setup":false}`；二次 `POST /v1/console/auth/sign-up` 返回 `FailedPrecondition`；登录后在 API Keys 页面可创建密钥 | [x] |
| 0.5 | Console 构建 | `task console-build`（若验收嵌入版 Console） | `console/dist/` 生成且无报错 | [x] |
| 0.6 | 服务启动 | `task dev-server` 或 `task build && ./bin/server` | gRPC / HTTP / Metrics 均监听；无启动 panic | [x] |
| 0.7 | 健康检查 | `GET /v1/health` | `{"status":"ok"}` 或等价 200 响应 | [x] |

**记录 bootstrap 输出（验收过程使用）：**

```
Project ID: _______________________
Database ID: _______________________
Console Admin: <注册时填写的 email>（owner）
API Key：登录后在 Console → API Keys 页面创建
```

---

## 1. 工程化与构建

| # | 验收项 | 操作步骤 | 预期结果 | 通过  |
|---|--------|----------|----------|-----|
| 1.1 | 单元测试 | `go test ./... -short` | 全部 PASS（集成测试可跳过） | [x] |
| 1.2 | 全量测试 | `go test ./...`（可选，需本地 Postgres） | 全部 PASS | [x] |
| 1.3 | 编译 | `task build` 或 `go build ./cmd/server` | 生成可执行文件，无编译错误 | [x] |
| 1.4 | Console 开发构建 | `task console-build` | 前端产物可嵌入 Go binary | [x] |

---

## 2. Client Account（终端用户认证）

**公共请求头说明**：以下 JSON API 经 grpc-gateway，Content-Type 为 `application/json`。

| # | 验收项 | 操作步骤 | 预期结果 | 通过  |
|---|--------|----------|----------|-----|
| 2.1 | 注册 | `POST /v1/account/sign-up`，body 含 `project_id=default`、email、password、name | 201/200；返回 `account` + `tokens`（含 access_token、refresh_token） | [x] |
| 2.2 | 重复注册 | 同一 project + email 再次注册 | 失败，`AlreadyExists` 或等价错误 | [x] |
| 2.3 | 登录 | `POST /v1/account/sign-in` | 返回用户信息与 token 对 | [x] |
| 2.4 | 错误密码 | 错误 password 登录 | `Unauthenticated` | [x] |
| 2.5 | 获取当前用户 | `GET /v1/account/me`，Header `Authorization: Bearer <access_token>` | 返回当前用户信息（email/name 等） | [x] |
| 2.6 | 无 Token 访问 Me | 不带 Authorization 调用 Me | `Unauthenticated` | [x] |
| 2.7 | 登出 | `POST /v1/account/sign-out`，带 access token | 成功；session 文档被删除 | [x] |
| 2.8 | 登出后 Me | 登出后用**同一 access token** 再调 Me | 失败（session 已失效 / token 不可用） | [x] |
| 2.9 | Refresh Token | `POST /v1/account/refresh`，body：`project_id` + `refresh_token` | 返回新的 access_token 与 refresh_token | [x] |
| 2.10 | 无效 Refresh | 使用伪造或过期的 refresh_token | `Unauthenticated` | [x] |
| 2.11 | Access 当 Refresh | 用 access_token 调 refresh 接口 | 失败（token type 不匹配） | [x] |
| 2.12 | 更新资料 | `PATCH /v1/account`，修改 `name` | 返回更新后的 account | [ ] |
| 2.13 | 会话列表 | `GET /v1/account/sessions` | 返回当前用户 session 列表 | [ ] |
| 2.14 | 删除会话 | `DELETE /v1/account/sessions/{id}` | 指定 session 被删除 | [ ] |
| 2.15 | Prefs 读写 | `GET/PATCH /v1/account/prefs` | prefs JSON 正确读写 | [ ] |
| 2.16 | Group JWT roles | 用户接受 group 邀请后重新登录，`/v1/account/me` 或解码 access token | roles 含 `group:{groupId}`、`member:{membershipId}` | [ ] |

**示例（注册）：**

```bash
curl -s -X POST http://localhost:8088/v1/account/sign-up \
  -H "Content-Type: application/json" \
  -d '{"project_id":"default","email":"qa@torchwood.local","password":"Qa@123456","name":"QA User"}'
```

**示例（Refresh）：**

```bash
curl -s -X POST http://localhost:8088/v1/account/refresh \
  -H "Content-Type: application/json" \
  -d '{"project_id":"default","refresh_token":"<REFRESH_TOKEN>"}'
```

---

## 3. Console 管理后台

| # | 验收项 | 操作步骤 | 预期结果 | 通过  |
|---|--------|----------|----------|-----|
| 3.1 | 页面可访问 | 浏览器打开 `/console/` | SPA 加载，显示登录页 | [x] |
| 3.2 | 管理员登录 | `admin@torchwood.local` / `Admin@123` | 进入 Dashboard，无报错 toast | [x] |
| 3.3 | 错误凭据 | 错误密码登录 | 提示错误，停留在登录页 | [x] |
| 3.4 | Projects 页 | 导航至 Projects | 列表展示项目（含 `default`） | [x] |
| 3.5 | API Keys 页 | 导航至 Api Keys | 可列表；创建后**仅一次**显示 secret | [x] |
| 3.6 | Users 页 | 导航至 Users（需先选择/绑定项目） | 展示通过 Client 注册的用户 | [x] |
| 3.7 | Storage 页 | 创建 Bucket、上传文件（若 UI 支持） | Bucket / File 列表有数据 | [x] |
| 3.8 | Databases 页 | 创建 Database / Collection | 元数据 catalog 可查看 | [x] |
| 3.9 | Databases 文档 | 在 Collection 详情中新建/编辑/删除 Document | 文档列表与编辑器正常 | [ ] |
| 3.10 | Groups 页 | 导航至 Groups → 新建用户组 → 邀请成员 | 成员列表显示 pending；无「接受/拒绝」按钮（管理员视角） | [ ] |
| 3.11 | 登出 / 会话 | 刷新页面或重新打开 | 未登录时跳转登录；已登录保持状态 | [x] |

**Console API（可选 curl 验证）：**

```bash
curl -s -X POST http://localhost:8088/v1/console/auth/sign-in \
  -H "Content-Type: application/json" \
  -d '{"email":"admin@torchwood.local","password":"Admin@123"}'
```

---

## 4. Server API（API Key / Admin）

**认证方式（二选一）：**

- `X-Api-Key: <project_api_key_secret>`
- Console JWT：`Authorization: Bearer <admin_token>` + `X-Torchwood-Project: default`

| # | 验收项 | 路径 / 方法 | 预期结果 | 通过  |
|---|--------|-------------|----------|-----|
| 4.1 | 无凭证拒绝 | 任意 `/v1/server/*` 无 Header | `Unauthenticated` | [x] |
| 4.2 | 列出项目 | `GET /v1/server/projects` | 返回项目列表 | [x] |
| 4.3 | 获取项目 | `GET /v1/server/projects/default` | 返回 default 项目详情 | [x] |
| 4.4 | 创建 API Key | `POST /v1/server/api-keys` | 返回 key + **一次性** secret | [x] |
| 4.5 | 列出 API Keys | `GET /v1/server/api-keys` | 列表不含 secret 明文 | [x] |
| 4.6 | 列出用户 | `GET /v1/server/users` | 含 2.1 注册的用户 | [x] |
| 4.7 | 获取用户 | `GET /v1/server/users/{id}` | 返回单个用户（无 password_hash） | [x] |
| 4.8 | 更新用户 | `PATCH /v1/server/users/{id}`，如 `status` | 更新成功 | [x] |
| 4.9 | 创建用户组 | `POST /v1/server/groups` | 返回 group | [x] |
| 4.10 | 列出 / 删除用户组 | `GET` / `DELETE /v1/server/groups/{id}` | 符合 CRUD 预期 | [x] |
| 4.11 | 创建 Bucket | `POST /v1/server/storage/buckets` | 返回 bucket id | [x] |
| 4.12 | 创建文件（gRPC 小文件） | `POST /v1/server/storage/buckets/{id}/files` | 元数据写入 `files` 集合 | [x] |
| 4.13 | 列出 / 获取 / 删除文件 | 对应 GET / DELETE | 符合预期 | [x] |
| 4.14 | 创建 Database | `POST /v1/server/databases` | catalog 有记录 | [x] |
| 4.15 | 创建 Collection | `POST /v1/server/databases/{db}/collections` | 动态表创建 | [x] |
| 4.16 | 创建 Attribute | `POST .../attributes` | 列追加成功 | [x] |
| 4.17 | 创建 Index | `POST .../indexes` | 索引创建成功 | [x] |
| 4.18 | Document CRUD | `POST/GET/PATCH/DELETE .../documents` | 文档增删改查成功 | [ ] |
| 4.19 | Document 列表/计数 | `GET .../documents`、`.../documents/count` + queries | 过滤与计数正确 | [ ] |
| 4.20 | 删除链路 | 按 Database → Collection 逆序删除 | 无残留错误 | [x] |
| 4.21 | 创建成员邀请 | `POST /v1/server/groups/{id}/memberships`，email + pending | 返回 membership，status=pending | [ ] |
| 4.22 | 成员状态（管理员） | `PATCH .../memberships/{id}/status`，body `{"status":"accepted"}` | 仅当邮箱对应用户已注册时成功 | [ ] |
| 4.23 | 成员角色 | `PATCH .../memberships/{id}`，body `{"roles":["admin"]}` | roles 更新成功 | [ ] |
| 4.24 | 删除用户组级联 | `DELETE /v1/server/groups/{id}` | group 及 memberships 均被删除 | [ ] |
| 4.25 | 用户组偏好 | `GET` / `PUT /v1/server/groups/{id}/prefs` | 未设置返回 `{}`；PUT 整体替换并返回更新值；无用户组角色的 users 主体 PUT 返回 403 | [ ] |

---

## 5. Storage HTTP（Multipart）

| # | 验收项 | 操作步骤 | 预期结果 | 通过 |
|---|--------|----------|----------|------|
| 5.1 | Multipart 上传 | `POST /v1/storage/buckets/{bucketId}/files`，`multipart/form-data`，字段 `file` | 201；返回 file id | [x] |
| 5.2 | 下载 | `GET .../files/{fileId}/download`，带合法凭证 | 文件内容与上传一致 | [x] |
| 5.3 | 内联查看 | `GET .../files/{fileId}/view` | Content-Type 正确，可预览 | [x] |
| 5.4 | API Key 上传 | 使用 `X-Api-Key` 认证上传 | 文件归属 API Key 对应项目 | [x] |
| 5.5 | JWT 上传 | 使用用户 `Bearer` token（**不带**伪造的 `X-Torchwood-Project`） | 仅能操作 token 内嵌 project 的资源 | [x] |

**示例：**

```bash
curl -s -X POST "http://localhost:8088/v1/storage/buckets/<BUCKET_ID>/files" \
  -H "X-Api-Key: <API_KEY_SECRET>" \
  -F "file=@./test.txt"
```

---

## 6. 安全加固验收（近期修复项）

| # | 验收项 | 操作步骤 | 预期结果 | 通过  |
|---|--------|----------|----------|-----|
| 6.1 | API Key 跨项目 IDOR | 用项目 A 的 API Key 调 `GET /v1/server/api-keys/{项目B的keyId}` | `NotFound` 或拒绝，不能读到 B 的 key | [x] |
| 6.2 | API Key Scope | 创建 scopes 仅含 `storage` 的 key，调用 `GET /v1/server/users` | `PermissionDenied` | [x] |
| 6.3 | API Key Scope 放行 | scopes 含 `users` 的 key 调 Users 列表 | 成功 | [x] |
| 6.4 | 伪造项目 Header（HTTP 文件） | 用户 JWT + `X-Torchwood-Project: <其他项目>` 上传/下载 | **不应**访问到其他项目文件 | [x] |
| 6.5 | 登出吊销 | SignOut 后使用旧 access token 调 Me | 失败 | [x] |
| 6.6 | Refresh 绑定 Session | SignOut 后使用旧 refresh_token | 失败 | [x] |
| 6.7 | Console Viewer 权限 | 创建 `role=viewer` 的 admin（无 `admin_projects` 记录），带 `X-Torchwood-Project` 调 Server API | `PermissionDenied`（无项目归属） | [x] |
| 6.8 | Console Owner 放行 | `role=owner` 的 admin 带 `X-Torchwood-Project: default` | 可正常访问 Server API | [x] |
| 6.9 | Viewer 授权后 | 在 `admin_projects` 插入 viewer 与 default 关联后重试 | 可访问该项目 | [x] |

**Viewer 授权 SQL 示例：**

```sql
INSERT INTO admin_projects (admin_id, project_id)
VALUES ('<viewer-admin-uuid>', 'default');
```

---

## 7. 审计日志（`audit_logs`）

| # | 验收项 | 操作步骤 | 预期结果 | 通过 |
|---|--------|----------|----------|------|
| 7.1 | 写入记录 | 调用任意需认证的 gRPC 方法（如 `GET /v1/server/users`） | `audit_logs` 表新增一行 | [x] |
| 7.2 | 字段完整性 | 查询最新记录 | 含 `action`（full method）、`status`（success/错误码）、`actor_id`、`actor_kind` | [x] |
| 7.3 | 项目关联 | 带 `X-Torchwood-Project` 的 Admin 请求 | `project_id` 为 header 中项目 | [x] |
| 7.4 | 公开接口 | 调用 `GET /v1/health` | 可不写审计或写匿名记录（按实现）；**不应**导致请求失败 | [x] |

**查询示例：**

```sql
SELECT id, project_id, actor_kind, action, status, created_at
FROM audit_logs
ORDER BY created_at DESC
LIMIT 10;
```

---

## 8. ACCESS_PERMISSION 细粒度鉴权

| # | 验收项 | 操作步骤 | 预期结果 | 通过 |
|---|--------|----------|----------|------|
| 8.1 | Me 需 users 角色 | 有效 end-user token 调 Me | 成功（Principal 含 `users` 角色） | [x] |
| 8.2 | SignOut 需 users 角色 | 有效 end-user token 调 SignOut | 成功 | [x] |
| 8.3 | 无角色 Token | 若可构造缺 `users` 角色的 token（开发调试）调 Me | `PermissionDenied` | [x] |

> 说明：`Me`、`SignOut` 在 proto 中标注为 `ACCESS_PERMISSION` + `permissions: ["users"]`。

---

## 9. 动态文档与查询 DSL（抽样）

> Document 层无独立 HTTP API，通过 Server Databases 元数据 + 系统集合间接验收。

| # | 验收项 | 操作步骤 | 预期结果 | 通过 |
|---|--------|----------|----------|------|
| 9.1 | 系统集合 | Client 注册后查 Users 列表 | `users` 文档存在 | [x] |
| 9.2 | 查询过滤 | `GET /v1/server/users?queries=equal("email","qa@torchwood.local")`（参数格式以 gateway 为准） | 仅返回匹配用户 | [x] |
| 9.3 | 自定义库 | 创建 app 库 + posts 集合 + attribute | 元数据与 schema 一致 | [x] |
| 9.4 | 列表权限 | 非 admin 角色列表用户（若可模拟） | 仅返回有 `_perms` 的文档 | [x] |
| 9.5 | 系统集合不在 default | `GET /v1/server/databases/default/collections`；`GET .../default/collections/users` | List 不含 7 系统集合；未自建则 users NotFound。adapter/专用 RPC 可见 `tw_<project>.users` 且 `is_system=true` | [ ] |
| 9.6 | Databases API 拒 sentinel | 以 databases scope 调 `GET /v1/server/databases/_` 或带 `database_id=_` 的集合/文档 RPC | 全部 `InvalidArgument` | [ ] |
| 9.7 | 业务库可建普通 users | 在 `default` 或 `app` 上 `CreateCollection(users)` | 成功且 `is_system=false`；与系统 `tw_<project>.users` 无关 | [ ] |
| 9.8 | 系统用户只经专用 API | Account / Server Users 读写用户 | 成功；Databases API 摸不到系统 users | [ ] |
| 9.9 | Console Databases 页 | 打开 default 库集合列表 | 无「系统」徽章、无 7 个系统集合；Users / Storage / Groups 走专用页 | [ ] |

---

## 10. 可观测性与运维

| # | 验收项 | 操作步骤 | 预期结果 | 通过 |
|---|--------|----------|----------|------|
| 10.1 | Metrics 端点 | 访问 `:9100` metrics 路径 | Prometheus 格式指标可抓取 | [x] |
| 10.2 | CORS | 浏览器 Console 跨域请求 API | 无 CORS 阻断（同源部署时 N/A） | [x] |
| 10.3 | 优雅错误 | 故意发送畸形 JSON | 返回结构化 error JSON，非 500 裸栈 | [x] |

---

## 11. Client Document API（P1）

**认证**：`Authorization: Bearer <user_access_token>`（Client 用户 JWT，无需 `X-Torchwood-Project`）。

| # | 验收项 | 路径 / 方法 | 预期结果 | 通过 |
|---|--------|-------------|----------|------|
| 11.1 | 创建文档 | `POST /v1/databases/{db}/collections/{coll}/documents` | 201；默认 `user:{id}` 权限 | [ ] |
| 11.2 | 列表文档 | `GET .../documents` | 仅返回有 read 权限的文档 | [ ] |
| 11.3 | 获取/更新/删除 | `GET/PATCH/DELETE .../documents/{id}` | CRUD 符合 `_perms` | [ ] |
| 11.4 | 跨用户隔离 | 用户 A 的 token 读用户 B 的私有文档 | `PermissionDenied` 或空列表 | [ ] |

---

## 12. Client Groups API（P1）

| # | 验收项 | 路径 / 方法 | 预期结果 | 通过 |
|---|--------|-------------|----------|------|
| 12.1 | 创建用户组 | `POST /v1/groups`，body `{"name":"..."}` | 返回 group；创建者为 owner 成员 | [ ] |
| 12.2 | 邀请成员 | `POST /v1/groups/{id}/memberships`，email | pending membership | [ ] |
| 12.3 | 被邀请人接受 | 被邀请用户 token 调 `PATCH .../memberships/{id}/status` `accepted` | status=accepted；group total +1 | [ ] |
| 12.4 | 非被邀请人拒绝 | 其他用户调同上接口 | `PermissionDenied` | [ ] |
| 12.5 | 退出用户组 | `DELETE .../memberships/{id}`（成员本人） | 成员移除；total -1 | [ ] |
| 12.6 | 邀请未注册用户 | 仅邮箱 pending 邀请（用户尚不存在） | 创建成功；接受前需先注册同邮箱账号 | [ ] |

---

## 13. Console 系统管理员管理（P1）

**认证**：Console 会话 cookie（`admin@torchwood.local / Admin@123` 登录），API 基址 `http://localhost:9099`。

| # | 验收项 | 路径 / 方法 | 预期结果 | 通过 |
|---|--------|-------------|----------|------|
| 13.1 | 当前账户 | `GET /v1/console/admins/me` | 返回自身 email/role | [ ] |
| 13.2 | 管理员列表 | `GET /v1/console/admins`（owner 或 admin 角色） | 返回全部管理员（不含密码哈希） | [ ] |
| 13.3 | 新建管理员 | `POST /v1/console/admins`，body `{"email","password","role"}` | 创建成功；密码强度不足 / 邮箱重复返回 4xx | [ ] |
| 13.4 | 改角色 / 重置密码 | `PATCH /v1/console/admins/{id}` | 仅 owner 可调用；admin 角色调用返回 403 | [ ] |
| 13.5 | 删除管理员 | `DELETE /v1/console/admins/{id}` | 仅 owner；删除自己返回 400 | [ ] |
| 13.6 | 最后 owner 保护 | 唯一 owner 尝试删除/降级自己 | `FailedPrecondition`（无法移除最后一个 owner） | [ ] |
| 13.7 | Console 页面 | 侧边栏 System 分组 → Admins 页面 | 列表、新建、编辑、删除可用；非 owner 不显示增删改入口 | [ ] |
| 13.8 | 侧边栏菜单分组 | 打开 Console | Dashboard 置顶；Develop / Auth / System 分组；Projects、Admins 在 System 分组 | [ ] |

---

## 14. 已知不在本次验收范围

以下能力在 `docs/archived/completed-tasks.md` §13 中标记为**未实现或占位**，验收时**不应**作为通过标准：

- [ ] 密码重置、邮箱验证、OAuth、MFA、匿名登录
- [ ] Server 侧创建用户、sessions/tokens 管理、impersonation
- [ ] Document 批量写、relationship、transaction
- [ ] Storage 分片上传、缩略图、file token
- [ ] Functions 真实 Docker 执行
- [ ] Realtime / Webhooks / Events / Messaging
- [ ] 速率限制、完整审计查询 API（Console 页面）

---

## 15. 验收结论

> **状态（2026-06-20）**：P0 §0–§10 已通过自动化测试；**P1 §2.12–§12.6、§3.9–§3.10、§4.18–§4.24 待人工验收**。
> **状态（2026-08-09）**：§13 Console 系统管理员管理 + 侧边栏菜单分组已实现并通过端到端验证。

| 结论 | 勾选 |
|------|------|
| **通过** — P0 + P1 Sprint 1 均满足 | [ ] |
| **有条件通过** — P0 自动验证 + P1 存在非阻塞缺陷 | [ ] |
| **不通过** — 存在阻塞缺陷 | [ ] |

**阻塞缺陷摘要：**

```
（填写）
```

**非阻塞备注：**

```
P0：§6.7–§10.3 已由自动化测试覆盖（见附录 C）。
P1：Document / Groups 有部分集成测试，但仍建议 Console 与 Client API 走查一遍。
```

---

## 附录 A：常用 curl 模板

```bash
# 变量
export BASE=http://localhost:8088
export API_KEY=<your-api-key-secret>
export PROJECT=default

# Server API（API Key）
curl -s "$BASE/v1/server/users" -H "X-Api-Key: $API_KEY"

# Server API（Console Admin）
export ADMIN_TOKEN=<console-jwt>
curl -s "$BASE/v1/server/users" \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "X-Torchwood-Project: $PROJECT"
```

## 附录 B：相关文档

- [已完成任务清单（已归档）](./archived/completed-tasks.md)
- [P0 底座设计（已归档）](./archived/p0-foundation-design.md)
- [路线图](./roadmap.md)
- [README 快速开始](../README.md)

## 附录 C：自动化验收测试

§6.7–§10.3 可通过集成测试自动验证（需本地 Postgres，端口由 `TORCHWOOD_TEST_DATABASE_SOURCE` / `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` 环境变量指定，`task test` 会自动从 `.env` 加载）：

```bash
# P0 验收（§6.7–§9.4）
go test ./tests/acceptance/... -run TestP0 -count=1 -v

# 可观测性（§10.1–§10.3，无需数据库）
go test ./internal/infra/server/... -run TestObservability -count=1 -v

# Storage HTTP（§5.1–§5.5）
go test ./internal/api/serverhttp/... -run TestFileHandler -count=1 -v

# Databases 元数据链路（§4.14–§4.17）
go test ./internal/app/server/... -run TestDatabases_AcceptanceChain -count=1 -v

# P1 Document CRUD（§4.18–§4.19、§11）
go test ./internal/app/server/... -run TestDatabases_DocumentCRUD -count=1 -v
go test ./internal/app/client/... -run TestDatabases_DocumentCRUD -count=1 -v

# P1 Groups Memberships（§4.21–§4.24、§12）
go test ./internal/app/server/... -run TestGroups_Memberships -count=1 -v

# Client Account 会话 / Prefs（§2.12–§2.15）
go test ./internal/app/client/... -run TestAccount_SessionsUpdatePrefs -count=1 -v
```

测试文件：

| 清单章节 | 测试 |
|----------|------|
| §6.7–§6.9 | `tests/acceptance/p0_acceptance_test.go` → `TestP0_Section6_*` |
| §7.1–§7.4 | `tests/acceptance/p0_acceptance_test.go` → `TestP0_Section7_*` |
| §8.1–§8.3 | `tests/acceptance/p0_acceptance_test.go` → `TestP0_Section8_*` |
| §9.1–§9.4 | `tests/acceptance/p0_acceptance_test.go` → `TestP0_Section9_*` |
| §10.1–§10.3 | `internal/infra/server/observability_acceptance_test.go` |
| §4.18–§4.19、§11 | `internal/app/server/documents_integration_test.go`、`internal/app/client/databases_integration_test.go` |
| §4.21–§4.24、§12 | `internal/app/server/groups_memberships_integration_test.go` |
| §2.12–§2.15 | `internal/app/client/account_sessions_test.go` |
