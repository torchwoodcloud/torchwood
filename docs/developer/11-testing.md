# Torchwood 测试与质量保障

> 说明测试分层、`internal/testutil` 集成库约定、CI 门禁与代码质量棘轮。
> 目标读者：所有提交代码的开发者。关联：`AGENTS.md`、`Taskfile.yml`、`.github/workflows/ci.yml`。
> 修订记录：2026-08-23 重写（以 `internal/testutil/db.go`、`Taskfile.yml:154` 的 `test` 任务、CI `buf breaking`/`golangci-lint --new-from-rev`/`wire` 漂移检查为准）。

---

## 1. 测试分层

| 层级 | 需真实 DB | 典型位置 | 示例 |
|------|-----------|----------|------|
| 纯单元 | 否（stub/mem） | `pkg/`、`internal/domain/`、`internal/api/*grpc/` | `pkg/crud/list_test.go`、`internal/api/servergrpc/projects_test.go`（`stubProjectRepo` + `contexts.WithPrincipal`） |
| 拦截器 | 部分需 DB | `internal/api/interceptor/` | `jwt_auth_test.go`、`policy_test_helper_test.go` |
| 集成 | 是（`SetupTestDB`） | `internal/infra/*`、`internal/app/*` | `internal/infra/documentdb/postgres_test.go` |
| 端到端 | 是 | `internal/runtime/` | `grpc_gateway_test.go`、`healthz_test.go`（readiness 503）、`authz_matrix_test.go`（全方法 × 凭证档过真实拦截器） |

通用约定：

- 集成测试首行 `if testing.Short() { t.Skip("skipping integration test") }`；
- 断言 `github.com/stretchr/testify/require`；
- gRPC 错误用 `status.Code(err)` + `codes.*`；
- API handler 测试用最小 stub 端口注入 Principal，不连 DB。

---

## 2. `task test` 是唯一入口

`Taskfile.yml:154` 定义：

```yaml
test:
  deps:
    - task: lint:go
    - task: test:sdk-go
    - task: test:sdk-ts
  cmds:
    - go test -race -v ./... -cover
```

| 子任务 | 位置 | 命令 |
|--------|------|------|
| `lint:go` | `Taskfile.yml:163` | `go vet ./...` + `gofmt -l .` 零差异 |
| `test:sdk-go` | `sdk/go` | `go test -v ./... -cover` |
| `test:sdk-ts` | `sdk/typescript` | `npm ci && npm run test`（见 `sdk/typescript/package.json:18`） |
| 主测 | 根 module | `go test -v ./... -cover`（含集成测试） |

- `dotenv: ['.env']` 使所有 task 自动加载根 `.env`，因此集成测试所需环境变量由 `task test` 注入；**直接 `go test ./...` 会 `t.Fatal`**（见 §3）；
- 手工等价：`export TORCHWOOD_TEST_DATABASE_SOURCE=... TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE=... && go test ./...`；
- 前置：`task docker:up` 启动本地三件套（`docker/local/docker-compose.yml`）。

覆盖率：`go test -cover` 输出各包语句覆盖率；CI 另以 `-race` 跑全量（见 §5）。

---

## 3. 集成测试数据库（`internal/testutil/`）

### 3.1 环境变量

| 变量 | 示例 | 说明 |
|------|------|------|
| `TORCHWOOD_TEST_DATABASE_SOURCE` | `postgres://torchwood:torchwood@127.0.0.1:5432/TORCHWOOD_test?sslmode=disable` | 测试 DSN 模板，**库名会被替换** |
| `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` | `postgres://torchwood:torchwood@127.0.0.1:5432/postgres?sslmode=disable` | 维护库 DSN（建库/删库） |

