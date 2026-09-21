# Torchwood 架构总览

本章面向所有开发者，给出系统的整体画像：产品定位、技术栈、分层结构、目录组织、进程拓扑、数据面三层模型与典型调用链。读完本章后，再按需进入后续章节。

> 事实源以代码为准：`AGENTS.md`（开发约定总纲）、`cmd/server/provides.go`、`internal/pkg/config/config.proto`、`proto/`。

---

## 1. 产品定位

Torchwood 是 **AI/Agent-Native 的 BaaS**（Backend as a Service），用 Go + PostgreSQL 实现，对外提供 gRPC 与 grpc-gateway（HTTP/JSON）两个 API 表面。"Agent-Native" 意味着 API 定义本身可被机器消费：Protobuf 是唯一事实来源，从它生成 gRPC stub、HTTP 网关与 OpenAPI 文档，Agent 和自动化脚本可以直接读取并调用；`/.well-known/torchwood` 提供公开的 Agent 可发现性目录（纯 HTTP 面，payload 构造直读授权策略注册表）。

| 能力 | 说明 |
|------|------|
| Protobuf 单一事实来源 | `proto/`（client / server / console / shared）经 `buf generate` 产出 gRPC stub、gateway 代码与 OpenAPI（均在 `genproto/`，禁手改） |
| scoped API Key | 客户端以 `X-API-Key` 调用 Server API；每个方法可调用的 scope 由 proto 上的 `method_auth` 注解声明（见 `05-authentication.md`） |
| 结构化鉴权注解 | 每个 gRPC 方法必须携带 `method_auth`（定义在 `proto/shared/v1/authz.proto`），策略注册表启动期强校验，缺失即启动失败 |
| 动态文档层 | 运行时建库、建集合、读写文档，无需手工迁移（`internal/infra/documentdb/` + `pkg/query`） |
| 事件与实时通道 | outbox 事件脊柱 + Redis Stream 投递；`/v1/realtime` WebSocket 订阅文档与经济事件，支持 seq 断点续传 |
| 官方 SDK | TypeScript SDK（HTTP）与 Go SDK（gRPC 直连 + `InvokeJSON` 动态分发），见 `12-sdk.md` |

核心业务域：项目多租户隔离、用户认证（JWT / session / OTP / OAuth / MFA）、动态文档数据库、S3/MinIO 对象存储、Functions（dispatcher 分发执行、cron / 事件触发器）、realtime 订阅、排行榜（leaderboards）、事件分析（analytics）、审计日志、经济闭环（payments / assets / subscriptions / billing）、版本化资源迁移（runbook）、React 管理后台（`/console/`）。

---

## 2. 技术栈

| 层 | 选型 |
|----|------|
| 语言 / 框架 | Go 1.26 · Lynx（进程 Runner、生命周期、配置绑定） |
| API | gRPC + grpc-gateway；Buf v2（`buf.yaml` / `buf.gen.yaml`）驱动代码生成 |
| 依赖注入 | Wire（`cmd/server/provides.go` → `wire_gen.go`；server / worker / dispatcher / packer 四个服务入口同模式） |
| 存储 | bun（静态表 ORM）· PostgreSQL · Redis · MinIO / S3 |
| 前端 | React + TypeScript + Vite + TanStack Query + Tailwind / shadcn/ui |

工具链：mise（`mise.toml`，工具版本与任务编排唯一入口）、golang-migrate（`db/migrations/` 控制面 + `internal/infra/projectschema/migrations/` 项目面模板）、Docker Compose（`docker/local/docker-compose.yml`，本地 PG / Redis / MinIO）。

---

## 3. Clean 四层

依赖方向自外向内，领域层不依赖任何外层：

```
internal/api  ──→  internal/app  ──→  internal/domain  ←──  internal/infra
  传输层              用例层              领域层（端口）        适配器层
```

