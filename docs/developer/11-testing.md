# 11 测试与质量保障

说明测试分层、`internal/pkg/testutil` 集成测试库约定、CI 门禁与 lint 全量门禁。所有提交代码的开发者必读。

> 关联：`AGENTS.md`、`mise.toml`、`.github/workflows/ci.yml`。

## 1. 测试分层

| 层级 | 需真实 DB | 典型位置 | 示例 |
|------|-----------|----------|------|
| 纯单元 | 否（stub / 内存实现） | `pkg/`、`internal/domain/`、`internal/api/*grpc/` | `pkg/crud/list_test.go`、`internal/api/servergrpc/projects_test.go`（stub repo + `contexts.WithPrincipal`） |
| 拦截器 | 部分需 DB | `internal/api/interceptor/` | `jwt_auth_test.go`、`policy_test_helper_test.go` |
| 集成 | 是（`SetupTestDB`） | `internal/infra/*`、`internal/app/*` | `internal/infra/documentdb/postgres_test.go` |
| 端到端 | 是 | `cmd/server/internal/runtime/` | `grpc_gateway_test.go`、`healthz_test.go`（readiness 503）、authz matrix 全方法 × 凭证档过真实拦截器 |

通用约定：

- 集成测试首行 `if testing.Short() { t.Skip("skipping integration test") }`；
- 断言用 `github.com/stretchr/testify/require`；
- gRPC 错误用 `status.Code(err)` + `codes.*`；
- API handler 测试用最小 stub 端口注入 Principal，不连 DB。

## 2. `mise run test` 是唯一入口

```toml
[tasks.test]
depends = ["lint:go", "lint:golangci", "test:sdk-go", "test:sdk-ts"]
run = "go test -race -v ./... -cover"
```

| 子任务 | 命令 |
|--------|------|
| `lint:go` | `go vet ./...` + `gofmt -l .` 零差异 |
| `lint:golangci` | `golangci-lint run ./...`（全量门禁） |
| `test:sdk-go` | sdk/go 内 `go test -v ./... -cover` |
| `test:sdk-ts` | sdk/typescript 内 `npm ci && npm run test` |
| 主测 | 根 module `go test -race -v ./... -cover`（含集成测试） |

- `mise.toml` 的 `[env]` 声明 `_.file = ".env"`，所有任务自动加载根 `.env`，集成测试所需环境变量由 `mise run test` 注入；**直接 `go test ./...` 会 `t.Fatal`**（缺 DSN，见 §3）。
- 手工等价：导出 `TORCHWOOD_TEST_DATABASE_SOURCE` 与 `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` 后再 `go test ./...`。
- 前置：`mise run docker:up` 启动本地三件套。

## 3. 集成测试数据库（internal/pkg/testutil）

### 3.1 环境变量

| 变量 | 示例 | 说明 |
|------|------|------|
| `TORCHWOOD_TEST_DATABASE_SOURCE` | `postgres://torchwood:torchwood@127.0.0.1:5432/TORCHWOOD_test?sslmode=disable` | 测试 DSN 模板，**库名会被替换** |
| `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` | `postgres://torchwood:torchwood@127.0.0.1:5432/postgres?sslmode=disable` | 维护库 DSN（建库 / 删库） |

无硬编码回退，缺失时 `SetupTestDB` 直接 `t.Fatal` 提示 `mise run test`（自动加载 `.env`）。

两个测试 DSN 保持 **owner 引导账号**（superuser）：testutil 的建隔离库 + 跑全量迁移是双账号契约的迁移侧（`CREATE EXTENSION vector`、public 建表、membership GRANT 都是引导面）。非 superuser 运行态形态由独立测试端到端锁定——owner 跑迁移 + 建 authenticator，再以 authenticator 完成 roles_sig 同步、项目 / 业务库 / 集合创建与文档读写冒烟，并断言 `rolsuper=false`。

### 3.2 SetupTestDB 生命周期

```go
db := testutil.SetupTestDB(t)
defer db.Close()
```

1. 派生唯一库名：`<原库名>_<pid>_<seq>`，强制小写（PG 标识符小写折叠）；
2. 经 admin 连接 `CREATE DATABASE`；
3. `t.Cleanup` 注册清理：`pg_terminate_backend` 杀残留连接 → `DROP DATABASE IF EXISTS`；
4. 执行迁移：`db/migrations/*.up.sql` 按文件名排序逐条执行（无需 golang-migrate）；
5. 返回 `*clients.Database`（内嵌 bun.DB，大缓冲避免大文档截断）；
6. 测试结束自动删库，无残留。

**并行安全契约**：隔离库建库 / 迁移 / 删库段持集群级 advisory lock 跨进程互斥（`go test -p N` 每包一独立进程，进程内锁无效）；admin 连接带读超时 + 瞬时过载退避重试；迁移 000004 的三角色为集群级共享对象——up 原子幂等创建、down 仅清理本库不 DROP ROLE。契约全文见 `testutil/db.go` 包注释。

### 3.3 常用 Fixture

| 函数 | 说明 |
|------|------|
| `CreateTestProject(ctx,db)` | 插入项目 + `projectschema.Apply`，返回 `(projectID, internalID, cleanup)` |
| `CreateTestProjectThrough(ctx,db,maxVersion)` | 同上，跑部分迁移（`maxVersion<=0` 为全量） |
| `CreateTestAdmin` / `SignAdminToken` / `GrantAdminProject` | console admin 相关 |
| `CreateTestAPIKey` | 插入 API Key，返回 `(rawSecret, cleanup)` |
| `NewMemObjectStore` | 内存 `ObjectStore`（含 Ping） |
| `NewInterceptorEnv` | 组装 auth + audit 拦截器，`InvokeUnary` 跑完整鉴权链 |
| `Eventually` | 轮询等待异步条件（事件分发 / worker 消费断言） |
| `FakeRateLimiter` | 限流器假实现（拦截器单测） |
| `SeedSystemDocumentCollections` | 播种系统 sentinel 集合 |
| `NewInterceptorEnvWithExecutionTokens` / `AuditLogCount` / `LatestAuditLog` | 执行身份 token 与审计行断言辅助 |

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