无硬编码回退，缺失时 `SetupTestDB` 直接 `t.Fatal` 提示 `run via task test`（`testutil/db.go:48`）。两个测试 DSN 保持 **owner 引导账号**（superuser）：testutil 的建隔离库 + 跑全量迁移是 §4.5 双账号契约的迁移侧（`CREATE EXTENSION vector`、public 建表、membership GRANT 都是引导面）；非 superuser 运行态形态由 `TestNonSuperuserAuthenticator_MigrateAndSmoke`（`testutil/nonsuperuser_test.go`，门禁 A2）以独立临时库端到端锁定——owner 跑迁移 + 建 authenticator，再以 authenticator 完成 roles_sig 同步、项目/业务库/集合创建与文档读写冒烟，并断言 `rolsuper=false`。

### 3.2 `SetupTestDB(t)` 生命周期（`testutil/db.go:43`）

```go
db := testutil.SetupTestDB(t)
defer db.Close()
```

1. 派生唯一库名：`<原库名>_<pid>_<seq>`，强制小写（`uniqueTestDBName:97`，PG 标识符小写折叠）；
2. `CREATE DATABASE <name>`（`adminDB`）；
3. `t.Cleanup` 注册：`pg_terminate_backend` 杀残留连接 → `DROP DATABASE IF EXISTS`；
4. 执行迁移：`db/migrations/*.up.sql` 按文件名排序逐条 `ExecContext`（`runMigrations:135`，无需 `golang-migrate`）；
5. 返回 `*clients.Database`（内嵌 `bun.DB`，`pgdriver.WithBufferSize(2<<20)` 避免大文档截断，见 `db.go:82`）；
6. 测试结束自动删库，无残留。

### 3.3 常用 Fixture

| 函数 | 说明 |
|------|------|
| `CreateTestProject(ctx,db)` | 插入项目 + `projectschema.Apply`，返回 `(projectID,internalID,cleanup)` |
| `CreateTestProjectThrough(ctx,db,maxVersion)` | 同上，部分迁移（`maxVersion<=0` 为全量） |
| `CreateTestAdmin` / `SignAdminToken` / `GrantAdminProject` | console admin 相关 |
| `CreateTestAPIKey` | 插入 API Key，返回 `(rawSecret,cleanup)` |
| `NewMemObjectStore` | 内存 `ObjectStore`（含 `Ping`） |
| `NewInterceptorEnv` | 组装 auth + audit 拦截器，`InvokeUnary` 跑完整鉴权链 |

### 3.4 示例

```go
func TestPostgresDocumentDatabase_CRUD(t *testing.T) {
  if testing.Short() { t.Skip("skipping integration test") }
  ctx := context.Background()
  db := testutil.SetupTestDB(t)
  defer db.Close()
  projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
  defer cleanup()
  docDB := NewPostgresDocumentDB(db, nil)
  // ... CRUD / 权限 / 分页断言
}
```

---

## 4. Lint 棘轮：`--new-from-rev` 0 → 全量 0

`Taskfile.yml:172` 与 CI `golangci-lint-action@v8` 均以 `origin/main` 为基线：

```bash
golangci-lint run --new-from-rev=origin/main ./...
```

- **含义**：仅报告相对 `origin/main` 新增的问题，存量遗留债不阻塞 PR；
- **目标**：棘轮式收敛——新代码 0 问题，存量逐步消化，最终过渡到全量 0（本地 `golangci-lint run ./...` 即全量检查，当前已无新增）；
- **CI 配置**：`ci.yml:88` 用 `args: --new-from-rev=origin/main`，`fetch-depth: 0` 保证基线可解析；
- **本地自检**：`task lint` 依次执行 `lint:go` + `lint:golangci` + `lint:sdk-go` + `lint:console`（`Taskfile.yml:188`）。

提交前：`task lint && task test`（或至少 `task lint` + `go test -short ./...`）。

---

## 5. CI 流水线（`.github/workflows/ci.yml`）

触发：`push` 到 `main` + 全部 `pull_request`；`concurrency.cancel-in-progress: true`。

### 5.1 `backend`（`ubuntu-latest`）

Services：`postgres:18-alpine`（`torchwood:torchwood`）与 `pgsty/silo:RELEASE.2026-09-03T13-18-01Z`（`minioadmin:minioadmin`），均带 healthcheck。

