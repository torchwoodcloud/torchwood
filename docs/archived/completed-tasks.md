# Torchwood 已完成任务清单

> [!WARNING] 已作废归档（ARCHIVED）
> 归档日期：2026-08-09
> 归档原因：本清单截止 2026-06-20（提交 `11af6f8`），已严重过期——§13「已知限制」中的 OAuth2、邮箱验证、密码找回、匿名登录、SMS OTP、微信登录等均已实现；内容已被代码与 `docs/roadmap.md` 取代。
> 后续信息源：`docs/roadmap.md`（规划与状态）、代码注释与集成测试。

> 本文档汇总 Torchwood P0 底座及已经落地实现的功能。
> 最新更新：2026-06-20，对应提交 `11af6f8`。

---

## 1. 项目骨架与工程化

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| 初始化 Go module | 完成 | `go.mod`（`github.com/torchwoodcloud/torchwood`，Go 1.25） |
| 配置 Buf 与 protobuf 生成 | 完成 | `buf.yaml`、`buf.gen.yaml`、`genproto/` |
| 定义 Task 工作流 | 完成 | `Taskfile.yml`：install-tools / up / down / migrate / generate-proto / generate-config / wire-all / generate-all / console-install / console-build / console-dev / dev-server / test / build |
| 本地基础设施编排 | 完成 | `docker/local/docker-compose.yml`（PostgreSQL 5433、Redis 6380、MinIO 9000/9001） |
| 环境变量与配置模板 | 完成 | `.env.example`、`configs/config.yaml.template` |
| 依赖注入组装 | 完成 | `cmd/server/provides.go`、`cmd/server/wire.go`、`cmd/server/wire_gen.go` |
| 忽略与源码控制 | 完成 | `.gitignore`（排除 `bin/`、`console/dist/`、`console/node_modules/`、`.env` 等） |

---

## 2. 配置体系

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| protobuf 配置 schema | 完成 | `internal/pkg/config/config.proto` |
| 生成 Go config 类型 | 完成 | `internal/pkg/config/config.pb.go` |
| 环境变量绑定 | 完成 | `internal/pkg/config/bind.go`：统一前缀 `TORCHWOOD_`，点号路径映射为下划线大写 |
| 运行时加载 `.env` | 完成 | `cmd/server/main.go` 通过 `godotenv` 加载 `.env` 后绑定 `./configs` |
| 关键环境变量 | 完成 | `TORCHWOOD_DATA_DATABASE_SOURCE`、`TORCHWOOD_SECURITY_JWT_SECRET`、`TORCHWOOD_STORAGE_S3_ENDPOINT`、`TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID`、`TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` 等 |

配置分组：

- `server.grpc` / `server.http` / `server.metrics` / `server.cors`
- `security.jwt`（secret、access_ttl、refresh_ttl） / `security.api_key.header`
- `data.database` / `data.redis`
- `storage.s3` / `storage.local`
- `functions.docker`
- `telemetry`（占位）

---

## 3. 数据库基础设施

### 3.1 静态元数据表（bun + golang-migrate）

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| 首版迁移脚本 | 完成 | `db/migrations/000001_init_tables.up.sql`、`000001_init_tables.down.sql` |
| bun model 定义 | 完成 | `internal/infra/bun/model/project.go`（Project、APIKey、ConsoleAdmin、DocumentDatabase、DocumentCollection、DocumentAttribute、DocumentIndex） |
| Project 仓库 | 完成 | `internal/infra/bun/bunrepo/project_repo.go` |
| API Key 仓库 | 完成 | `internal/infra/bun/bunrepo/apikey_repo.go` |
| Console Admin 仓库 | 完成 | `internal/infra/bun/bunrepo/console_admin_repo.go` |

静态表覆盖：

- `projects`（项目元数据）
- `api_keys`（项目级 API Key，secret SHA256 哈希存储）
- `console_admins`（全局控制台管理员）
- `document_databases` / `document_collections` / `document_attributes` / `document_indexes`（动态文档层的 catalog）

