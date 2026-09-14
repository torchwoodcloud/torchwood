# 技术选型决策（Appwrite Go + PostgreSQL 迁移）

> 本文档基于 `D:\\Codes\\qiulin\\Torchwoodwork` 现有工程实践，对 Lynx、Wire、bun、grpc-gateway + gRPC、DDD/Clean 架构进行可行性评估，并给出推荐选型。

## 1. 选型结论（一句话版）

- **核心框架**：沿用 `Lynx`（已具备 config/logging/HTTP/gRPC/scheduler/pubsub/telemetry 生命周期）。
- **依赖注入**：沿用 `Wire`，按模块拆分 ProviderSet。
- **ORM**：`bun` 负责**系统级静态表**（users/projects/groups/buckets/functions 等）；用户动态 Collection/Document 层使用 **bun 作为连接池 + 原生 SQL/JSONB**（不依赖 bun 的 model 映射）。
- **REST 暴露**：主路径用 **grpc-gateway + gRPC**；文件上传/下载/预览、OAuth 回调、Realtime WebSocket 等走 **独立 HTTP handler**，与 gateway 共存。
- **业务架构**：**DDD/Clean Architecture**，目录结构与 fleetwork 对齐：`api / app / domain / infra`。

---

## 2. 逐项评估

### 2.1 Lynx（核心框架）

| 维度 | 评估 |
|------|------|
| 现状 | fleetwork 已深度使用，提供 `lynx.New`、`lynx.Component`、`config 绑定`、`logger`、`health check`、`graceful shutdown`、`grpc/http server wrapper`。 |
| 优点 | 与现有工程一致；启动流程、组件生命周期、telemetry 都已就绪；降低迁移学习成本。 |
| 注意点 | Lynx 的 HTTP server 需要接收一个 `http.Handler`。grpc-gateway 的 `runtime.ServeMux` 可直接作为 handler；自定义文件/OAuth/WS 路由可再包一层 `http.ServeMux` 做前缀分发。 |
| **结论** | **采用**。 |

### 2.2 Wire（依赖注入）

| 维度 | 评估 |
|------|------|
| 现状 | fleetwork 已用 Wire 生成 `cmd/server/wire_gen.go`，provider 分布在 `internal/api`、`internal/app`、`internal/domain`、`internal/infra`。 |
| 优点 | 编译期生成、无反射、与 Clean Arch 分层天然契合；新模块只需新增 `ProviderSet` 并 `wire_all`。 |
| 注意点 | 动态依赖（如按 project 切换 database connection/namespace）不适合直接走 Wire，应在 runtime 通过 factory 创建。 |
| **结论** | **采用**。系统级单例用 Wire 注入；项目/租户级 DB factory 在 runtime 创建。 |

### 2.3 bun ORM

#### 2.3.1 适合场景：系统级静态表

fleetwork 中 bun 的使用方式与效果：

- 模型定义：`internal/infra/bun/model/*.go`（`bun.BaseModel` + struct tags）。
- 迁移：`db/migrations/*.sql`（golang-migrate 风格），不依赖 bun auto-migration。
- CRUD：`internal/infra/bun/bunrepo/*.go` 使用 `NewInsert/NewUpdate/NewSelect/NewDelete`。
- 列表查询：`pkg/crud` 抽象了 filter/order/pagination，bun repo 通过 `FieldMappings` 把前端字段映射到 SQL 表达式。

Appwrite 中可同样用 bun 建模的系统表：

- `projects`、`tenants`/`organizations`
- `users`、`groups`、`memberships`
- `buckets`、`files`（元数据）
- `functions`、`deployments`、`executions`
- `attributes`、`indexes`、`collections`（schema 元数据）
- `webhooks`、`api_keys`、`audit`、`outbox`

这些表结构固定，bun 非常适合。

#### 2.3.2 不适合场景：用户动态 Collection/Document

Appwrite 的核心是**用户可动态创建 collection、attribute、index、document**。Utopia Database 的 Postgres adapter 采用：

