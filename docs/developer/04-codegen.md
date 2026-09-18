# 代码生成与工具链

本章面向后端开发者，说明仓库的三套代码生成机制（Buf proto 生成、config proto 生成、Wire 依赖注入）与 mise 任务编排，以及配套的漂移门禁。原则只有一条：**生成产物一律不手改，一切改动回到源头（proto / provider 声明）后重新生成。**

> 事实源：`mise.toml`、`buf.yaml`、`buf.gen.yaml`、`cmd/*/provides.go` → `wire_gen.go`。

---

## 1. mise 工作流

`mise.toml` 的 `[env]` 声明 `_.file = ".env"`，所有任务自动加载仓库根 `.env`（迁移与测试 DSN 依赖于此）；`[tools]` 钉住全部工具版本（与 CI 同源），`[task_config].shell` 统一任务 shell。常用任务（`mise tasks` 可随时列出全量）：

| 任务 | 命令内容 | 用途 |
|------|----------|------|
| `mise install` | 按 `[tools]` 安装 go / node / pnpm / buf / protoc / protoc-gen-go / golangci-lint | 首次安装工具链 |
| `mise run generate:proto` | `buf lint` + `buf generate` | proto → `genproto/`（§2） |
| `mise run generate:config` | 在 `internal/pkg/config` 内执行 protoc | 生成 `config.pb.go`（§3） |
| `mise run wire:server` / `wire:worker` / `wire:dispatcher` / `wire:packer` | `go mod tidy` + wire | 重算各自的 `wire_gen.go`（§4） |
| `mise run wire:all` | 上述四个 wire 任务依次执行 | 全量 Wire |
| `mise run generate:all` | generate:proto → generate:config → wire:all | 一键全量生成（§5） |
| `mise run lint:proto` | `buf lint` + `buf breaking --against '.git#branch=origin/main'` | proto 兼容门禁 |
| `mise run lint:go` | `go vet ./...` + `gofmt -l .` | Go 静态与格式检查 |
| `mise run lint:golangci` | `golangci-lint run ./...` | 全量 lint 门禁 |
| `mise run db:migrate` | `go run -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate ... up`（版本走 go.mod 钉版） | 数据库迁移（DSN 优先 `MIGRATE_DSN`，其次 `TORCHWOOD_DATA_DATABASE_SOURCE`） |
| `mise run build` | console:build + `go build` 五个二进制（带 version/commit/date ldflags） | 产出 `bin/server`、`bin/worker`、`bin/dispatcher`、`bin/packer`、`bin/torchwood` |
| `mise run test` | lint:go + lint:golangci + test:sdk-go + test:sdk-ts + `go test -race -v ./... -cover` | 全量测试 |

常用组合：开工前 `mise run docker:up && mise run db:migrate`；改 proto / config / provider 后 `mise run generate:all && mise run build`；改 Console 后 `mise run console:build && mise run build`。

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

- `modules.path: proto` 声明模块根。
- lint 采用 `STANDARD` 规则集，5 项豁免各有明确理由（见 `buf.yaml` 内注释，改动前必读）：包名与目录有意分离；复用 `shared.v1.Empty` / `ListRequest`（AIP-132 风格）；`ACCESS_*` 枚举值为 authz 语义命名。
- `breaking: FILE` 做文件级不兼容检测，对照 origin/main。

### 2.2 buf.gen.yaml：四个插件

输出统一到 `genproto/`，`paths=source_relative`：

| 插件 | 版本 | 产物 |
|------|------|------|
| `protocolbuffers/go` | v1.36.10 | `*.pb.go` |
| `grpc/go` | v1.6.0 | `*_grpc.pb.go` |
| `grpc-ecosystem/gateway` | v2.27.4 | `*.pb.gw.go` |
| `grpc-ecosystem/openapiv2` | v2.27.3 | `*.swagger.json` |

openapiv2 插件有两个关键选项：

- `json_names_for_fields=false`：OpenAPI 字段名使用 proto 声明的 snake_case，与运行时 marshaler（`errors.go` 的 `UseProtoNames:true`）保持一致；
- `disable_default_errors=true`：关闭生成器自带的 rpcStatus 默认错误注入，统一错误响应（`shared.v1.ErrorResponse`）由各 proto 文件级 `openapiv2_swagger` 的 `responses.default` 声明（见 `09-api-guide.md`）。

### 2.3 四组 proto

