# Torchwood 架构总览

> 面向后端开发者的分层、进程与存储总览。以代码为事实源：`AGENTS.md`、`README.md`、`cmd/server/provides.go`、`internal/pkg/config/config.proto`、`proto/`。
> 最新更新：2026-08-23

---

## 1. 产品定位

Torchwood 是 **Appwrite-inspired、AI/Agent-Native 的 BaaS**，Go + PostgreSQL，`gRPC + grpc-gateway` 双表面。

| 能力 | 说明 |
|------|------|
| Protobuf 单一事实来源 | `proto/` → `buf generate` 产出 gRPC stub / gateway / OpenAPI（`genproto/`），Agent 可直接消费 |
| scoped API Key | `x-api-key` 调用 Server API，按 `api_keys.scopes` 限权（`internal/api/interceptor/apikey_scope.go:25`） |
| 结构化鉴权注解 | 每个 gRPC 方法带 `method_auth`（`proto/shared/v1/authz.proto:18`），启动期强校验 |
| 动态文档层 | 运行时建库/集合/文档，无需手工迁移（`internal/infra/documentdb/` + `pkg/query`） |
| 官方 SDK | `sdk/typescript`（HTTP）与 `sdk/go`（gRPC 直连 + `InvokeJSON` 动态分发） |

核心域：项目多租户隔离、用户认证（JWT/session/OTP/OAuth/MFA）、动态文档、S3/MinIO 存储、Docker 函数执行、React Console（`/console/`）。

---

## 2. 技术栈

| 层 | 选型 |
|----|------|
| 语言/框架 | Go 1.26.5 · Lynx（Runner/生命周期/配置绑定） |
| API | gRPC + grpc-gateway；Buf（`buf.yaml`/`buf.gen.yaml`）驱动生成 |
| DI | Wire（`cmd/server/provides.go:30` → `wire_gen.go`） |
| 存储 | bun（静态表）· PostgreSQL 18 · Redis 7 · MinIO/S3 |
| 前端 | React 19 + TS 6 + Vite 8 + TanStack Query + Tailwind/shadcn |

工具链：`Taskfile.yml` 任务编排、`golang-migrate`（`db/migrations/`）、`docker/local/docker-compose.yml`（PG/Redis/MinIO）。

---

## 3. Clean 四层（依赖外→内）

```
internal/api  ──→  internal/app  ──→  internal/domain  ←──  internal/infra
 传输层              用例层              领域层（端口）       适配器层
```

| 层 | 目录 | 职责 | 依赖 |
|----|------|------|------|
| 传输层 | `internal/api` | gRPC handler + 自定义 HTTP（`serverhttp/` multipart/OAuth 回调）、参数校验、错误映射 | → `app` |
| 用例层 | `internal/app` | 业务规则与事务编排，不感知 gRPC/HTTP（`app/client`/`console`/`server`/`storage`/`functions`） | → `domain` |
| 领域层 | `internal/domain` | 模型与端口接口（`*Repo`），无外部依赖（`domain/users` 定义 `UserRepo`） | — |
| 适配器层 | `internal/infra` | 端口实现：`bun/bunrepo`、`documentdb`、`storage`、`queue`、`messaging`、`infra/server` | 实现 `domain` |

公共包：`internal/pkg`（`config`/`database`/`contexts`/`buildinfo`）进程内共享；`pkg/`（`query`/`crud`/`jwtparser`/`password`/`secretbox`/`idgen`/`semaphore`）可复用库。

规则：

- **端口在 domain，实现在 infra**；`app` 只依赖接口，可替实现或注入 mock；
- 传输层不写业务，全部下沉到用例层；
- 列表复用 `pkg/crud`（AIP-132/158/160），动态文档用 `pkg/query` DSL。

---

## 4. 目录树（三段式内部）