| 层 | 目录 | 职责 | 依赖 |
|----|------|------|------|
| 传输层 | `internal/api` | gRPC handler（`clientgrpc/`、`consolegrpc/`、`servergrpc/`）、自定义 HTTP（`serverhttp/`：Storage multipart 上传下载、OAuth 回调、Functions 代码上传、支付回调、函数触发器公开路由、`/.well-known/torchwood`）、realtime WebSocket、拦截器链（auth / ratelimit / audit / usage / validate / clientinfo / trusted_proxy）、错误映射 | → `app` |
| 用例层 | `internal/app` | 业务规则与事务编排，不感知 gRPC / HTTP。按域分子包：`client`、`console`、`server`、`storage`、`functions`、`documents`、`events`、`assets`、`billing`、`payments`、`subscriptions`、`analytics`、`leaderboards`、`shared` | → `domain` |
| 领域层 | `internal/domain` | 领域模型与端口接口（各类 `*Repo`、`auth.PolicySet` 策略类型、`events.Envelope` 事件信封等）。无外部依赖，`domain` 层禁止 import gRPC。子包：`projects`、`users`、`groups`、`auth`、`databases`、`storage`、`functions`、`events`、`audit`、`billing`、`payments`、`subscriptions`、`assets`、`analytics`、`leaderboards`、`messaging`、`idgen`、`runbook`、`shared` | — |
| 适配器层 | `internal/infra` | 端口实现：`bun`（`bunrepo` 仓储集）、`documentdb`、`projectschema`、`storage`、`functions`、`auth`、`events`、`queue`、`messaging`、`realtime`、`clients`（DB/Redis 连接与执行身份注入）、`health`、`idgen`、`billing`、`payments` | 实现 `domain` |
| 装配 / 运行时 | `cmd/server/internal/runtime` 等 | server 私有运行时装配：gRPC / gateway / Console SPA / landing / CORS / metrics / healthz、authz 策略收集（`BuildMethodPolicies`）与授权矩阵文档生成 | → `api` / `app` / `infra` |

两个补充目录约定：

- **业务共享内核** `internal/pkg/`：`config`（config.proto + bind）、`contexts`（Principal）、`bootkit`（启动期钩子与配置辅助：项目 schema 确保等 PreStart 钩子集，server / worker / dispatcher / packer 四进程共享）、`testutil`（集成测试数据库辅助）、`runbook`（runbook 引擎：文件层 / 编排层 / 动词对账决策层，CLI 命令组与 server 侧状态面共享）。
- **通用可复用库** `pkg/`：`buildinfo`、`query`（文档查询 DSL）、`crud`（列表分页抽象）、`jwtparser`、`password`、`secretbox`、`semaphore`、`idgen`、`ident`、`uow`。

此外，四个顶层组件包与 `internal/` 四层同级，各自只被对应的 `cmd/<app>` 入口引用：

- `dispatcher/`：Functions 执行分发器实现（唯一 docker.sock 持有方）；
- `packer/`：git 部署源打包服务实现（浅克隆 + 子目录物化为 zip）；
- `worker/`：后台作业实现（Functions 队列消费、outbox 分发、各类周期任务）；
- `cli/`：Torchwood CLI 实现（经 sdk/go `InvokeJSON` 调 Server API，不 import genproto，`cli/import_guard_test.go` 兜底）。

三条分层规则：

1. **端口在 domain，实现在 infra**。用例层只依赖接口，测试时可替换实现或注入 mock。
2. **传输层不写业务**。参数校验、Principal 读取之外的一切逻辑下沉到用例层。
3. **列表查询复用 `pkg/crud`**（AIP-132/158/160 抽象），动态文档查询用 `pkg/query` DSL，不手拼 SQL 过滤条件。

---

## 4. 目录树