## 4. Lint 全量门禁

存量 lint 债已清零，本地与 CI 均为**全量门禁**（`--new-from-rev` 棘轮已退役）：

```bash
golangci-lint run ./...
```

- 启用 `bodyclose` / `gosec` / `noctx` / `sqlclosecheck` 等；仅剩行内圈定的 exclusions 暂挂项（`.golangci.yml` 头注释）。
- `mise run test` 的 deps 已含 `lint:golangci`——本地裸跑 `go test` 全绿但 lint 红仍会翻车。
- 本地自检：`mise run lint` 依次执行 `lint:go` + `lint:golangci` + `lint:sdk-go` + `lint:console`。

提交前：`mise run lint && mise run test`（或至少 `mise run lint` + `go test -short ./...`）。

## 5. CI 流水线

`.github/workflows/ci.yml`：`push` 到 `main` + 全部 `pull_request` 触发；并发取消旧跑。

### 5.1 backend job（ubuntu-latest）

Services：`percona/percona-distribution-postgresql:18`（发行版基座内置 pgvector 0.8.3；`--locale=C --encoding=UTF8`——locale=C 默认 SQL_ASCII，pgdriver 握手拒连）、`pgsty/silo`（MinIO）、`redis:7-alpine`，均带 healthcheck。env 含 `TORCHWOOD_TEST_REDIS_ADDR`、`TORCHWOOD_TEST_MINIO_ENDPOINT`、`TORCHWOOD_RUN_DOCKER_TESTS=1`。

步骤（精简）：

1. checkout + `jdx/mise-action`（按 `mise.toml` 装齐 go / node / pnpm / buf / protoc / protoc-gen-go / golangci-lint）；
2. `buf lint` → **`buf breaking --against '.git#branch=origin/main'`**；
3. 预拉 `node:18-alpine`（Functions 运行时基镜像）；
4. `mise run lint:go` / `mise run lint:golangci` 全量门禁；
5. `go test -race -covermode=atomic ./...`（单元 + 集成）→ sdk/go 测试；
6. **RLS 相对基准门禁**：RLS 查询耗时相对基准 30x 阈值；
7. **Coverage gate**：总覆盖率 ≥48%，只升不降；
8. **functions docker e2e 断言**：输出无 `--- SKIP`（防静默跳过）；
9. **Codegen 漂移门禁**：`mise run generate:all`（protoc 31.1 + protoc-gen-go 1.36.11 由 mise 钉版）后 `git diff --exit-code`；
10. sdk/typescript 测试 → `mise run sdk:demo-build` → `mise run build`（含 console embed 链路验证）。

### 5.2 frontend job

`pnpm install --frozen-lockfile` → `pnpm lint` → `pnpm test`（vitest，jsdom 环境）→ `pnpm build`。

## 6. 门禁速查

| 门禁 | 命令 | 失败含义 |
|------|------|----------|
| Proto 兼容性 | `buf breaking --against '.git#branch=origin/main'` | 破坏性变更（删字段未 reserved、改字段号等） |
| Lint 全量 | `golangci-lint run ./...` | 全仓任意 lint 问题 |
| 生成物一致性 | 钉版本生成后 `git diff --exit-code` | 生成物未提交或被手改 |
| RLS 相对基准 | RLS 耗时 vs 基准（30x 阈值） | RLS policy 性能显著劣化 |
| Coverage gate | 总覆盖率 ≥48% 只升不降 | 覆盖率跌破下限 |
| functions e2e | 断言 docker e2e 无 SKIP | e2e 被静默跳过 |

本地复现：

```bash
buf breaking --against '.git#branch=origin/main'
golangci-lint run ./...
mise run generate:all && git diff --exit-code -- genproto internal/pkg/config cmd go.mod go.sum
```

## 7. 健康检查与可观测（测试相关）

- `internal/infra/health/checks.go`：`DependencyChecker`（实现 lynx Checker，默认超时 2s），`NewCheckers(db, rdb, obj)` 并行探测 postgres / redis / minio；Details 带 panic 兜底为 unavailable。
- 端点：`GET /v1/health`（别名 `/v1/server/health`，ACCESS_PUBLIC）返回 `{status, dependencies}`；`GET /healthz/readiness` 任一依赖失败 503；`GET /v1/server/health/version` 返回构建注入的 `{version, commit, date}`。
- 慢查询：`internal/infra/clients/dbhook.go` 的 `SlowQueryHook`（bun QueryHook），阈值 `data.database.slow_query_threshold`（默认 500ms，`"0"` 禁用）。

## 8. 本地验证清单

```bash
mise run docker:up    # Postgres / Redis / MinIO
mise run lint         # go vet + gofmt + golangci-lint(全量) + sdk vet + eslint
mise run test         # sdk-go + sdk-ts + go test -race -cover（自动加载 .env）
mise run build        # console:build + go build（ldflags 注入 version/commit/date）
# 手工：curl /v1/health  /v1/server/health/version  /healthz/readiness
```

## 相关文档

- `02-quickstart.md` — 测试环境搭建（`.env` 测试 DSN）
- `06-databases.md` §12 — DocumentDB 测试地图