- 每个 collection 对应一张真实表（`_id/_uid/_tenant/_createdAt/_updatedAt/<attrs>/_permissions`）。
- 权限单独放在 `<collection>_perms` 表。
- 用户创建 attribute 时执行 `ALTER TABLE ... ADD COLUMN`。
- 查询时根据 attribute 类型生成 SQL，并 JOIN 权限表。

bun 的局限：

- bun 是 **struct/model 驱动**的 ORM，不支持运行时根据任意 schema 生成表/列并做类型安全 CRUD。
- `bun.NewCreateTable()` 需要已知 struct；`NewInsert().Model(&x)` 需要已知模型类型。
- 动态关系、动态索引、动态权限 JOIN 无法直接通过 bun 高级 API 表达。

#### 2.3.3 可行方案：bun + 原生 SQL 混合

方案 A（推荐 P0/P1 快速推进）：**JSONB 单表存储用户文档**

- 复用 fleetwork 的 `collection_records` 思路：
  - 表结构固定：`id, tenant_id, project_id, collection_key, data JSONB, created_at, updated_at, created_by, updated_by`。
  - 可额外增加 `_permissions JSONB` 或独立 `_perms` 表。
- 优点：
  - 与 bun 兼容（`map[string]any` 直接映射 JSONB）。
  - schema 变更成本低，创建 collection 只需插入元数据，无需 DDL。
  - fleetwork 已有 `pkg/crud` + JSONB 字段映射实践。
- 缺点：
  - 复杂查询、关系、唯一索引、全文搜索性能弱于真实列。
  - 与 Appwrite 原生 Postgres adapter 不兼容（若需要复用旧数据则不行）。

方案 B（推荐若要求 Appwrite 完全兼容/高性能）：**动态列表 + 原生 SQL adapter**

- 在 `bun.DB` 之上实现一个独立的 `DocumentDB` adapter：
  - 使用 `db.NewRaw(...)` / `db.ExecContext(...)` 执行动态 DDL（`CREATE TABLE`、`ALTER TABLE`、`CREATE INDEX`）。
  - 使用 `pgx`/`bun` 的 raw query 做 CRUD，扫描到 `map[string]any` 或动态 document struct。
  - bun 仅作为**连接池、事务管理、参数绑定**层。
- 优点：
  - 与 Appwrite Postgres adapter  schema 对齐。
  - 能充分利用 PostgreSQL 的列索引、GIN/GIST/HNSW、关系约束。
- 缺点：
  - 工作量大，需要重新实现 Utopia Database 的 Postgres adapter 逻辑。
  - 动态 DDL 在连接池、并发、schema 版本管理上需要额外注意。

#### 2.3.4 bun 结论

| 场景 | 方案 |
|------|------|
| 系统静态表 | **bun 模型 + SQL 迁移** |
| 用户动态文档 | **bun 仅作连接池/SQL 构建器 + 原生 SQL 动态 adapter**；或 **JSONB 单表（牺牲部分 Appwrite 原生兼容性）** |

**最终建议**：

- **P0/P1 核心系统（Account/Users/Groups/Projects/Storage/Functions）**：bun 模型化。
- **Databases 模块**：
  - 如果目标是“Appwrite 功能等价但允许新 schema”：采用 **JSONB 单表 + GIN 索引**，快速落地。
  - 如果目标是“与 Appwrite 现有 Postgres 数据/行为完全兼容”：采用 **动态列表 + 原生 SQL adapter**，bun 仅打底。

> 两种方案可以并存：系统元数据用 bun，用户数据层用动态 adapter。后续若性能不足，可逐步把热 collection 从 JSONB 单表迁移到真实列。

---

### 2.4 grpc-gateway + gRPC 作为 REST 层

#### 2.4.1 fleetwork 现状

- gRPC 服务注册：`internal/infra/server/grpc.go`。
- grpc-gateway 注册：`internal/infra/server/grpc_gateway.go`，通过 `runtime.NewServeMux` + `RegisterXxxServiceHandlerFromEndpoint`。
- 认证：proto 注解 `(lynx.shared.method_auth)` + `interceptor.NewAuthInterceptor`。
- 错误：`HTTPErrorHandler` 自定义错误映射。
- 路径：使用 `google.api.http` 注解（AIP 风格）。

