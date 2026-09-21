# 代码生成与工具链

本章面向后端开发者，说明仓库的四套生成机制（Buf proto 生成 → `genproto/`、config proto 生成 → `config.pb.go`、Wire 依赖注入 → `wire_gen.go`、授权矩阵文档生成 → `authz-matrix.md`）与 mise 任务编排，以及配套的漂移门禁。原则只有一条：**生成产物一律不手改，一切改动回到源头（proto / provider 声明 / 策略注册表）后重新生成。**

> 事实源：`mise.toml`、`buf.yaml`、`buf.gen.yaml`、`cmd/*/provides.go` → `wire_gen.go`、`cmd/server/internal/runtime/cmd/genauthzmatrix`。

---

## 1. mise 工具链与任务

`mise.toml` 三段约定：

- `[env] _.file = ".env"`：所有任务自动加载仓库根 `.env`（迁移与测试 DSN 依赖于此）；
- `[tools]` 钉住全部工具版本——go 1.26.5 / node 24.19.0 / pnpm 11.20.0 / buf 1.65.0 / protoc 31.1 / protoc-gen-go 1.36.11 / golangci-lint 2.12.2。CI 经 `jdx/mise-action` 从同一份 `mise.toml` 装工具，本地与 CI 完全同源；`mise install` 一次性装齐；
- `[task_config] shell = "sh -o errexit -c"`：任务统一用 sh 执行（Windows 需 Git Bash 的 sh 在 PATH）。

例外：Wire 不在 `[tools]` 中，各 `wire:*` 任务以 `go run -mod=mod github.com/google/wire/cmd/wire` 按主模块 `go.mod` 的钉版（v0.7.0）执行。

生成与门禁相关任务（`mise tasks` 列全量；下表"实际执行"照抄 `mise.toml`）：

| 任务 | 实际执行 | 用途 |
|------|----------|------|
| `mise run generate:proto` | `buf lint` → `buf generate` | proto → `genproto/`（§2） |
| `mise run generate:config` | 在 `internal/pkg/config` 内 `protoc -I. --go_out=. --go_opt=paths=source_relative ./config.proto` | 生成 `config.pb.go`（§3） |
| `mise run gen:authz-matrix` | `go run ./cmd/server/internal/runtime/cmd/genauthzmatrix` | 重新生成 `docs/developer/authz-matrix.md`（§5） |
| `mise run wire:server` / `wire:worker` / `wire:dispatcher` / `wire:packer` | 各自 `cmd/<name>` 目录内 `go mod tidy` + `go run -mod=mod github.com/google/wire/cmd/wire` | 重算各自 `wire_gen.go`（§4） |
| `mise run wire:all` | 上述四个 wire 任务依次执行 | 全量 Wire |
| `mise run generate:all` | generate:proto → generate:config → wire:all | 一键全量生成 |
| `mise run lint:proto` | `buf lint` + `buf breaking --against '.git#branch=origin/main'` | proto 兼容门禁 |
| `mise run lint:go` | `go vet ./...` + `gofmt -l .` 为空 | Go 静态与格式检查 |
| `mise run lint:golangci` | `golangci-lint run ./...` | 全量 lint 门禁 |
| `mise run test` | depends lint:go / lint:golangci / test:sdk-go / test:sdk-ts，再 `go test -race -v ./... -cover` | 本地全量门禁 |

常用组合：开工前 `mise run docker:up && mise run db:migrate`；改 proto / config / provider 后 `mise run generate:all && mise run build`；改 Console 后 `mise run console:build && mise run build`（Go embed 打包 `console/dist`，跳过第一步会打进旧版）。

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

- `modules.path: proto` 声明模块根；三个 BSR 依赖的确切 commit 由 `buf.lock` 锁定。
- lint 采用 `STANDARD` 规则集，5 项豁免各有明确理由（见 `buf.yaml` 内注释，改动前必读）：包名 `torchwood.*.v1` 有意与目录 `proto/{client,server,console,shared}/v1` 分离；复用 `shared.v1.Empty` / `ListRequest` 及消息直出响应（AIP-132 风格）；`ACCESS_*` 枚举值为 authz 语义命名。
- `breaking: FILE` 做文件级不兼容检测，对照 origin/main（禁止字段号复用、未 `reserved` 的删除等）。

### 2.2 buf.gen.yaml：四个 remote 插件

输出统一到 `genproto/`，全部 `paths=source_relative`：

| 插件 | 版本 | 产物 |
|------|------|------|
| `protocolbuffers/go` | v1.36.10 | `*.pb.go` |
| `grpc/go` | v1.6.0 | `*_grpc.pb.go` |
| `grpc-ecosystem/gateway` | v2.27.4 | `*.pb.gw.go` |
| `grpc-ecosystem/openapiv2` | v2.27.3 | `*.swagger.json` |

