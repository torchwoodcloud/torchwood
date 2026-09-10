# Torchwood

[English](README.md) | **简体中文**

Torchwood 是一个受 Appwrite 启发、**AI/Agent-Native** 的后端即服务（BaaS）平台，使用 Go + PostgreSQL + gRPC/grpc-gateway 构建，提供用户认证、动态文档数据库、文件存储、函数执行与 Admin Console —— API 与工具链自设计之初即面向 LLM Agent、自动化与 MCP。

## 功能特性

- **Agent 原生 API**：Protobuf 为单一事实来源，`buf generate` 产出 gRPC stub、grpc-gateway handler 与 OpenAPI（`genproto/`）；细粒度 scope 的 API Key（`x-api-key`）供 Agent/自动化调用 Server API。
- **项目管理**：多项目隔离，每个 `(project.id, database.id)` 对应一个 PostgreSQL schema。
- **用户认证**：邮箱注册/登录、JWT access/refresh（含轮换）、会话 Cookie、Email/Phone OTP、OAuth2（Google/GitHub/WeChat）、匿名会话、Magic URL、一次性 JWT、TOTP MFA 与邮箱变更二次确认。
- **动态文档数据库**：schema-per-database，`_tenant` 隔离、`_acl` 文档级权限（Postgres RLS policy 判定），typed 查询 AST（`pkg/query`，Appwrite 风格 DSL 仅作客户端糖），支持 keyset 分页、批量操作、字段增量与数组原子更新。
- **文件存储**：S3/MinIO 兼容，上传/下载/在线预览、缩略图、公开 bucket、HMAC File Token、分片上传与断点续传。
- **函数执行**：Docker 构建/运行执行器，同步/异步执行，异步 `cmd/worker` 与保留策略。
- **Admin Console**：React SPA，嵌入 Go 二进制，路径 `/console/`。

## 技术栈

### 后端