步骤（精简）：

1. `checkout@v4`（`fetch-depth: 0` 供 `--new-from-rev`）；
2. `setup-go`（`go-version-file: go.mod`）+ `setup-task` + `buf-setup-action@v1`（`v1.65.0`）；
3. `buf lint` → **`buf breaking --against '.git#branch=origin/main'`**（见 §6）；
4. 预拉 `node:18-alpine` / `python:3.11-alpine`（Functions 运行时基镜像）；
5. `test -z "$(gofmt -l .)"` → `mkdir -p console/dist && touch console/dist/index.html`（保证 `console/embed.go` 可编译）；
6. `go vet ./...` → `golangci-lint run --new-from-rev=origin/main`；
7. `go test -race ./...`（单元+集成）→ `sdk/go: go test -race ./...`；
8. **Codegen 漂移门禁**（`ci.yml:101`）：`buf generate` + `protoc config.proto` + `task wire:all` 后 `git diff --exit-code -- genproto internal/pkg/config cmd go.mod go.sum`，任何生成物漂移直接失败；
9. `pnpm@11.20.0` + `node@22` → `sdk/typescript: npm ci && npm run test` → `task sdk:demo-build` → `task build`（含 `console:build` 的 embed 链路验证）。

### 5.2 `frontend`（`working-directory: console`）

`pnpm install --frozen-lockfile` → `pnpm lint`（`eslint.config.js`）→ `pnpm test`（`vitest run`，`vite.config.ts:13` 含 `test.environment: jsdom`）→ `pnpm build`（`tsc -b && vite build`）。

---

## 6. 三类漂移门禁

| 门禁 | 命令 | 失败含义 |
|------|------|----------|
| Proto 兼容性 | `buf breaking --against '.git#branch=origin/main'`（`buf.yaml` 规则） | 删除/改类型字段未 `reserved`、改字段号等破坏性变更 |
| Lint 棘轮 | `golangci-lint run --new-from-rev=origin/main` | 相对基线新增 lint 问题 |
| 生成物一致性 | `buf generate` + `protoc` + `task wire:all` 后 `git diff --exit-code` | `genproto/`、`internal/pkg/config/*.pb.go`、`cmd/*/wire_gen.go` 未提交或手改生成物 |

本地复现：

```bash
buf breaking --against '.git#branch=origin/main'
golangci-lint run --new-from-rev=origin/main ./...
task generate:all && git diff --exit-code -- genproto internal/pkg/config cmd
```

---

## 7. 健康与可观测（测试相关）

- `internal/infra/health/checks.go`：`DependencyChecker`（`Name/Timeout/Check` 实现 `lynx.Checker`，`DefaultTimeout=2s`），`NewCheckers(db,rdb,obj)` 并行探测 postgres/redis/minio，`Details(ctx)` panic 兜底为 `unavailable`；
- 端点：`GET /v1/health`（别名 `/v1/server/health`，`ACCESS_PUBLIC`）返回 `{status,dependencies}`；`GET /healthz/readiness` 任一失败 503；`GET /v1/server/health/version` 返回构建注入的 `{version,commit,date}`；
- 慢查询：`internal/infra/clients/dbhook.go` 的 `SlowQueryHook`（`bun.QueryHook`），阈值 `data.database.slow_query_threshold`（默认 `500ms`，`"0"` 禁用，`debug=true` 全量 Debug），`TORCHWOOD_DATA_DATABASE_SLOW_QUERY_THRESHOLD` 覆盖。

---

## 8. 本地验证清单

```bash
task docker:up                  # Postgres/Redis/MinIO
task lint                # go vet + gofmt + golangci-lint(棘轮) + sdk vet + eslint
task test                # sdk-go + sdk-ts + go test -v -cover（自动加载 .env）
task build               # console:build + go build（ldflags 注入 version/commit/date）
# 手工：curl /v1/health  /v1/server/health/version  /healthz/readiness
```