openapiv2 插件的两个选项各自有 CI 测试锁定（`cmd/server/internal/runtime/grpc_swagger_test.go`）：

- `json_names_for_fields=false`：OpenAPI 字段名使用 proto 声明的 snake_case（该 remote 插件默认为 true，必须显式置 false），与运行时 marshaler（`cmd/server/internal/runtime/errors.go` 的 `UseProtoNames: true`）一致。锁定测试：`TestSwaggerPropertyNamesAreSnakeCase`。
- `disable_default_errors=true`：关闭生成器自带的 rpcStatus 默认错误注入，统一错误响应（`shared.v1.ErrorResponse`）由各 proto 文件级 `openapiv2_swagger` 的 `responses.default` 声明。锁定测试：`TestSwaggerNoRpcStatus`。

`genproto/` 是独立 Go module（`genproto/go.mod`），主模块经 `replace github.com/torchwoodcloud/torchwood/genproto => ./genproto` 引用。**`genproto/` 禁手改**，一切修改回到 `proto/` 后重新 `buf generate`。

### 2.3 四组 proto

| 组 | 源目录 | 语义 | 输出 |
|----|--------|------|------|
| Client API | `proto/client/v1` | 终端用户直调（Account / Analytics / Assets / Databases / Functions / Groups / Leaderboards / Payments / RuntimeVars / Subscriptions，共 10 个 service） | `genproto/client/v1` |
| Server API | `proto/server/v1` | Agent / 自动化经 scoped API Key 调用（Analytics / APIKeys / Assets / AuditLogs / Auth / Billing / Databases / Functions / Groups / Health / Leaderboards / OAuthProviders / Outbox / Payments / Projects / Runbook / RuntimeVars / Storage / Subscriptions / Users，共 20 个 service） | `genproto/server/v1` |
| Console API | `proto/console/v1` | 管理后台（ConsoleAuth / Admins / Leaderboards 管控） | `genproto/console/v1` |
| Shared | `proto/shared/v1` | 跨组复用类型，7 个文件：`authz.proto`（`method_auth`/`service_auth` 注解与 AccessLevel/Scope 枚举）、`common.proto`（ListRequest/ListResponseMeta/Empty）、`error.proto`（ErrorCode/Error/ErrorResponse）、`document.proto`（Document/ArrayUpdate/Change）、`entities.proto`（TokenBundle/Session/Group/Subscription 等）、`leaderboard.proto`（排行榜实体）、`query.proto`（Query/Filter/VectorSearch 等动态查询形状） | `genproto/shared/v1` |

---

## 3. config proto 生成

`generate:config` 在 `internal/pkg/config` 目录内执行 `protoc`（mise 钉版 protoc 31.1 + protoc-gen-go 1.36.11，与 `config.pb.go` 头注释一致）：

```bash
protoc -I. --go_out=. --go_opt=paths=source_relative ./config.proto
```

产出 `internal/pkg/config/config.pb.go`（仅 message 与 getter，`config.proto` 无 service）。它定义 `AppConfig` 的类型形状，供 `bind.go` 的 `UnmarshalConfig` 把 lynx 配置（YAML + `TORCHWOOD_*` 环境变量，点号路径映射）解码进结构体。改了 `config.proto` 但忘了重新生成，新配置键将无法被环境变量覆盖（目标结构体上没有该字段）。

---

## 4. Wire 依赖注入

四个服务二进制的装配结构完全同构：

| 文件 | 角色 |
|------|------|
| `cmd/<name>/provides.go` | 手写 `ProviderSet`（各层 ProviderSet + 组合根专属 provider），头部 `//go:generate wire` |
| `cmd/<name>/wire.go` | `wireinject` 构建标签下的 `wireBootstrap`，函数体 `panic(wire.Build(ProviderSet))` |
| `cmd/<name>/wire_gen.go` | Wire 生成的真实装配代码（禁手改） |

覆盖范围：`cmd/server`（boot / api / app / infra / domain / runtime 各层 + NewAppConfig / NewComponents / NewBuildInfo 等）、`cmd/worker`、`cmd/dispatcher`、`cmd/packer`。`cmd/torchwood`（CLI）无 Wire，装配在 `cli/` 包手工完成。worker 的 `provides.go` 另行校验 `data.database.source` 必填，缺失拒绝启动。

**改动 provider（新增 / 删除 / 改签名）后必须 `mise run wire:all`**，否则 `wire_gen.go` 与 provider 声明失步，编译或启动失败。注意各 wire 任务先跑 `go mod tidy`——依赖变更时 `go.mod` / `go.sum` 也会更新，因此它们同属生成面（见 §5 门禁 2 的漂移范围）。

---

## 5. 生成顺序与漂移门禁