#### 2.4.2 应用到 Appwrite 的适配点

**可行之处：**

- 所有结构化 CRUD（users/groups/databases/buckets/functions/variables 等）都可用 gRPC + gateway 暴露。
- proto 校验（buf.validate）可替代 Appwrite 的输入校验。
- 自动生成 OpenAPI/ Swagger 方便 Console 对接。
- 与 fleetwork 的 auth interceptor、error handler、logging/telemetry 无缝集成。

**需要特殊处理的地方：**

1. **`$` 前缀字段**  
   Appwrite 响应中大量字段以 `$` 开头：`$id`、`$collectionId`、`$permissions`、`$createdAt`、`$updatedAt`。  
   Protobuf 中可用 `json_name` 选项：
   ```proto
   string id = 1 [json_name = "$id"];
   string collection_id = 2 [json_name = "$collectionId"];
   repeated string permissions = 3 [json_name = "$permissions"];
   ```
   Go 的 `protojson` 会尊重 `json_name`，grpc-gateway 使用 `protojson` 编解码，因此 HTTP JSON 输出可保持 `$id`。需要验证是否对 `$` 有特殊处理，理论上 `json_name` 是任意字符串。

2. **Appwrite 的 `queries` DSL**  
   Appwrite List 接口通过 `queries` 数组传过滤/排序/分页条件。  
   Protobuf 中可定义为 `repeated google.protobuf.Value queries` 或 `repeated string queries`，在 app 层调用统一的 Query DSL parser 转换为 SQL。

3. **文件上传 / 下载 / 预览**  
   grpc-gateway 对 multipart 上传支持有限；文件下载需要控制 `Content-Type`、`Content-Disposition`、`Range` 等。  
   建议这些接口**不走 grpc-gateway**，而是写独立 HTTP handler（挂载在 `/v1/storage/buckets/{bucketId}/files/{fileId}/download` 等路径）。

4. **OAuth2 回调 / 重定向**  
   OAuth 流程需要 302 重定向、query 参数解析、state 校验。适合独立 HTTP handler。

5. **Realtime WebSocket**  
   必须由独立 WebSocket server 提供，不能通过 grpc-gateway。

6. **路径风格**  
   Appwrite 使用 `/v1/account/sessions/email`、`/v1/databases/{databaseId}/collections/{collectionId}/documents` 等。  
   grpc-gateway 支持这类自定义路径，但需要在 proto 中精确声明，维护成本较高。

#### 2.4.3 结论

| 接口类型 | 推荐方案 |
|----------|----------|
| 结构化 CRUD（Account/Users/Groups/Databases/Storage/Functions 元数据） | **grpc-gateway + gRPC** |
| 文件上传/下载/预览 | **独立 HTTP handler**（复用 fleework 的 storage URL resolver / S3 预签名） |
| OAuth2 回调/重定向 | **独立 HTTP handler** |
| Realtime WebSocket | **独立 WebSocket server** |
| 健康检查 / metrics | Lynx 自带或独立 handler |

---

### 2.5 DDD / Clean Architecture

#### 2.5.1 与 fleetwork 目录对齐

fleetwork 目录结构：

```
internal/
  api/        # gRPC handlers / transport
  app/        # use-cases
  domain/     # entities + repository ports + domain services
  infra/      # adapters (bun repos, server, storage, ...)
```

Appwrite 迁移可直接沿用：