```
torchwood/
├── cmd/
│   ├── server/                 # 主服务入口：main.go + provides.go + wire.go → wire_gen.go
│   │   └── internal/runtime/   # server 私有运行时装配（grpc / gateway / Console SPA / landing /
│   │                           #   CORS / metrics / healthz / authz 策略收集与矩阵文档生成）
│   ├── worker/                 # 异步 worker 入口（main + Wire 装配骨架；作业实现随仓库根 worker/）
│   ├── dispatcher/             # 函数分发器入口（独立进程，专职持有 docker.sock；实现随 dispatcher/）
│   ├── packer/                 # git 部署源打包服务入口（独立进程；实现随 packer/）
│   └── torchwood/              # CLI 入口（main.go + app.go 根命令表装配；实现随仓库根 cli/）
├── console/                    # React SPA，embed.go //go:embed dist；Vite 开发代理 /v1
├── proto/                      # client/v1 · server/v1 · console/v1 · shared/v1（唯一事实源）
├── genproto/                   # 生成产物 *.pb.go / *_grpc.pb.go / *.pb.gw.go / *.swagger.json（禁手改）
├── internal/                   # server 主调用链四层 + 业务共享内核
│   ├── api/                    # clientgrpc | consolegrpc | servergrpc | serverhttp | realtime
│   │                           # | interceptor | shared（文档时间字段等传输层共享工具）
│   ├── app/                    # client | console | server | storage | functions | documents | events
│   │                           # | assets | billing | payments | subscriptions | analytics
│   │                           # | leaderboards | shared
│   ├── domain/                 # projects | users | groups | auth(含 PolicySet) | databases | storage
│   │                           # | functions | events(Envelope) | audit | billing | payments
│   │                           # | subscriptions | assets | analytics | leaderboards | messaging
│   │                           # | idgen | runbook | shared
│   ├── infra/                  # bun/bunrepo | documentdb | projectschema | storage | functions | auth
│   │                           # | events | queue | messaging | realtime | clients | health | idgen
│   │                           # | billing | payments
│   └── pkg/                    # 业务共享内核：config | contexts | bootkit | testutil | runbook
├── dispatcher/                 # 函数分发器实现（docker.sock daemon / 容器池与租约 / 多节点路由 /
│                               #   Redis registry / :9070 HTTP API）
├── packer/                     # git 部署源打包实现（go-git 浅克隆 + SSRF 防护 + zip 物化 + 并发自限）
├── worker/                     # 后台作业实现（Functions 队列消费 / cron 与事件触发器 / outbox 分发 /
│                               #   分片清理 / Stream 修剪 / 计费与订阅 / 资产过期 / 用量与 analytics 聚合 /
│                               #   leaderboards 结榜清理）
├── cli/                        # Torchwood CLI 实现（lynx-go/commands；经 sdk/go InvokeJSON 调 Server API）
├── pkg/                        # 通用可复用库（见 §3）
├── sdk/                        # go/（client · server · functions · query · docexamples）
│                               # | typescript/（client · server · query） | demo/
├── configs/config.yaml.template  # 全部配置键与默认值，敏感键以环境变量注入
├── db/migrations/              # golang-migrate SQL（public 控制面 + 全局 catalog + RLS 函数）
├── buf.yaml / buf.gen.yaml     # Buf v2 生成驱动
└── mise.toml                   # 工具钉版与任务全表（mise tasks 一览）
```

---

## 5. 进程拓扑

系统由五个独立进程组成。server / worker / dispatcher / packer 等服务端常驻入口通过 `godotenv.Load()` 加载部署环境的 `.env`；torchwood CLI **不加载 `.env`**（CLI 常在不可信仓库目录运行，自动加载会被恶意 `.env` 重定向 endpoint/DSN 窃取凭据，`TORCHWOOD_CLI_*` 环境变量需自行 export）。服务端配置统一经 `internal/pkg/config/bind.go` 绑定（五进程共用同一 config schema，各进程只消费自己的分段）：

| 进程 | 入口 | 职责 | 启动期校验 |
|------|------|------|-----------|
| server | `cmd/server` | Lynx Runner，监听三组端口：gRPC `127.0.0.1:9060`、HTTP `:9080`（grpc-gateway + 自定义 HTTP handler + `/v1/realtime` WebSocket + Console SPA `/console/` + landing 页）、metrics `127.0.0.1:9040`。装配代码在 `cmd/server/internal/runtime/`，服务注册顺序 grpc → gateway → realtime-subscriber → metrics | `security.jwt.secret` 必填；authz 策略语义断言（`AssertSemantic`）失败即启动失败 |
| worker | `cmd/worker` | 后台任务常驻进程，12 个 lynx 服务组件：Functions 执行队列消费（内含 cron 触发器调度、事件触发器消费组、孤儿恢复、执行记录清理）、outbox 事件分发、chunk 分片清理、Redis Stream 修剪、支付订单关单、资产过期、订阅计费、用量聚合、leaderboards 清理与结榜、analytics rollup 与维护（完整节奏见 `13-operations.md`）。与 server 共享 `app/domain/infra`，但 Wire 装配独立、无 `api` 层 | `data.database.source` 必填 |
| dispatcher | `cmd/dispatcher` | Functions 执行常驻进程，唯一 docker.sock 持有方：Build / Execute / RemoveImage 全经它分发，resident 容器池 + 租约认领，多节点模型（node_id + Redis registry + 执行路由），`:9070` 单 HTTP API 面（`/healthz`、`/metrics` 豁免 token） | `functions.dispatcher.url` 必填（server/worker 侧寻址） |
| packer | `cmd/packer` | git 部署源打包常驻进程：专职承载不可信 git 输入的重资源操作（浅克隆 + worktree 核算 + 子目录物化为 zip），把 `url@ref[:directory]` 归一为与 zip 源同构的代码包交回 server；server/worker 零 git 流量。无 Redis/DB/docker 依赖，`:9071` 单 HTTP API 面 | `functions.packer.url` 为空 = git 部署源未启用 |
| torchwood CLI | `cmd/torchwood` | 开发者命令行工具，基于 `lynx-go/commands`，经 `sdk/go/server.InvokeJSON` 按 protoregistry 动态分发调用 Server API。`rpc` 命令覆盖全部 Server RPC，新增 RPC 无需在 CLI 登记。退出码契约：0 成功 / 1 参数与校验错 / 2=40x / 3=5xx / 4=429 | `TORCHWOOD_CLI_*` 环境覆盖 |