### 3.2 动态文档 Postgres 适配器

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| 动态 schema + `_tenant` 隔离 | 完成 | `internal/infra/documentdb/postgres.go` |
| `_perms` 权限表 | 完成 | `internal/infra/documentdb/postgres.go`（`ensurePermsTable`、`setPermissions`、`clearPermissions`） |
| Database CRUD | 完成 | `CreateDatabase` / `GetDatabase` / `ListDatabases` / `DeleteDatabase` |
| Collection CRUD | 完成 | `CreateCollection` / `GetCollection` / `ListCollections` / `DeleteCollection` |
| Attribute 新增 | 完成 | `CreateAttribute`（`ALTER TABLE ADD COLUMN`） |
| Index 新增 | 完成 | `CreateIndex`（`CREATE INDEX` / `UNIQUE` / `FULLTEXT`） |
| Document CRUD | 完成 | `CreateDocument` / `GetDocument` / `UpdateDocument` / `DeleteDocument` |
| Document 列表与计数 | 完成 | `ListDocuments` / `CountDocuments` |
| 系统集合自动初始化 | 完成 | `EnsureSystemCollections` + `internal/infra/documentdb/system_collection_specs.go` |

系统集合已定义：

- `users`：email、password_hash、name、status、email_verified、phone、phone_verified、labels、prefs
- `sessions`：user_id、secret_hash、provider、user_agent、ip、country、factors、expire_at
- `buckets`：name、permissions
- `files`：bucket_id、name、mime_type、size、metadata
- `teams`：name、permissions、total
- `memberships`：team_id、user_id、email、name、roles、status、invited_at、joined_at

### 3.3 查询 DSL 与 CRUD 工具

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| Appwrite 风格查询解析器 | 完成 | `pkg/query/query.go` |
| DSL 转 SQL | 完成 | `internal/infra/documentdb/postgres.go`（`buildAppwriteQuery`） |
| 列表/分页/排序抽象 | 完成 | `pkg/crud/`（filter.go、order.go、pagination.go、list.go、repository.go） |

已支持的查询运算符：

- 比较：`equal`、`notEqual`、`lessThan`、`lessThanEqual`、`greaterThan`、`greaterThanEqual`
- 字符串：`contains`、`startsWith`、`endsWith`、`search`
- 范围：`between`
- 空值：`isNull`、`isNotNull`
- 排序：`orderAsc`、`orderDesc`
- 分页：`limit`、`offset`、`cursorAfter`、`cursorBefore`
- 投影：`select`

---

## 4. 认证与授权

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| JWT 签发/解析 | 完成 | `pkg/jwtparser/jwt.go` |
| 密码哈希 | 完成 | `pkg/password/password.go`（bcrypt） |
| 会话 Cookie 签名 | 完成 | `internal/infra/auth/session_cookie.go` |
| 凭证校验器 | 完成 | `internal/infra/auth/validator.go` |
| Principal 上下文 | 完成 | `internal/pkg/contexts/principal.go` |
| gRPC 认证拦截器 | 完成 | `pkg/grpc/interceptor/jwt.go` |
| proto authz 注解 | 完成 | `proto/shared/v1/authz.proto` |
| Console admin 调用 Server API | 完成 | 拦截器允许 admin token 访问 `ACCESS_API_KEY` 方法，需 `X-Torchwood-Project` header |

凭证类型：

- JWT access token（Client 用户）
- 签名 session cookie（Client 浏览器）
- API Key（Server/SDK）
- Console admin JWT（管理后台）

---

## 5. Protobuf API 定义

### 5.1 Client API（面向终端用户）