```
internal/
  api/
    grpc/          # gRPC service implementations
    gateway/       # grpc-gateway custom marshaler / error handler
    http/          # 文件、OAuth、WS 等自定义路由
    shared/        # auth context, pagination, error conversion
  app/
    account.go
    users.go
    groups.go
    databases.go
    storage.go
    functions.go
    ...
  domain/
    shared/        # identity, authz, roles, permissions, events, file URL resolver
    projects/
    users/
    groups/
    databases/     # DocumentDB port, Collection/Document/Attribute/Index entities
    storage/
    functions/
  infra/
    bun/
      model/       # bun models for system tables
      bunrepo/     # system repos
      mapper/      # domain <-> bun model
    documentdb/    # Postgres dynamic adapter (if方案B)
    or jsondoc/    # JSONB single-table adapter (if方案A)
    storage/       # S3 / local adapter
    functions/     # Docker executor adapter
    queue/         # PG or Redis queue
    server/        # Lynx components (grpc, gateway, http, ws, metrics)
```

#### 2.5.2 关键领域端口

- `domain/users/repo.UsersRepo`：用户 CRUD、密码哈希、session/token。
- `domain/groups/repo.GroupsRepo`：用户组、membership、角色。
- `domain/databases/repo.DocumentDatabase`：动态 collection/document/attribute/index/query 的抽象。
- `domain/storage/repo.FileRepo / BlobStore`：文件元数据与二进制存储。
- `domain/functions/repo.FunctionRepo / Executor`：函数、部署、执行。
- `domain/shared/EventPublisher`：事件发布。
- `domain/shared/Queue`：任务队列抽象。

#### 2.5.3 结论

**采用 DDD/Clean Architecture**，并严格保持：

- domain 层不依赖 bun/grpc/lynx 等框架。
- infra 层实现 domain ports。
- app 层编排 use cases，处理事务边界。
- api 层只做协议转换。

---

## 3. 推荐技术栈总览

| 层级 | 技术 |
|------|------|
| 框架 / 生命周期 | `lynx-go/lynx` |
| DI | `google/wire` |
| 数据库驱动 / SQL 构建 | `pgx/v5` + `uptrace/bun` |
| 系统表 ORM | `bun` model + golang-migrate |
| 动态文档层 | 原生 SQL adapter（方案 B）或 JSONB 单表（方案 A） |
| 结构化 API | `gRPC` + `grpc-gateway/v2` |
| 文件 / OAuth / WS | 标准库 `net/http` + `gorilla/websocket` |
| 认证 | JWT（`golang-jwt/jwt/v5`）+ session/api-key，参考 fleework `internal/grpc/interceptor/jwt.go` |
| 密码/MFA | `golang.org/x/crypto/argon2|bcrypt|scrypt` + `pquerna/otp` |
| 队列 | 优先 **PostgreSQL 队列表**（纯 PG 栈）；保留 Redis 适配可选 |
| 缓存 | `go-redis` / `ristretto` |
| 调度 | `robfig/cron/v3` 或 lynx scheduler |
| 函数执行 | Docker SDK (`github.com/docker/docker`) |
| 对象存储 | `aws-sdk-go-v2/service/s3` 或 minio-go |
| 图片处理 | `disintegration/imaging` 或外部服务 |
| 配置 | fleework config proto + `FLEETWORK_` 环境变量 |
| 构建/生成 | `buf`、`protoc-gen-go`、`protoc-gen-go-grpc`、`protoc-gen-grpc-gateway`、`wire` |

---

## 4. 关键风险与应对

| 风险 | 影响 | 应对 |
|------|------|------|
| bun 无法原生支持动态 schema | 数据库层工作量大 | 系统表用 bun；动态层用原生 SQL adapter 或 JSONB 单表。 |
| grpc-gateway 对 `$` 字段、multipart、重定向支持有限 | 部分接口需独立 HTTP handler | 文件/OAuth/WS 单独实现；`$` 字段用 `json_name` 验证。 |
| Appwrite 查询 DSL 复杂 | List 接口实现成本高 | app 层统一 Query parser，转换为 SQL/jsonb 表达式。 |
| 与 Appwrite SDK/Console 完全兼容需要大量 proto | 初期工作量巨大 | P1 先保证功能等价，路径/字段逐步对齐；必要时保留兼容层。 |
| 动态 DDL 在连接池/并发下不稳定 | 性能/可靠性 | DDL 操作串行化；使用 schema 级别锁或事务；生产环境避免高频 DDL。 |
| OAuth2 provider 数量多 | 配置爆炸 | 先实现 5-10 个主流 provider，其余按需扩展。 |