CLI 的实现细节见 `02-quickstart.md` §7；版本化资源迁移（runbook）命令组见 `19-runbook.md`。

---

## 6. 数据面三层

所有数据落在 PostgreSQL，按用途分为三层 schema：

| 层 | schema | 内容 | 驱动 |
|----|--------|------|------|
| 控制面 | `public` | `projects`、`admins`、`admin_projects`、`api_keys`、`audit_logs`、`provider_resource_index`、`project_oauth_providers`、`idempotency_keys`、`invite_codes`、`runbook_steps`、runtime vars 四表（`runtime_var_sets` / `runtime_vars` / `runtime_var_heads` / `runtime_var_versions`）、`document_events_outbox`（+ `document_events_outbox_dead` 死信）、`tw_secrets`（roles_sig 密钥），以及**全局 catalog 三表**：`catalog_databases` / `catalog_collections`（attrs/indexes/permissions JSONB 合一 + 物理名）与 `catalog_migrations`（schema 演进 copy 迁移任务账本） | bun + golang-migrate |
| 项目数据面 | `tw_<project.id>` | 系统静态表：`sys_users`、`sys_sessions`、`sys_identities`、`sys_groups`、`sys_memberships`、`sys_buckets`、`sys_files`（无 `_tenant` / `_perms` / `_version` / `project_id` 列），以及 Functions（函数 / 触发器 / 部署 / 执行）、支付 / 订阅 / 账单 / 用量、资产、排行榜、analytics、OAuth 目录表（`internal/infra/projectschema/`，模板迁移 000001–000025） | bun，每项目一 schema |
| 业务文档面 | `tw_<project.id>_<database.id>` | 用户 collection 的物理表。物理表名 = collectionID；每表带 `_tenant` 隔离列与内嵌 `_acl` ACE，权限判定在 RLS policy | documentdb，每 (project, database) 一 schema |

关键不变量：

- sentinel 库 `_`（`ident.ProjectDataPlaneID`）仅限内部寻址，对外请求一律 `RejectExternalDatabaseID` 拒绝。
- `app` 是 CreateProject 缺省创建的第一个普通业务库，可删可重建。
- DDL 只走两段式 `businessSchema` 流程，永不解析一段式命名。
- 文档权限判定的唯一执行点是 RLS policy：应用层 Principal 经每请求事务以 `SET LOCAL ROLE` + `app.roles` GUC（附 roles_sig 验签）注入，漏注入一律 fail-closed。
- `project.id` / `database.id` 命名规则与完整模型见 `06-databases.md`。

---

## 7. 典型调用链

以"Agent 通过 API Key 创建用户"为例：

```
HTTP 客户端 / Agent
  │ POST /v1/server/users  （X-API-Key 头）
  ▼ grpc-gateway                     cmd/server/internal/runtime/grpc_gateway.go
  │ JSON ↔ proto 转码、CORS、header 透传
  ▼ gRPC Server + 拦截器链            clientinfo → auth → ratelimit → audit → usage → validate
  │   auth：PolicySet 策略门禁 + Principal 注入（internal/api/interceptor/jwt.go）
  ▼ internal/api/servergrpc.UsersService.CreateUser     【传输层】
  ▼ internal/app/server.Users                           【用例层】
  │   业务规则、RequireServerPrincipal 等纵深防御
  ▼ internal/domain/users.UserRepo                      【端口】
  ▼ internal/infra/bun/bunrepo                          【适配器层】
  ▼ PostgreSQL
```

