# Torchwood 代码生成与工具链

> Task、Buf、Wire 三件套：`Taskfile.yml`、`buf.yaml`/`buf.gen.yaml`、`cmd/*/provides.go→wire_gen.go`。以代码为准。
> 最新更新：2026-08-23

---

## 1. Task 工作流（Taskfile.yml）

`Taskfile.yml:2` `dotenv: ['.env']` 自动加载 `.env`（测试/迁移 DSN 依赖）。

| 任务 | 命令 | 用途 |
|------|------|------|
| `tools:install` | `go install protoc-gen-go / migrate / buf@v1.65.0 / wire / golangci-lint@v2.12.2` | 首次安装工具链 |
| `generate:proto` | `buf lint` + `buf generate` | 生成 gRPC/gateway/Swagger（§2） |
| `generate:config` | `protoc -I. --go_out=. --go_opt=paths=source_relative ./config.proto`（`internal/pkg/config` 内执行） | 生成 `config.pb.go` |
| `wire:server` / `wire:worker` | `go mod tidy && go run -mod=mod github.com/google/wire/cmd/wire` | 各自重算 `wire_gen.go` |
| `wire:all` | `wire:server` + `wire:worker` | 全量 Wire |
| `generate:all` | `generate:proto` → `generate:config` → `wire:all` | 一键全量（§5） |
| `lint:proto` | `buf lint` + `buf breaking --against '.git#branch=origin/main'` | proto 兼容门禁 |
| `lint:golangci` | `golangci-lint run --new-from-rev=origin/main ./...` | 增量棘轮（全量 `golangci-lint run ./...` 0 warning） |
| `lint:go` | `go vet ./...` + `gofmt -l .` | Go 静态/格式检查 |
| `up`/`down`/`clean` | `docker compose`（`docker/local`） | 基础设施 |
| `db:migrate` | `migrate -path ./db/migrations -database <DSN> up`（`TORCHWOOD_DATA_DATABASE_SOURCE` 优先） | 迁移 |
| `build` | `console:build` + `go build -ldflags version/commit/date` 三二进制 | 产出 `bin/server` `bin/worker` `bin/torchwood` |
| `test` | `lint:go` + `test:sdk-go` + `test:sdk-ts` + `go test -v ./... -cover` | 全量测试 |

常用组合：改动前 `task docker:up && task db:migrate`；改动 proto/config/provider 后 `task generate:all && task build`；改 Console 后 `task console:build && task build`（`embed dist`）。

---

## 2. Buf 驱动的 proto 生成

### 2.1 buf.yaml（v2）

```yaml
version: v2
deps: [buf.build/googleapis/googleapis, buf.build/bufbuild/protovalidate, buf.build/grpc-ecosystem/grpc-gateway]
modules: [{path: proto}]
lint:   {use: [STANDARD], except: [PACKAGE_DIRECTORY_MATCH, RPC_REQUEST_RESPONSE_UNIQUE, RPC_REQUEST_STANDARD_NAME, RPC_RESPONSE_STANDARD_NAME, ENUM_VALUE_PREFIX]}
breaking: {use: [FILE]}
```

- `modules.path: proto` 声明模块根；
- `STANDARD` lint + 5 项豁免（包名与目录有意分离、复用 `shared.v1.Empty/ListRequest`、AIP-132 复用、`ACCESS_*` 语义命名）；
- `breaking: FILE` 文件级不兼容检测。

### 2.2 buf.gen.yaml（4 插件 → `genproto/`，`paths=source_relative`）

| 插件 | 版本 | 产物 |
|------|------|------|
| `protocolbuffers/go` | v1.36.10 | `*.pb.go` |
| `grpc/go` | v1.6.0 | `*_grpc.pb.go` |
| `grpc-ecosystem/gateway` | v2.27.4 | `*.pb.gw.go` |
| `grpc-ecosystem/openapiv2` | v2.27.3 | `*.swagger.json`（`json_names_for_fields=false` 输出 snake_case；`disable_default_errors=true` 关闭 rpcStatus 注入，default 错误响应由各 service proto 文件级声明） |

### 2.3 proto → genproto 四组

| 组 | 源目录 | 语义 | 输出 |
|----|--------|------|------|
| Client API | `proto/client/v1` | 终端用户直调（Account/Databases/Groups/Payments/Assets/Subscriptions） | `genproto/client/v1` |
| Server API | `proto/server/v1` | Agent/自动化经 scoped API Key（Projects/Users/Storage/Databases/Functions/APIKeys/Groups/Health/OAuthProviders/Payments/Assets/Subscriptions/Billing/Outbox） | `genproto/server/v1` |
| Console API | `proto/console/v1` | 管理后台（ConsoleAuth/Admins） | `genproto/console/v1` |
| Shared | `proto/shared/v1` | `authz.proto`（鉴权注解）、`common.proto`（分页元数据） | `genproto/shared/v1` |

**禁手改 `genproto/`**：一切改动回 `proto/` 后重 `buf generate`。

---

## 3. generate:config

