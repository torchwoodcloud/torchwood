# Torchwood 架构总览

> 面向后端开发者的分层、进程与存储总览。以代码为事实源：`AGENTS.md`、`README.md`、`cmd/server/provides.go`、`internal/pkg/config/config.proto`、`proto/`。
> 最新更新：2026-09-07

---

## 1. 产品定位

Torchwood 是 **Appwrite-inspired、AI/Agent-Native 的 BaaS**，Go + PostgreSQL，`gRPC + grpc-gateway` 双表面。

| 能力 | 说明 |
|------|------|
| Protobuf 单一事实来源 | `proto/` → `buf generate` 产出 gRPC stub / gateway / OpenAPI（`genproto/`），Agent 可直接消费 |
| scoped API Key | `x-api-key` 调用 Server API，按 proto `method_auth` 声明的 `api_key_scope` 限权（策略注册表，`05-authentication.md` §3/§6） |
| 结构化鉴权注解 | 每个 gRPC 方法带 `method_auth`（`proto/shared/v1/authz.proto`，策略唯一声明源），启动期强校验 |
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
| 传输层 | `internal/api` | gRPC handler + 自定义 HTTP（`serverhttp/` multipart/OAuth 回调）、拦截器（auth/限流/审计/用量/protovalidate）、参数校验、错误映射 | → `app` |
| 用例层 | `internal/app` | 业务规则与事务编排，不感知 gRPC/HTTP（`app/client`/`console`/`server`/`storage`/`functions`/`documents`） | → `domain` |
| 领域层 | `internal/domain` | 模型与端口接口（`*Repo`、`auth.PolicySet` 策略类型），无外部依赖 | — |
| 适配器层 | `internal/infra` | 端口实现：`bun/bunrepo`、`documentdb`、`storage`、`queue`、`messaging`、`auth`、`projectschema` | 实现 `domain` |
| 装配/运行时 | `internal/runtime` + `cmd/*` | gRPC/gateway/Console SPA/metrics 装配、authz 策略收集（`BuildMethodPolicies`）、CORS、健康检查 | → `api`/`app`/`infra` |

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
├── cmd/torchwood/        # CLI（lynx-go/commands，sdk/go InvokeJSON，不直连 genproto；import_guard_test 兜底）
├── console/           # React SPA，embed.go //go:embed dist；Vite 代理 /v1
├── proto/             # client/v1 · server/v1 · console/v1 · shared/v1（唯一事实源）
├── genproto/          # 生成产物 *.pb.go / *_grpc.pb.go / *.pb.gw.go / *.swagger.json（禁手改）
├── internal/
│   ├── api/           # clientgrpc | consolegrpc | servergrpc | serverhttp | realtime | interceptor
│   │   ├── clientgrpc/   # Account、Databases、Groups、Payments、Assets、Subscriptions（Client 面）
│   │   ├── consolegrpc/  # ConsoleAuth、Admins
│   │   ├── servergrpc/   # Projects/Users/Storage/Databases/Functions/APIKeys/Groups/Health/OAuthProviders/Payments/Assets/Subscriptions/Billing/Outbox
│   │   ├── serverhttp/   # multipart 上传下载、OAuth 回调、Functions code 上传、well-known
│   │   └── interceptor/  # auth（策略执行）、ratelimit、audit、usage、validate(protovalidate)、clientinfo、trusted_proxy
│   ├── runtime/       # 服务装配与运行时：grpc/gateway/Console SPA/CORS/metrics 注册、authz 策略收集（BuildMethodPolicies → PolicySet）、authz-matrix 文档生成
│   ├── app/           # client | console | server | storage | functions | documents | events | shared(四守卫)
│   ├── domain/        # projects/users/auth(含 PolicySet 策略类型)/databases/storage/functions/billing/audit/shared...
│   ├── infra/         # bun/bunrepo | documentdb | storage | functions | auth(validator) | projectschema | events | queue | messaging | health
│   ├── bootkit/       # server/worker 共享启动校验（jwt secret、roles_sig 密钥初始化）与钩子
│   ├── pkg/           # config/config.proto + bind.go | contexts(Principal) | database | buildinfo
│   └── testutil/      # 集成测试 DB 辅助（TORCHWOOD_TEST_*，os.Getenv 直读）
├── pkg/               # query(DSL 糖+typed AST) | crud | grpc/interceptor | jwtparser | password | secretbox | semaphore | idgen | uow
├── sdk/               # typescript/ | go/client+server | demo/
├── configs/config.yaml.template  # 全部键与默认值，敏感键注释环境变量
├── db/migrations/     # golang-migrate SQL（public 控制面 + 全局 catalog 两表 + RLS 函数）
├── buf.yaml / buf.gen.yaml       # Buf v2 驱动
└── Taskfile.yml       # 任务全表
```

`internal/api/app/domain/infra` 三段式为代码组织主轴；`pkg/` 为可对外复用，`internal/pkg` 仅进程内。

---

## 5. 三进程

| 进程 | 入口 | 职责 | 配置校验 |
|------|------|------|----------|
| `server` | `cmd/server` | Lynx Runner：gRPC `127.0.0.1:9060` + gateway/Console SPA `:9080` + Metrics `127.0.0.1:9040` + 自定义 HTTP；装配在 `internal/runtime`（`grpc.go`/`grpc_gateway.go`/`console.go`/`metrics.go`），注册顺序 `grpc→gateway→realtime→metrics` | `security.jwt.secret` 必填（`internal/bootkit/config.go:33`，server/worker 共享）+ authz 策略语义断言（`AssertSemantic`） |
| `dev:worker` | `cmd/worker` | 后台任务消费者：Functions 队列、outbox 分发、chunk 清理、Stream 修剪、计费闭环等；与 server 共享 `app/domain/infra` 但独立 `ProviderSet`（无 `api` 层） | `data.database.source` 必填（`cmd/worker/provides.go:128`） |
| `CLI` | `cmd/torchwood` | `bin/torchwood`，`lynx-go/commands` + `sdk/go/server.InvokeJSON` 按 `protoregistry.GlobalFiles` 动态分发；`rpc` 逃生舱覆盖全部 Server RPC，新增 RPC 无需登记。全局旗标在子命令路径之后、位置参数之前给出（环境变量 `TORCHWOOD_CLI_*` 优先）；退出码 0 成功 / 1 参数与校验错 / 2=40x / 3=5xx / 4=429 | `TORCHWOOD_CLI_*` 环境覆盖 |

三者均 `godotenv.Load()` 加载 `.env`，配置绑定走 `config.NewBindConfigFunc()`（`internal/pkg/config/bind.go:21`），Wire 生成见 `04-codegen.md`。

---

## 6. 三类库（物理三层）

| 层 | schema | 内容 | 驱动 | 说明 |
|----|--------|------|------|------|
| 控制面 | `public` | `projects`/`admins`/`admin_projects`/`api_keys`/`audit_logs`/`outbox`+`outbox_dead`/`provider_resource_index`/`billing_*` + **全局 catalog 两表**（`catalog_databases`/`catalog_collections`，attrs/indexes/permissions JSONB 合一）+ `idempotency_keys` + `document_events_outbox` | bun + golang-migrate | 事件脊柱 + 审计 + 元数据目录，公库唯一 |
| 项目数据面 | `tw_<project.id>` | 系统静态表 `users`/`sessions`/`identities`/`groups`/`memberships`/`buckets`/`files`（无 `_id`/`_acl`/`_version`） + 账本/Functions/OAuth 目录（`internal/infra/projectschema/`；文档目录已全局化迁出至 public） | bun | 每项目一 schema |
| 业务文档面 | `tw_<project.id>_<database.id>` | 用户 collection 真表，**物理表名 = collectionID**；每表 `_tenant` 隔离 + `_acl` 内嵌 ACE + **RLS policy 判定**（`tw_visible`/`tw_can`，`SET LOCAL ROLE` + roles_sig 注入），`pkg/query` typed AST 查询 | documentdb | 每 `(project,database)` 一 schema |

- sentinel `_`（`ident.ProjectDataPlaneID`）仅内部寻址，对外 `RejectExternalDatabaseID`；
- `app` 为 CreateProject 缺省创建的普通首库，可删可重建；
- DDL 只走两段式 `businessSchema`，永不解析一段式；
- 文档面权限判定唯一执行点是 RLS policy（业务集合）；应用 `Principal` 经每请求事务 `SET LOCAL ROLE` + `app.roles`(+roles_sig 验签) 注入，漏注入 fail-closed；
- `project.id` / `database.id` 规则见 `docs/developer/06-databases.md`。

---

## 7. 典型调用链

```
HTTP 客户端 / Agent
  │ POST /v1/server/users（x-api-key）
  ▼ grpc-gateway（internal/runtime/grpc_gateway.go）
  │ JSON↔proto、CORS、header 透传
  ▼ gRPC Server + 拦截器链（clientinfo → auth → ratelimit → audit → usage → validate）
  │   auth：PolicySet 策略门禁 + Principal 注入（internal/api/interceptor/jwt.go:126）
  ▼ internal/api/servergrpc.UsersService.CreateUser     【传输层】
  │ 请求校验、Principal 读取
  ▼ internal/app/server.Users                           【用例层】
  │ 业务规则、RequireServerPrincipal 等纵深防御
  ▼ internal/domain/users.UserRepo                      【端口】
  ▼ internal/infra/bun/bunrepo | documentdb             【适配器层】
  ▼ PostgreSQL / Redis / S3