---

## 5. 已确认决策

| 问题 | 决策 |
|------|------|
| 动态文档层方案 | **方案 B：动态列 + 原生 SQL adapter**（对齐 Appwrite Utopia Database Postgres adapter，按 collection 建真实表和 `_perms` 表）。 |
| Appwrite SDK/Console 兼容性 | **参考即可，不强兼容**。proto/路径/错误结构可按 fleetwork 风格重新设计，必要时保留概念映射。 |
| 队列 | **允许使用 Redis**（fleetwork 已有 Redis，可作为队列/缓存）。 |
| 文件二进制存储 | **S3 兼容存储**（本地开发可用 MinIO，生产对接 S3/MinIO/兼容对象存储）。 |
| Functions 执行环境 | **Docker**（通过 Docker SDK 构建/运行容器）。 |
| Realtime | **纳入 P1**（与 Account/Users/Groups/Databases/Storage/Functions 同阶段实现）。 |

---

## 6. 基于决策的修正要点

1. **数据库层必须实现 Utopia-style Postgres adapter**：
   - 每个 collection 一张真实表（`_id BIGINT PK / _uid TEXT / _tenant INT / _createdAt / _updatedAt / <动态列> / _permissions TEXT`）。
   - 权限单独 `<collection>_perms` 表，查询时 JOIN。
   - 多租户通过 `shared tables + _tenant` + schema/namespace 实现。
   - 支持动态 `CREATE/ALTER TABLE`、`CREATE INDEX`、关系、upsert、JSONB/vector/spatial 扩展。
   - bun 仅用于系统表和连接池；动态层用 `bun.DB` 执行原生 SQL。

2. **REST 接口分层**：
   - grpc-gateway 负责结构化 CRUD（users/groups/databases/buckets/functions 等）。
   - 独立 HTTP handler 负责：文件上传/下载/预览、OAuth2 回调/重定向、Realtime WebSocket。

3. **Redis 复用**：
   - 队列：可用 Redis Stream/List（fleetwork 已有 go-redis）。
   - 缓存：session/cache/presence。
   - 事件广播：Realtime 可通过 Redis Pub/Sub 做多实例广播。

4. **Storage**：
   - 元数据用 bun 模型存 PostgreSQL。
   - 二进制通过 S3 预签名 URL 上传/下载；本地开发用 MinIO。

5. **Functions**：
   - 元数据用 bun 存 PostgreSQL。
   - 部署包存 S3。
   - 执行器用 Docker SDK 启动运行时容器。

6. **Realtime**：
   - WebSocket server 独立运行。
   - 事件源来自数据库变更/函数执行/存储变更；通过 Redis Pub/Sub 广播到多实例，再推送到客户端。

---

## 7. 下一步建议

按以下顺序推进：

1. **P0 底座实现**
   - 基于 fleetwork 初始化项目结构（Lynx + Wire + config + logging/telemetry）。
   - 创建系统表 bun 模型与 migrations：projects、users、groups、memberships、buckets、files、functions、deployments、executions、api_keys、webhooks、audit、outbox。
   - 实现基础认证中间件（JWT / session / API key），支持 proto authz 注解。
   - 搭建 grpc-gateway 与独立 HTTP handler 共存的服务器框架。

2. **动态数据库层原型**
   - 定义 `domain/databases/repo.DocumentDatabase` 端口。
   - 实现 Postgres adapter：create database(schema)、create collection、add attribute、create index、CRUD、find with queries、permissions JOIN、relationships。
   - 编写单元/集成测试验证动态 DDL 和权限查询。

3. **Account / Auth 端到端模块**
   - proto + grpc-gateway + app use-case + bun repo。
   - 实现 sign-up / sign-in / session / JWT / email verification / password recovery。

4. **后续 P1 模块**
   - Users、Groups、Databases、Storage、Functions、Project settings、Health。

5. **Realtime P1**
   - WebSocket 连接、channel 订阅、presence、事件广播。