| 服务 | 方法 | 路径 | 状态 |
|------|------|------|------|
| `AccountService` | `SignUp` | `POST /v1/account/sign-up` | 完成 |
| `AccountService` | `SignIn` | `POST /v1/account/sign-in` | 完成 |
| `AccountService` | `SignOut` | `POST /v1/account/sign-out` | 完成 |
| `AccountService` | `Me` | `GET /v1/account/me` | 完成 |
| `AccountService` | `Refresh` | `POST /v1/account/refresh` | 完成 |
| `AccountService` | `UpdateAccount` | `PATCH /v1/account` | 完成 |
| `AccountService` | `ListSessions` / `DeleteSession` / `DeleteSessions` | `/v1/account/sessions` | 完成 |
| `AccountService` | `GetPrefs` / `UpdatePrefs` | `/v1/account/prefs` | 完成 |
| `DatabasesService` | Document CRUD + List + Count | `/v1/databases/{db}/collections/{coll}/documents` | 完成 |
| `TeamsService` | Team + Membership CRUD / Status | `/v1/teams`、`/v1/teams/{id}/memberships` | 完成 |

文件：`proto/client/v1/account.proto`、`proto/client/v1/databases.proto`、`proto/client/v1/teams.proto`。

### 5.2 Server API（面向管理后台 / Server SDK）

| 服务 | 已支持方法 | 路径 |
|------|-----------|------|
| `ProjectsService` | CreateProject / ListProjects / GetProject | `/v1/server/projects` |
| `APIKeysService` | CreateAPIKey / ListAPIKeys / GetAPIKey / DeleteAPIKey | `/v1/server/api-keys` |
| `UsersService` | ListUsers / GetUser / UpdateUser / DeleteUser | `/v1/server/users` |
| `TeamsService` | CreateTeam / ListTeams / GetTeam / DeleteTeam | `/v1/server/teams` |
| `TeamsService` | Membership CRUD / UpdateStatus | `/v1/server/teams/{team_id}/memberships` |
| `DatabasesService` | Database / Collection / Attribute / Index CRUD | `/v1/server/databases` |
| `DatabasesService` | Document CRUD / List / Count | `/v1/server/databases/{db}/collections/{coll}/documents` |
| `StorageService` | CreateBucket / ListBuckets / GetBucket / DeleteBucket / CreateFile / ListFiles / GetFile / DeleteFile | `/v1/server/storage/buckets`、`/v1/server/storage/buckets/{bucket_id}/files` |
| `HealthService` | Check | `/v1/server/health` |

文件：`proto/server/v1/*.proto`。

### 5.3 Console API

| 服务 | 方法 | 路径 | 状态 |
|------|------|------|------|
| `ConsoleAuthService` | `SignIn` | `POST /v1/console/auth/sign-in` | 完成 |

文件：`proto/console/v1/auth.proto`。

### 5.4 共享定义

- `proto/shared/v1/authz.proto`：服务/方法级访问控制注解（`ACCESS_PUBLIC`、`ACCESS_AUTHENTICATED`、`ACCESS_API_KEY`）
- `proto/shared/v1/common.proto`：`ListRequest`、`Empty`、`ListResponseMeta`
- `proto/shared/v1/error.proto`：错误模型占位

---

## 6. API 传输层实现

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| Client gRPC handler | 完成 | `internal/api/clientgrpc/{account,databases,teams}.go` |
| Server gRPC handlers | 完成 | `internal/api/servergrpc/{projects,apikeys,users,teams,storage,databases,health}.go` |
| Console gRPC handler | 完成 | `internal/api/consolegrpc/auth.go` |
| 自定义 Storage HTTP handler | 完成 | `internal/api/serverhttp/file_handler.go` |
| gRPC 服务器 | 完成 | `internal/infra/server/grpc.go` |
| grpc-gateway + CORS | 完成 | `internal/infra/server/grpc_gateway.go`、`internal/infra/server/cors.go` |
| 错误处理 | 完成 | `internal/infra/server/errors.go` |
| Metrics 服务器 | 完成 | `internal/infra/server/metrics.go` |
| Admin Console SPA 托管 | 完成 | `internal/infra/server/console.go`、`console/embed.go` |

Storage HTTP handler 路由：

- `POST /v1/storage/buckets/{bucketId}/files`：multipart 上传
- `GET /v1/storage/buckets/{bucketId}/files/{fileId}/download`：附件下载
- `GET /v1/storage/buckets/{bucketId}/files/{fileId}/view`：内联查看

---