```

认证：`interceptor/jwt.go` → `auth.Validator`（`internal/infra/auth/validator.go` + `authenticate.go`）校验 `api_key` 的 `Enabled`/`ExpireAt` 与 `project Status==active` → 写入 `Principal`；策略来自 `ProvideMethodPolicies` 收集的 proto 注解（`internal/runtime/authz_policy.go`）。

代码路径示例：

| 环节 | 位置 |
|------|------|
| gRPC handler | `internal/api/servergrpc/users.go:CreateUser` |
| 用例 | `internal/app/server/users.go` |
| 端口 | `internal/domain/users` (`UserRepo`) |
| 适配器 | `internal/infra/bun/bunrepo` + `internal/infra/documentdb` |
| 注册 | `internal/runtime/grpc.go`（gRPC + 拦截器链）+ `grpc_gateway.go` |

---

## 8. 近期加固一句话点列

- **W-J 事件脊柱**：`outbox` + `outbox_dead` 死信表，`OutboxService/ListDeadLetters:ReplayDeadLetter`（`outbox:read/write`，`proto/server/v1/outbox.proto:43`）+ gauge `torchwood_outbox_dead`，经济事件信封补 `version`（`updated_at` 纳秒）判序，防重发与乱序（`arch-review-2026-08-fix-plan.md:345`）。
- **W-H 工程门禁**：`golangci-lint run --new-from-rev=origin/main` 棘轮 + 全量 0 warning、`buf breaking --against '.git#branch=origin/main'`、零漂移 `buf generate + config + wire:all + git diff --exit-code`、`go test -race` 全量（`Taskfile.yml:29,172`）。
- **W-K 契约治理**：`ListRequest.filter/order_by` 未实现一律 `reserved` 消灭静默 no-op，client/server 重复 message 抽 `shared` 基底，新增 RPC 只须在 proto 标 `method_auth`（access + admin_roles/api_key_scope/permissions 一处声明）。
- **authz 策略注册表（机制重设计）**：策略唯一声明在 proto 注解 → `BuildMethodPolicies`（`internal/runtime/authz_policy.go`）启动期收集为 `domainauth.PolicySet`，拦截器/HTTP/realtime/scope 词表/授权矩阵文档全消费同一注册表；Go 侧手写规则表（`apiKeyScopeRules`/`adminRoleMethodRules`）退役；档位（read_only/business_write/delegated_platform/platform_only）由声明派生，语义断言 fail-closed（`05-authentication.md` §3/§7）。
- **请求形状校验 protovalidate**：required/长度/正则/枚举/范围以 `buf.validate` 注解声明在 proto，`ValidateInterceptor` 链尾统一求值（client/server 两面 20 处 handler 手写检查上收），跨字段与业务规则仍留 app 层（`09-api-guide.md` §2.3）。
- **DocumentDB 重设计落地**：全局 catalog 两表（GetCollection 单查询）、`_acl` 内嵌 + RLS policy 判定（`tw_can`/`tw_visible` + roles_sig 验签，GUC 伪造通道封死）、物理表名 = collectionID、keyset-only 分页、写幂等 `request_id`、outbox seq 事件链、execute-tx 事务内核、数组列/vector/KNN（`06-databases.md`）。
- **W-I 独立加密密钥**：`security.encryption_key`（`TORCHWOOD_SECURITY_ENCRYPTION_KEY`）隔离静态字段加密（OAuth/TOTP），未配回退 `jwt.secret` 并告警（`internal/pkg/config/crypto.go:10`）。
- **全局信号量**：`pkg/semaphore` Redis `SET NX + TTL` 分布式计数（`build 4` / `run 16`，TTL 5m，多槽 `slot:<idx>`），内存 `InMemory` 回退（`internal/app/functions/functions.go:31`）。
- **逐语句超时**：跨 `bun`/`documentdb` 仓储 `context.WithTimeout 5s/10s` 收敛慢查询与残留连接（`W-H` 收敛项）。

---

## 9. 约束与入口

- gRPC 方法必须带 `method_auth`（或服务 `service_auth` 默认），否则启动失败（`missing auth policy` / `registered grpc methods missing authz annotation`，`internal/runtime/grpc.go`）。
- Proto 删除字段一律 `reserved`（号+名）；更新请求可选字段 `proto3 optional`；时间 `google.protobuf.Timestamp`（JSON RFC3339）。
- 列表复用 `pkg/crud`（AIP-132/158/160），动态文档用 `pkg/query`；配置单一入口 `config.proto`。
- JWT claims 映射与 `pkg/jwtparser` 保持一致；Console 会话 `TORCHWOOD_session_console` HttpOnly cookie（`03-configuration.md §6.2`）。

> 详见 `AGENTS.md`（开发约定总纲）、`docs/roadmap.md` §0（Agent-Native 战略）、`02-quickstart.md`（启动）、`03-configuration.md`（配置）、`04-codegen.md`（生成）、`05-authentication.md`（鉴权）。