认证链路：拦截器 `interceptor/jwt.go` 调用 `auth.Validator`（`internal/infra/auth/validator.go`）校验 API Key 的 `Enabled` / `ExpireAt` 与项目 `Status == "active"`，通过后写入 `Principal`（API Key 方法同时允许 admin console session）。策略本身来自 proto 注解——启动期由 `cmd/server/internal/runtime/authz_policy.go` 的 `BuildMethodPolicies` 收集为 `PolicySet`。

| 环节 | 位置 |
|------|------|
| gRPC handler | `internal/api/servergrpc/users.go` |
| 用例 | `internal/app/server/users.go` |
| 端口 | `internal/domain/users`（`UserRepo`） |
| 适配器 | `internal/infra/bun/bunrepo`（+ 动态文档走 `internal/infra/documentdb`） |
| 注册 | `cmd/server/internal/runtime/grpc.go`（gRPC + 拦截器链）、`grpc_gateway.go`（HTTP） |

---

## 8. 关键设计机制

以下机制贯穿多个子系统，是理解代码行为的前提：

- **authz 策略注册表**：授权策略唯一声明在 proto 注解（access 档位 + admin_roles / api_key_scope / permissions），启动期收集为 `PolicySet`。gRPC 拦截器、HTTP、realtime、SDK scope 词表与授权矩阵文档（`docs/developer/authz-matrix.md`，生成漂移即红）全部消费同一注册表。档位（read_only / business_write / delegated_platform / platform_only）由声明派生，语义断言 fail-closed。详见 `05-authentication.md`。
- **请求形状校验上收 proto**：required / 长度 / 正则 / 枚举 / 范围用 `buf.validate` 注解声明，`ValidateInterceptor` 在拦截器链尾统一求值；跨字段与业务规则仍写在用例层。见 `09-api-guide.md`。
- **DocumentDB 权限内核**：文档 ACE 内嵌 `_acl` 数组列，权限判定在 RLS policy（`tw_visible`）完成；角色经 `SET LOCAL ROLE` + `app.roles` GUC 注入，并携带 HMAC 签名（roles_sig）防伪造，验签失败零角色 fail-closed。详见 `06-databases.md`。
- **事件脊柱（outbox）**：业务事务内写 `document_events_outbox` 行（带全局 `seq`），AFTER INSERT 触发器 `pg_notify('tw_outbox')` 唤醒 worker，经 Redis Stream 投递；死信入 `document_events_outbox_dead`，可通过 `OutboxService` 查询与重放。经济事件信封带 `version`（取 `updated_at` 纳秒）用于判序。
- **独立加密密钥**：`security.encryption_key` 隔离静态字段加密（OAuth / TOTP secret）；未配置时回退 `jwt.secret` 并告警（`internal/pkg/config/crypto.go`）。
- **分布式信号量**：`pkg/semaphore` 基于 Redis `SET NX + TTL` 实现多槽计数（Functions 构建 / 运行并发上限），Redis 不可用时回退进程内实现。
- **逐语句超时**：bun / documentdb 仓储统一以 `context.WithTimeout`（5s / 10s）收敛慢查询与连接占用。
- **工程门禁**：`golangci-lint` 全量门禁、`buf breaking` 对照 origin/main、生成物零漂移检查（CI 在 generate 后 `git diff --exit-code`）、`go test -race` 全量。见 `11-testing.md`。
- **契约治理**：未实现的 `ListRequest.filter` / `order_by` 一律 `reserved`（消灭静默 no-op）；client / server 重复 message 抽 `proto/shared` 基底；删除字段一律 `reserved`（字段号 + 字段名）。

---

## 9. 硬性约束速查

- gRPC 方法必须带 `method_auth`（或服务级 `service_auth` 默认），否则启动失败。
- Proto 删除字段一律 `reserved`；更新类请求的可选字段用 `proto3 optional`；时间字段一律 `google.protobuf.Timestamp`。
- 列表查询复用 `pkg/crud`，动态文档查询用 `pkg/query`；配置单一入口 `internal/pkg/config/config.proto`。
- JWT claims 保持与 `pkg/jwtparser` 的映射兼容；Console 会话走 `TORCHWOOD_session_console` HttpOnly cookie（见 `03-configuration.md`）。

---

## 相关文档

- `AGENTS.md` — 开发约定总纲（必读）
- `docs/roadmap.md` §0 — Agent-Native 战略
- `02-quickstart.md` — 环境搭建与启动
- `03-configuration.md` / `04-codegen.md` — 配置体系与代码生成
- `05-authentication.md` — 认证与授权详解