## 7. 领域层（Domain）

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| Principal / CredentialType | 完成 | `internal/domain/shared/principal.go` |
| Project 领域模型与仓库端口 | 完成 | `internal/domain/projects/project.go`、`repository.go` |
| 动态文档领域模型与端口 | 完成 | `internal/domain/databases/document.go`、`repository.go` |
| 对象存储端口 | 完成 | `internal/domain/storage/object.go` |
| Teams 成员常量与校验 | 完成 | `internal/domain/teams/membership.go` |
| 函数执行器端口 | 完成 | `internal/domain/functions/executor.go` |
| 领域层 provider 集合 | 完成 | `internal/domain/provides.go` |

`DocumentDatabase` 端口方法：

```go
CreateDatabase / GetDatabase / ListDatabases / DeleteDatabase
CreateCollection / GetCollection / ListCollections / DeleteCollection
CreateAttribute / CreateIndex
CreateDocument / GetDocument / UpdateDocument / DeleteDocument
ListDocuments / CountDocuments
EnsureSystemCollections
```

---

## 8. 应用用例层（Application）

| 模块 | 已完成能力 | 关键文件 |
|------|-----------|----------|
| Client Account | 注册、登录、登出、Me、Refresh、更新资料、会话列表/删除、prefs 读写、JWT team roles 注入 | `internal/app/client/account.go` |
| Client Databases | 终端用户 Document CRUD（默认 `user:{id}` 权限） | `internal/app/client/databases.go` |
| Client Teams | 创建团队（含 owner）、邀请、接受/拒绝、退出 | `internal/app/client/teams.go` |
| Server Projects | 创建项目、列表、获取 | `internal/app/server/projects.go` |
| Server API Keys | 创建（返回一次性 secret）、列表、获取、删除 | `internal/app/server/apikeys.go` |
| Server Users | 列表、获取、更新、删除、状态更新 | `internal/app/server/users.go` |
| Server Teams | 团队 CRUD、成员 CRUD、邀请接受/拒绝、角色、级联删除、`ListAcceptedTeamRoles` | `internal/app/server/teams.go` |
| Server Databases | Database / Collection / Attribute / Index / Document CRUD | `internal/app/server/databases.go` |
| Storage | Bucket / File 元数据 CRUD、multipart 上传下载、MinIO 对象存取 | `internal/app/storage/storage.go` |
| Console Auth | 管理员登录、签发 Console JWT | `internal/app/console/auth.go` |
| Functions | executor 用例 stub | `internal/app/functions/functions.go` |

---

## 9. 基础设施适配器

| 适配器 | 状态 | 关键文件 |
|--------|------|----------|
| Postgres / bun 客户端 | 完成 | `internal/infra/clients/database.go` |
| MinIO/S3 对象存储 | 完成 | `internal/infra/storage/minio.go` |
| Docker 函数执行器 stub | 完成 | `internal/infra/functions/docker.go` |
| Auth 校验器 | 完成 | `internal/infra/auth/validator.go`、`session_cookie.go` |
| bun repos | 完成 | `internal/infra/bun/bunrepo/*.go` |
| 动态文档 Postgres adapter | 完成 | `internal/infra/documentdb/postgres.go`、`system_collection_specs.go` |

对象存储接口已实现：

- `EnsureBucket(ctx, bucket)`
- `Put(ctx, bucket, key, reader, size, contentType)`
- `Get(ctx, bucket, key)` 返回 `io.ReadCloser`
- `Delete(ctx, bucket, key)`

对象 key 规则：`{projectID}/{bucketID}/{fileID}`。

---