- Go 1.26.5 · [Lynx](https://github.com/lynx-go/lynx) 服务框架 · gRPC + grpc-gateway · [Wire](https://github.com/google/wire) 依赖注入 · [bun](https://github.com/uptrace/bun) ORM · PostgreSQL · Redis · MinIO/S3

### 前端

- React 19 + TypeScript 6 · Vite 8 · React Router 7 · TanStack Query 5 · Tailwind CSS 3 + shadcn/ui · sonner · lucide-react

精确版本以 `go.mod` 与 `console/package.json` 为准。

## 快速开始

### 前置要求

Go 1.26.5+、Node.js 22+ + pnpm、Docker + Compose、[Task](https://taskfile.dev/)（`go install github.com/go-task/task/v3/cmd/task@latest`）。

### 1. 启动基础设施

```bash
task docker:up
```

启动 PostgreSQL（5432）、Redis（6379）与 MinIO（9000/9001）。端口可通过 `.env` 中的 `POSTGRES_PORT`/`REDIS_PORT`/`MINIO_API_PORT`/`MINIO_CONSOLE_PORT` 覆盖。

### 2. 配置环境变量

```bash
cp .env.example .env
```

关键变量（`TORCHWOOD_` 前缀，schema 见 `pkg/config/config.proto`，绑定见 `pkg/config/bind.go`）：

```env
TORCHWOOD_DATA_DATABASE_SOURCE=postgres://torchwood:torchwood@127.0.0.1:5432/torchwood?sslmode=disable
TORCHWOOD_SECURITY_JWT_SECRET=dev-only-0123456789abcdef-0123456789abcdef  # ≥32 字符，不得含弱子串
TORCHWOOD_SECURITY_SETUP_TOKEN=dev-setup-0123456789abcdef0123456789abcdef
TORCHWOOD_STORAGE_S3_ENDPOINT=http://127.0.0.1:9000
TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID=minioadmin
TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY=minioadmin
```

生产环境请为 `JWT_SECRET`/`SETUP_TOKEN` 生成强随机值（`openssl rand -hex 32`）；`SETUP_TOKEN` 未设置时首个管理员注册将被拒绝。

### 3. 数据库迁移

```bash
task db:migrate
```

### 4. 工具链与代码生成

```bash
task tools:install   # buf、wire、migrate、protoc-gen-go、golangci-lint（首次）
task generate:all    # buf generate + config proto + wire:all
task console:install # pnpm install（首次）
```

### 5. 构建并运行

```bash
task build        # 先 console:build，再编译 server + worker + CLI 到 ./bin/
./bin/server.exe  # Windows；Linux/macOS 为 ./bin/server
# 开发模式：
task dev:server   # go run ./cmd/server
```

修改 Console 后需先 `task console:build` 再 `task build`，否则 `//go:embed` 打包的是旧 `dist`。

### 6. 首次引导（bootstrap）

打开 `http://127.0.0.1:9080/console/`，登录页自动切换为「初始化设置」表单，填入 `project_id` + `database_id` 与 `SETUP_TOKEN`：

- 创建 owner 管理员（首个管理员固定为 `owner`）；
- 创建指定项目及其系统 `default` 库；若 `database_id` 非 `default`，再创建该业务库。

API Key 不在注册时生成，登录后在 Console **API Keys** 页面创建，以 `x-api-key` 调用 Server API。

### 端点（默认值来自 `configs/config.yaml.template`，非硬编码）

| 入口 | 地址 |
|------|------|
| Admin Console | `http://127.0.0.1:9080/console/` |
| HTTP / grpc-gateway | `http://127.0.0.1:9080/v1/...` |
| gRPC（仅回环） | `127.0.0.1:9060` |
| Metrics | `http://127.0.0.1:9040/metrics` |
| 健康检查 | `http://127.0.0.1:9080/healthz/liveness`, `/healthz/readiness` |

## 常用开发任务

| 任务 | 说明 |
|------|------|
| `task docker:up` / `down` / `clean` | 启动 / 停止 / 清空本地基础设施 |
| `task db:migrate` | 执行 `db/migrations` |
| `task generate:proto` / `generate:config` / `wire:all` / `generate:all` | buf / config proto / Wire |
| `task console:install` / `console:build` / `console:dev` | 前端 pnpm 工作流 |
| `task dev:server` / `task dev:worker` | 直接运行 server/worker |
| `task build` | 构建前端 + server + worker + `bin/torchwood` CLI |
| `task test` | SDK Go/TS 测试 + `go test -v ./... -cover` |
| `task lint` | `go vet` + `golangci-lint` + console lint |

## 项目结构

```
.
├── cmd/server/          # 服务入口与 Wire 装配（provides.go -> wire_gen.go）
├── cmd/worker/          # 异步 worker（函数执行队列消费者，独立 Wire）
├── cmd/torchwood/          # Torchwood CLI（lynx-go/commands，基于 sdk/go InvokeJSON）
├── console/             # Admin Console SPA（embed.go -> //go:embed dist）
├── configs/             # config.yaml.template（及本地 config.yaml）
├── db/migrations/       # golang-migrate SQL 迁移
├── docker/local/        # 本地 Compose：Postgres + Redis + MinIO
├── genproto/            # 生成的 protobuf 代码（*.pb.go / *.pb.gw.go / *.swagger.json）
├── proto/               # protobuf 源文件（client / server / console / shared）
├── internal/
│   ├── api/             # 传输层：clientgrpc / consolegrpc / servergrpc / serverhttp
│   ├── app/             # 用例层（client / console / server / functions / storage）
│   ├── domain/          # 领域模型与端口（audit / auth / databases / functions / users / ...）
│   ├── infra/           # 适配器层（bun / documentdb / storage / queue / messaging / ...）
│   ├── pkg/             # 进程内共享包（config / database / contexts / buildinfo）
│   └── testutil/        # 集成测试辅助
├── pkg/                 # 可复用库（crud / query DSL / jwtparser / password / idgen / semaphore / secretbox）
├── sdk/                 # 官方 SDK：typescript/ + go/ + demo/
├── buf.yaml / buf.gen.yaml
├── Taskfile.yml
└── README.md / README_ZH.md
```

## 架构说明

- **Clean Architecture 四层**：`internal/api`（传输层）→ `internal/app`（用例层）→ `internal/domain`（领域模型与端口）→ `internal/infra`（适配器层）。`domain` 定义接口，`infra` 实现。
- **Wire 注入**：`cmd/server/provides.go` 声明 provider 集合，`cmd/server/wire_gen.go`（`cmd/worker` 同理）由 `task wire:all` 生成；provider 变更后需重新生成。
- **三进程**：`server`（gRPC + gateway + 自定义 HTTP handler + metrics + 嵌入式 Console）、`worker`（函数执行队列消费者，独立 Wire 装配）、`CLI`（`bin/torchwood`，lynx-go/commands + `sdk/go/server` 的 `InvokeJSON`，不直接 import `genproto`/gRPC，`rpc` 逃生舱自动覆盖新增 RPC）。
- **三类数据库**：`public` 控制面与事件脊柱（`projects`、`admins`、`api_keys`、`audit_logs`、`outbox`/`outbox_dead`、全局 catalog 两表 `catalog_databases`/`catalog_collections`，bun + golang-migrate）；`tw_<project.id>` 项目数据面——系统静态表（`users`/`sessions`/`identities`/`groups`/`memberships`/`buckets`/`files`）+ 账本/Functions/OAuth（`internal/infra/projectschema/`）；`tw_<project.id>_<database.id>` 业务文档面——仅放用户 collection（真实表，表名 = collectionID，`_tenant` + `_acl` 内嵌 + RLS policy）。
- **API 形态**：Protobuf 为单一事实来源（`proto/` → `genproto/`），REST 由 grpc-gateway 暴露，文件 multipart 与 OAuth 回调走 `internal/api/serverhttp`；gRPC 方法须带 `method_auth` 注解（启动期收集为策略注册表）。
- **认证**：end-user JWT/会话 Cookie、API Key（以 `keys` + `key:<id>` 双角色参与 `_acl` 判定，不绕过文档权限）、Console admin JWT（`TORCHWOOD_session_console` HttpOnly Cookie）。Admin 通过 `X-Torchwood-Project` header 指定项目。
- **近期加固（点到为止）**：outbox 死信重放 `torchwood admin outbox list-dead/replay`（`document_events_outbox_dead`）；全局 Redis 信号量 `pkg/semaphore`（build 4 / run 16，TTL 租约，内存回退）；仓储层 per-statement 5s/10s `context.WithTimeout`；`golangci-lint` 全量 0 告警、`--new-from-rev=origin/main` 棘轮；`buf breaking --against '.git#branch=origin/main'` 门禁。

## 测试

```bash
task test   # lint:go + sdk/go + sdk/typescript + go test -v ./... -cover
```

集成测试（`internal/infra/documentdb/postgres_test.go`、`internal/app/client/account_test.go` 等）自动创建/销毁 `TORCHWOOD_test` 库。DSN 来自 `TORCHWOOD_TEST_DATABASE_SOURCE` / `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE`（见 `.env.example`），`task test` 自动从 `.env` 加载。

## SDK

详见 [`sdk/README.md`](sdk/README.md)：

- **TypeScript**（`sdk/typescript`，`@torchwood/sdk`）—— 基于 HTTP（grpc-gateway）的 Client + Server API，含 `sdk/demo` 演示。
- **Go**（`sdk/go`，`github.com/torchwoodcloud/torchwood/sdk/go`）—— gRPC 直连薄封装：`client`（终端用户认证，自动刷新 token）与 `server`（API Key + `InvokeJSON` 动态分发，CLI 即基于此）。

```bash
task sdk:install && task sdk:build
task sdk:demo   # http://localhost:5174
```

## 开发者文档

完整文档见 [`docs/developer/`](docs/developer/README.md)：

| 文档 | 内容 |
|------|------|
| [01-overview](docs/developer/01-overview.md) | 架构总览、技术栈、分层、调用链 |
| [02-quickstart](docs/developer/02-quickstart.md) | 环境搭建、bootstrap、端点、CLI |
| [03-configuration](docs/developer/03-configuration.md) | config.proto、`TORCHWOOD_` 环境变量映射 |
| [04-codegen](docs/developer/04-codegen.md) | Task / Buf / Wire |
| [05-authentication](docs/developer/05-authentication.md) | JWT / session / API Key / scopes |
| [06-databases](docs/developer/06-databases.md) | 动态文档、`_acl` + RLS 权限、查询 AST |
| [07-storage](docs/developer/07-storage.md) | S3/MinIO、分片上传、File Token |
| [08-functions](docs/developer/08-functions.md) | Docker 执行器、worker、生命周期 |
| [09-api-guide](docs/developer/09-api-guide.md) | 新增 gRPC 方法、错误映射、分页 |
| [10-console](docs/developer/10-console.md) | 前端结构、会话 Cookie |
| [11-testing](docs/developer/11-testing.md) | 测试分层、CI、lint、可观测性 |
| [12-sdk](docs/developer/12-sdk.md) | SDK 使用指南 |
| [13-operations](docs/developer/13-operations.md) | 部署、健康检查、备份 |
| [14-agent-tools](docs/developer/14-agent-tools.md) | Agent 工具箱（18 动词 overlay） |

另见 `AGENTS.md`（贡献约定）、`docs/roadmap.md`（AI/Agent-Native 战略）、`docs/tech-decision.md`。

## 许可证

MIT（待定）