`Taskfile.yml:14` 在 `internal/pkg/config` 内执行：

```bash
protoc -I. --go_out=. --go_opt=paths=source_relative ./config.proto
```

产出 `internal/pkg/config/config.pb.go`（仅 message/getter，供 `bind.go` 反射与 `NewAppConfig` 校验）。

---

## 4. Wire 依赖注入

| 文件 | 角色 |
|------|------|
| `cmd/server/provides.go:30` | 手写 `ProviderSet = wire.NewSet(boot.New, api.ProviderSet, app.ProviderSet, infra.ProviderSet, domain.ProviderSet, NewLogger, NewComponents, ...)` + `NewAppConfig`/`NewBuildInfo` 等 |
| `cmd/server/wire.go` | `//go:generate wire` + `wire.Build(ProviderSet)` |
| `cmd/server/wire_gen.go` | 生成装配代码（禁手改） |

`cmd/worker/` 同构（`provides.go:22`）：校验 `data.database.source`，装配 `Worker`/`OutboxWorker` 等。

**变更后必 `task wire:all`**（`AGENTS.md` 约定），否则 `wire_gen.go` 与 `provides.go` 失步启动失败。

---

## 5. 生成顺序与漂移门禁

```
generate:all
 ├─ generate:proto      # proto/ → genproto/
 ├─ generate:config     # config.proto → config.pb.go
 └─ wire:all
      ├─ wire:server
      └─ wire:worker
```

| 场景 | 命令 |
|------|------|
| 改 `proto/**/*.proto` | `task generate:proto` |
| 改 `config.proto` | `task generate:config` |
| 改 `provides.go` / provider 签名 | `task wire:all` |
| 全量/首次拉取 | `task generate:all && task build` |

**漂移门禁（CI 与本地一致）**

- **proto 兼容**：`task lint:proto` → `buf breaking --against '.git#branch=origin/main'`（`Taskfile.yml:29`），禁字段号复用、删除未 `reserved`、破坏性变更；`buf lint` 的 5 项 `except` 已在 `buf.yaml:19` 注释理由，改前必读；
- **codegen 零漂移**：`task generate:all && git diff --exit-code`（CI `lint` job），本地验证同样执行；任何 `genproto/`、`config.pb.go`、`wire_gen.go` 未提交即失败；
- **lint 棘轮**：`golangci-lint run --new-from-rev=origin/main` 仅拦新增（`Taskfile.yml:172`），全量 `golangci-lint run ./...` 零告警后渐进烧存量债（`docs/review/arch-review-2026-08-fix-plan.md:317`）；`go vet` + `gofmt` 为前置门禁。

**新增 gRPC 方法清单**（fail-closed，`05-authentication.md §5`）：

1. 在 `proto/*/v1/*.proto` 为方法加 `(method_auth)`（或依赖 `service_auth` 默认），必填否则 `collectMethodsByAccess` 启动 `missing auth policy`；
2. 若 `ACCESS_API_KEY`，在 `internal/api/interceptor/apikey_scope.go:25` 登记 `{resource, op}`，否则 `AssertAPIKeyScopeCoverage` panic；
3. 若 `op==write`，在 `admin_roles.go:16` 登记允许角色，否则 `AssertAdminRoleWriteCoverage` panic；
4. 在对应 `app/shared/authz.go` 选择 `RequireServerPrincipal`（业务写，API Key 可做）或 `RequirePlatformPrincipal`（平台级）做纵深防御；
5. 运行 `task generate:all && task build && go vet ./...` 验证零漂移。

`proto/shared/v1/authz.proto:18` 的 `AccessLevel` 与 `method_auth` 为鉴权唯一事实源，OpenAPI `x-torchwood-access` 扩展一致性由 `internal/runtime/grpc_swagger_test.go` 断言。

> 生成产物一律可重放：同一 commit 下重复 `task generate:all` 应零 diff；CI 以此为门禁，本地提交前必跑。

---

## 6. 常见问题

- **改了 proto 但未生成**：`buf generate` 未跑导致 `genproto/` 旧代码，`go build` 会报方法缺失；先 `task generate:proto` 再 `task build`。
- **Wire 失步**：改 `provides.go` 签名后未 `wire:all`，编译报 `wire_gen.go` 类型不匹配；按 §5 重算。
- **config 改后未生效**：改 `config.proto` 后漏 `generate:config`，`bind.go` 反射不到新键，环境变量覆盖失效。
- **CI 漂移失败**：本地 `git diff --exit-code` 有未提交生成物，先 `task generate:all` 并提交全部改动。

---

## 7. 参考

- `Taskfile.yml` 任务全表
- `buf.yaml` / `buf.gen.yaml` Buf 版本与依赖
- `proto/client|server|console|shared` 唯一事实来源
- `genproto/` 生成产物（禁手改）
- `cmd/server|worker/provides.go` / `wire.go` / `wire_gen.go`
- `internal/pkg/config/config.proto`
- `AGENTS.md` 生成约定
- `internal/runtime/grpc_swagger_test.go` swagger/`method_auth` 一致性断言