## 10. Admin Console 前端

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| 项目初始化 | 完成 | `console/package.json`、`console/vite.config.ts`、`console/tsconfig*.json` |
| 路由与鉴权 | 完成 | `console/src/App.tsx`、`console/src/hooks/useAuth.tsx` |
| 布局组件 | 完成 | `console/src/components/Layout.tsx`、`PageHeader.tsx`、`EmptyState.tsx`、`LoadingTable.tsx` |
| shadcn/ui 风格基础组件 | 完成 | `console/src/components/ui/{button,card,input,label,select,skeleton,table,badge}.tsx` |
| 页面 | 完成 | Dashboard、Projects、API Keys、Users、Storage、Databases（含文档编辑器）、Teams（含成员管理） |
| API 客户端 | 完成 | `console/src/api/{client,auth,projects,apiKeys,users,storage,databases,teams}.ts` |
| 错误提示 | 完成 | 全局 axios 拦截器 + `sonner` toast |
| 嵌入 Go 二进制 | 完成 | `console/embed.go` |

技术栈：React 19 + TypeScript 6 + Vite 8 + React Router 7 + TanStack Query 5 + Tailwind CSS 3 + lucide-react + sonner。

---

## 11. 数据初始化与测试

| 任务 | 状态 | 关键文件 |
|------|------|----------|
| 默认数据 seed | 完成 | `cmd/seed/main.go`：创建 default project、console admin（`admin@torchwood.local / Admin@123`）、默认 API Key |
| 集成测试辅助 | 完成 | `internal/testutil/db.go` |
| 查询 DSL 测试 | 完成 | `pkg/query/query_test.go` |
| CRUD 工具测试 | 完成 | `pkg/crud/*_test.go` |
| Account 用例测试 | 完成 | `internal/app/client/account_test.go`、`account_sessions_test.go` |
| Server Document 集成测试 | 完成 | `internal/app/server/documents_integration_test.go` |
| Client Document 集成测试 | 完成 | `internal/app/client/databases_integration_test.go` |
| Teams Membership 集成测试 | 完成 | `internal/app/server/teams_memberships_integration_test.go` |
| P0 自动化验收测试 | 完成 | `tests/acceptance/p0_acceptance_test.go`、`internal/infra/server/observability_acceptance_test.go` |
| 动态文档 adapter 测试 | 完成 | `internal/infra/documentdb/postgres_test.go` |
| 构建验证 | 完成 | `go build ./cmd/server` 通过 |
| 全量测试 | 完成 | `go test ./...` 通过 |

---

## 12. 文档

| 文档 | 用途 | 状态 |
|------|------|------|
| `README.md` | 项目概览、快速开始、常用任务、结构说明 | 完成 |
| `AGENTS.md` | Agent 开发指南、架构约定、Task 用法 | 完成 |
| `docs/tech-decision.md` | 技术选型决策 | 完成 |
| `docs/p0-foundation-design.md` | P0 底座详细设计 | 完成 |
| `docs/p0-design-review.md` | 设计评审与关键决策确认 | 完成 |
| `docs/appwrite-go-migration-modules.md` | Appwrite 功能迁移全景清单 | 完成 |

---

## 13. 当前已知限制 / 半成品

P1 Sprint 1 已落地 Account 会话扩展、Document CRUD、Teams Memberships；以下能力仍待补齐：

- **Client Account**：缺少密码重置、邮箱验证、匿名登录、Magic URL、OAuth、MFA、账号日志；email 修改无重验证流程。
- **Server Users**：缺少创建用户、sessions/tokens 管理、labels/prefs 完整字段映射、密码重置、impersonation。
- **Teams**：缺少团队 prefs（`GET/PUT /v1/server/teams/{id}/prefs`）；Console 创建「已通过」成员时需传 `user_id`（仅邮箱 + accepted 会失败）。
- **Databases**：缺少批量操作、自增/自减、attribute/index 删除与更新、relationship、transaction、vector/geo。
- **Storage**：缺少 preview/缩略图、公开 bucket、file token、分片上传、usage。
- **Functions**：仅为 stub，未接入真实 Docker build/run/execution。
- **Realtime / Webhooks / Events / Messaging**：尚未实现。
- **Project settings**：OAuth providers、platforms、SMTP、email templates、policies 等均未实现。
- **安全增强**：速率限制、完整审计查询 API（Console 页面）未实现。
- **队列/Worker**：未实现，当前为同步调用。

详细待办见 `docs/roadmap.md` §2 与 M1 里程碑。