```
torchwood/
├── cmd/server/        # 主服务入口：main.go + provides.go + wire.go → wire_gen.go
├── cmd/worker/        # 异步 worker 入口：独立 Wire 装配（同构）
├── cmd/client/        # CLI（lynx-go/commands，sdk/go InvokeJSON，不直连 genproto；import_guard_test 兜底）
├── console/           # React SPA，embed.go //go:embed dist；Vite 代理 /v1
├── proto/             # client/v1 · server/v1 · console/v1 · shared/v1（唯一事实源）
├── genproto/          # 生成产物 *.pb.go / *_grpc.pb.go / *.pb.gw.go / *.swagger.json（禁手改）
├── internal/
│   ├── api/           # clientgrpc | consolegrpc | servergrpc | serverhttp | realtime
│   │   ├── clientgrpc/   # Account、Databases、Groups（Client 面）
│   │   ├── consolegrpc/  # ConsoleAuth、Admins
│   │   ├── servergrpc/   # Projects/Users/Storage/Databases/Functions/APIKeys/Groups/Health/OAuthProviders/Billing/Outbox
│   │   └── serverhttp/   # multipart 上传下载、OAuth 回调、Functions code 上传
│   ├── app/           # client | console | server | storage | functions | shared
│   ├── domain/        # projects/users/auth/databases/storage/functions/billing/audit/shared...
│   ├── infra/         # bun/bunrepo | documentdb | storage | functions/docker.go | auth/validator.go:21 | server/grpc.go:31 | queue | messaging | health
│   ├── pkg/           # config/config.proto + bind.go:21 | contexts(Principal) | database | buildinfo
│   └── testutil/      # 集成测试 DB 辅助（TORCHWOOD_TEST_*，os.Getenv 直读）
├── pkg/               # query(DSL) | crud | grpc/interceptor | jwtparser | password | secretbox | semaphore | idgen | uow
├── sdk/               # typescript/ | go/client+server | demo/
├── configs/config.yaml.template  # 全部键与默认值，敏感键注释环境变量
├── db/migrations/     # golang-migrate SQL
├── buf.yaml / buf.gen.yaml       # Buf v2 驱动
└── Taskfile.yml       # 任务全表
```

`internal/api/app/domain/infra` 三段式为代码组织主轴；`pkg/` 为可对外复用，`internal/pkg` 仅进程内。

---

## 5. 三进程

| 进程 | 入口 | 职责 | 配置校验 |
|------|------|------|----------|
| `server` | `cmd/server` | Lynx Runner：gRPC `127.0.0.1:9060` + gateway/Console SPA `:9080` + Metrics `127.0.0.1:9040` + 自定义 HTTP；注册顺序 `grpc→gateway→realtime→metrics` | `security.jwt.secret` 必填（`provides.go:63`） |
| `dev:worker` | `cmd/worker` | 后台任务消费者：Functions 队列、outbox 分发、chunk 清理、Stream 修剪、计费闭环等；与 server 共享 `app/domain/infra` 但独立 `ProviderSet`（无 `api` 层） | `data.database.source` 必填（`worker/provides.go:108`） |
| `CLI` | `cmd/client` | `bin/torchwood`，`lynx-go/commands` + `sdk/go/server.InvokeJSON` 按 `protoregistry.GlobalFiles` 动态分发；`rpc` 逃生舱覆盖全部 Server RPC，新增 RPC 无需登记。全局旗标在子命令路径之后、位置参数之前给出（环境变量 `TORCHWOOD_CLI_*` 优先）；退出码 0 成功 / 1 参数与校验错 / 2=40x / 3=5xx / 4=429 | `TORCHWOOD_CLI_*` 环境覆盖 |

三者均 `godotenv.Load()` 加载 `.env`，配置绑定走 `config.NewBindConfigFunc()`（`internal/pkg/config/bind.go:21`），Wire 生成见 `04-codegen.md`。

---

## 6. 三类库（物理三层）

| 层 | schema | 内容 | 驱动 | 说明 |
|----|--------|------|------|------|
| 控制面 | `public` | `projects`/`admins`/`admin_projects`/`api_keys`/`audit_logs`/`outbox`+`outbox_dead`/`provider_resource_index`/`billing_*` | bun + golang-migrate | 事件脊柱 + 审计，公库唯一 |
| 项目数据面 | `tw_<project.id>` | 系统静态表 `users`/`sessions`/`identities`/`groups`/`memberships`/`buckets`/`files`（无 `_id`/`_perms`/`_version`） + 账本/Functions/OAuth/文档目录（`internal/infra/projectschema/`） | bun | 每项目一 schema |
| 业务文档面 | `tw_<project.id>_<database.id>` | 用户 collection 真表（`_tenant` 隔离 + `_perms` 文档级权限，`pkg/query` DSL） | documentdb | 每 `(project,database)` 一 schema |

- sentinel `_`（`ident.ProjectDataPlaneID`）仅内部寻址，对外 `RejectExternalDatabaseID`；
- `default` 为普通首库可删可重建；
- DDL 只走两段式 `businessSchema`，永不解析一段式；
- `project.id` / `database.id` 规则见 `docs/developer/06-databases.md`。