| 组 | 源目录 | 语义 | 输出 |
|----|--------|------|------|
| Client API | `proto/client/v1` | 终端用户直调（Account / Databases / Groups / Payments / Assets / Subscriptions） | `genproto/client/v1` |
| Server API | `proto/server/v1` | Agent / 自动化经 scoped API Key 调用（Projects / Users / Storage / Databases / Functions / APIKeys / Groups / Health / OAuthProviders / Payments / Assets / Subscriptions / Billing / Outbox / Analytics / Leaderboards / AuditLogs / Runbook） | `genproto/server/v1` |
| Console API | `proto/console/v1` | 管理后台（ConsoleAuth / Admins / Leaderboards 管控） | `genproto/console/v1` |
| Shared | `proto/shared/v1` | `authz.proto`（鉴权注解）、`common.proto`（分页元数据与统一错误响应） | `genproto/shared/v1` |

**`genproto/` 禁手改**。一切修改回到 `proto/` 后重新 `buf generate`。

---

## 3. config proto 生成

`generate:config` 在 `internal/pkg/config` 目录内执行：

```bash
protoc -I. --go_out=. --go_opt=paths=source_relative ./config.proto
```

产出 `internal/pkg/config/config.pb.go`（仅 message 与 getter），供 `bind.go` 反射遍历配置键与 `NewAppConfig` 启动校验使用。改了 `config.proto` 但忘了重新生成，新配置键将无法被环境变量覆盖（`bind.go` 反射不到）。

---

## 4. Wire 依赖注入

| 文件 | 角色 |
|------|------|
| `cmd/server/provides.go` | 手写 `ProviderSet`（boot / api / app / infra / domain 各层 ProviderSet + NewLogger / NewComponents / NewAppConfig / NewBuildInfo 等） |
| `cmd/server/wire.go` | `//go:generate wire` + `wire.Build(ProviderSet)` |
| `cmd/server/wire_gen.go` | Wire 生成装配代码（禁手改） |

`cmd/worker/` 与 `cmd/dispatcher/` 同构。worker 的 `provides.go` 另行校验 `data.database.source` 必填。

**改动 provider（新增 / 删除 / 改签名）后必须 `mise run wire:all`**，否则 `wire_gen.go` 与 provider 声明失步，编译或启动失败。

---

## 5. 生成顺序与漂移门禁

```
generate:all
 ├─ generate:proto      # proto/ → genproto/
 ├─ generate:config     # config.proto → config.pb.go
 └─ wire:all            # server / worker / dispatcher 三份 wire_gen.go
```

按改动对象选择任务：

| 改了什么 | 跑什么 |
|----------|--------|
| `proto/**/*.proto` | `mise run generate:proto` |
| `internal/pkg/config/config.proto` | `mise run generate:config` |
| 任意 `provides.go` / provider 签名 | `mise run wire:all` |
| 首次拉取 / 全量验证 | `mise run generate:all && mise run build` |

三道门禁（本地提交前与 CI 一致）：

1. **proto 兼容**：`mise run lint:proto` 执行 `buf breaking --against '.git#branch=origin/main'`，禁止字段号复用、未 `reserved` 的删除等破坏性变更。
2. **codegen 零漂移**：`mise run generate:all && git diff --exit-code`——任何生成物（`genproto/`、`config.pb.go`、`wire_gen.go`）与提交不一致即失败。同一 commit 下重复生成应零 diff。
3. **lint 全量门禁**：`golangci-lint run ./...`（无棘轮豁免），`go vet` + `gofmt` 为前置。

### 新增 gRPC 方法的生成侧清单

1. 在 proto 方法上声明 `(method_auth)` 注解（或依赖服务级 `service_auth` 默认）——策略唯一声明源在 proto，缺失时启动报 `missing auth policy`；
2. 同步 OpenAPI 扩展 `x-torchwood-access`（`public` / `end_user` / `server` / `permission`），一致性由 `cmd/server/internal/runtime/grpc_swagger_test.go` 断言；
3. 在 `internal/app/shared/authz.go` 按语义选择 `RequireServerPrincipal`（业务写，API Key 可调用）或 `RequirePlatformPrincipal`（平台级）做纵深防御；
4. `mise run generate:all && mise run build && go vet ./...` 验证零漂移。

完整流程与鉴权细节见 `09-api-guide.md` 与 `05-authentication.md`。

---

## 6. 常见问题

| 现象 | 原因与处理 |
|------|-----------|
| 改了 proto 后 `go build` 报方法缺失 | 忘跑 `buf generate`，`genproto/` 还是旧代码。先 `mise run generate:proto` 再 build |
| Wire 编译报类型不匹配 | 改了 provider 签名未重算。`mise run wire:all` |
| 新配置键环境变量覆盖不生效 | 改了 `config.proto` 未 `generate:config`，`bind.go` 反射不到新键 |
| CI 漂移检查失败 | 生成物未提交。本地 `mise run generate:all` 后将全部生成物一并提交 |

---

## 相关文档

- `09-api-guide.md` — 新增 RPC 的完整流程（含 authz 与 OpenAPI）
- `05-authentication.md` — 策略注册表与档位语义
- `03-configuration.md` — config.proto 的运行时绑定
- `AGENTS.md` — 生成约定总纲