```
generate:all
 ├─ generate:proto      # proto/ → genproto/
 ├─ generate:config     # config.proto → config.pb.go
 └─ wire:all            # server / worker / dispatcher / packer 四份 wire_gen.go（+ go.mod/go.sum）

gen:authz-matrix        # 独立于 generate:all：策略注册表 → docs/developer/authz-matrix.md
```

按改动对象选择任务：

| 改了什么 | 跑什么 |
|----------|--------|
| `proto/**/*.proto` | `mise run generate:proto` |
| proto 中的 `method_auth` / `service_auth` 策略声明 | 上行 + `mise run gen:authz-matrix` |
| `internal/pkg/config/config.proto` | `mise run generate:config` |
| 任意 `provides.go` / provider 签名 | `mise run wire:all` |
| 首次拉取 / 全量验证 | `mise run generate:all && mise run build` |

四道漂移与质量门禁（本地提交前与 CI 一致）：

1. **proto 兼容**：`mise run lint:proto`（buf lint + `buf breaking --against '.git#branch=origin/main'`）。
2. **生成物零漂移**：CI 步骤执行 `mise run generate:all` 后 `git diff --exit-code -- genproto internal/pkg/config cmd go.mod go.sum`——任何生成物与提交不一致即失败。同一 commit 下重复生成应零 diff；本地等价操作是 `generate:all` 后 `git status` 应干净。
3. **授权矩阵零漂移**：`TestAuthzMatrixDoc_NoDrift`（`cmd/server/internal/runtime/authz_matrix_doc_test.go`）把磁盘上的 `docs/developer/authz-matrix.md` 与从真实 proto 策略注解即时渲染的结果逐字节比对，任何 `go test ./...`（含 CI）即触发，漂移即红。处理后：`mise run gen:authz-matrix` 重新生成并随策略变更一起提交。数据源与启动期同源（`ProvideMethodPolicies` → `BuildMethodPolicies`），文档永远不会比拦截器"更旧"。
4. **lint 全量门禁**：`golangci-lint run ./...`（golangci-lint v2 配置：standard 五件套 + bodyclose / gosec / noctx / sqlclosecheck）。无 `--new-from-rev` 棘轮；存量暂挂以 `.golangci.yml` 的 exclusions 逐文件精确圈定，禁止追加新路径。`go vet` + `gofmt`（`lint:go`）为其前置。

### 新增 gRPC 方法的生成侧清单

1. 在 proto 方法上声明 `(method_auth)` 注解（或依赖服务级 `service_auth` 默认）——策略唯一声明源在 proto（扩展号 52001/52002），缺失时启动期 `BuildMethodPolicies` 报 `missing auth policy` 直接启动失败；
2. 同步 OpenAPI 扩展 `x-torchwood-access`（`public` / `end_user` / `server` / `permission` / `system`；`permission` 档必须逐方法声明 operation 级扩展，否则继承顶层被一致性测试拦下），与 `method_auth` 的一致性由 `grpc_swagger_test.go` 断言；
3. 在 `internal/app/shared/authz.go` 按语义选用例层纵深防御（`RequireServerPrincipal` / `RequirePlatformPrincipal` / `RequireEndUser` / `RequireConsolePrincipal` / `RequireAnyOf`）；
4. `mise run generate:proto && mise run gen:authz-matrix`，授权矩阵文档随策略变更同一 PR 提交；
5. `mise run generate:all && mise run build && go vet ./...` 验证零漂移。

完整流程与鉴权细节见 `09-api-guide.md` 与 `05-authentication.md`。

---

## 6. 常见问题

| 现象 | 原因与处理 |
|------|-----------|
| 改了 proto 后 `go build` 报方法缺失 | 忘跑 `buf generate`，`genproto/` 还是旧代码。先 `mise run generate:proto` 再 build |
| 改了策略注解后 `go test` 报授权矩阵漂移 | `docs/developer/authz-matrix.md` 未重生成。`mise run gen:authz-matrix` 后随变更一并提交 |
| Wire 编译报类型不匹配 | 改了 provider 签名未重算。`mise run wire:all` |
| 新配置键环境变量覆盖不生效 | 改了 `config.proto` 未 `generate:config`，`AppConfig` 上没有该字段 |
| CI 漂移检查失败 | 生成物未提交。本地 `mise run generate:all` 后将 `genproto/`、`internal/pkg/config/config.pb.go`、四份 `wire_gen.go`、`go.mod`/`go.sum` 一并提交 |

---

## 相关文档

- `09-api-guide.md` — 新增 RPC 的完整流程（含 authz 与 OpenAPI 一致性断言）
- `05-authentication.md` — 策略注册表与档位语义
- `authz-matrix.md` — 本章 §5 生成的授权矩阵（全量 RPC × 鉴权档位对照表）
- `03-configuration.md` — config.proto 的运行时绑定
- `AGENTS.md` — 生成约定总纲