---

## 7. 典型调用链

```
HTTP 客户端 / Agent
  │ POST /v1/server/users（x-api-key）
  ▼ grpc-gateway（internal/infra/server/grpc_gateway.go）
  │ JSON↔proto、CORS、header 透传
  ▼ gRPC Server + AuthInterceptor（internal/api/interceptor/jwt.go:77）
  │ 校验 scope/角色、Principal 注入（contexts.WithPrincipal）
  ▼ internal/api/servergrpc.UsersService.CreateUser     【传输层】
  │ 请求校验、Principal 读取
  ▼ internal/app/server.Users                           【用例层】
  │ 业务规则、RequireServerWriteActor 等纵深防御
  ▼ internal/domain/users.UserRepo                      【端口】
  ▼ internal/infra/bun/bunrepo | documentdb             【适配器层】
  ▼ PostgreSQL / Redis / S3
```

认证：`jwt.go:77` → `auth.Validator:21`（`internal/infra/auth/validator.go:21`）校验 `api_key` 的 `Enabled`/`ExpireAt` 与 `project Status==active` → 写入 `Principal`。

代码路径示例：

| 环节 | 位置 |
|------|------|
| gRPC handler | `internal/api/servergrpc/users.go:CreateUser` |
| 用例 | `internal/app/server/users.go` |
| 端口 | `internal/domain/users` (`UserRepo`) |
| 适配器 | `internal/infra/bun/bunrepo` + `internal/infra/documentdb` |
| 注册 | `internal/infra/server/grpc.go:31` + `grpc_gateway.go` |

---

## 8. 近期加固一句话点列

- **W-J 事件脊柱**：`outbox` + `outbox_dead` 死信表，`OutboxService/ListDeadLetters:ReplayDeadLetter`（`outbox:read/write`，`proto/server/v1/outbox.proto:43`）+ gauge `torchwood_outbox_dead`，经济事件信封补 `version`（`updated_at` 纳秒）判序，防重发与乱序（`arch-review-2026-08-fix-plan.md:345`）。
- **W-H 工程门禁**：`golangci-lint run --new-from-rev=origin/main` 棘轮 + 全量 0 warning、`buf breaking --against '.git#branch=origin/main'`、零漂移 `buf generate + config + wire:all + git diff --exit-code`、`go test -race` 全量（`Taskfile.yml:29,172`）。
- **W-K 契约治理**：`ListRequest.filter/order_by` 未实现一律 `reserved` 消灭静默 no-op，client/server 重复 message 抽 `shared` 基底，新增 RPC 须同步 `method_auth` + `apiKeyScopeRules` + `adminRoleMethodRules`（启动期双断言 `grpc.go:94`）。
- **W-I 独立加密密钥**：`security.encryption_key`（`TORCHWOOD_SECURITY_ENCRYPTION_KEY`）隔离静态字段加密（OAuth/TOTP），未配回退 `jwt.secret` 并告警（`internal/pkg/config/crypto.go:10`）。
- **全局信号量**：`pkg/semaphore` Redis `SET NX + TTL` 分布式计数（`build 4` / `run 16`，TTL 5m，多槽 `slot:<idx>`），内存 `InMemory` 回退（`internal/app/functions/functions.go:31`）。
- **逐语句超时**：跨 `bun`/`documentdb` 仓储 `context.WithTimeout 5s/10s` 收敛慢查询与残留连接（`W-H` 收敛项）。

---

## 9. 约束与入口

- gRPC 方法必须带 `method_auth`（或服务 `service_auth` 默认），否则 `collectMethodsByAccess` 启动失败（`internal/infra/server/grpc.go:217`）。
- Proto 删除字段一律 `reserved`（号+名）；更新请求可选字段 `proto3 optional`；时间 `google.protobuf.Timestamp`（JSON RFC3339）。
- 列表复用 `pkg/crud`（AIP-132/158/160），动态文档用 `pkg/query`；配置单一入口 `config.proto`。
- JWT claims 映射与 `pkg/jwtparser` 保持一致；Console 会话 `TORCHWOOD_session_console` HttpOnly cookie（`03-configuration.md §6.2`）。

> 详见 `AGENTS.md`（开发约定总纲）、`docs/roadmap.md` §0（Agent-Native 战略）、`02-quickstart.md`（启动）、`03-configuration.md`（配置）、`04-codegen.md`（生成）、`05-authentication.md`（鉴权）。
