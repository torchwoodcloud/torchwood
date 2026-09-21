# 11 测试与质量保障

说明测试分层、`internal/pkg/testutil` 集成测试库、`mise run test` 入口、lint 全量门禁、CI 流水线与健康检查端点。所有提交代码的开发者必读。

> 关联：`AGENTS.md`、`mise.toml`、`.github/workflows/ci.yml`、`.golangci.yml`。

## 1. 测试分层

| 层级 | 需真实 DB | 典型位置 | 示例 |
|------|-----------|----------|------|
| 纯单元 | 否（stub / 内存实现） | `pkg/`、`internal/domain/`、`internal/api/*grpc/` | `pkg/crud/list_test.go`；`internal/api/servergrpc/projects_test.go`（stub repo + `contexts.WithPrincipal`）；`internal/api/interceptor/jwt_auth_test.go`（`policy_test_helper_test.go` 提供小策略注册表 + stub validator） |
| 授权矩阵 | 否（stub validator） | `cmd/server/internal/runtime/` | `authz_matrix_test.go`：数据源为真实 proto 的 `BuildMethodPolicies` 策略集，全部方法按 access 级别裁剪凭证档（匿名 / 端用户 / admin 各角色 / API key 各 scope），过真实 `AuthInterceptor`，断言与独立推导函数 `expectedDecision` 全量一致；同目录 `grpc_authz_test.go`（`AssertSemantic` 真实注册表语义断言）、`sdk_scopes_contract_test.go`（SDK scope 词表契约锁） |
| 集成 | 是（`SetupTestDB`） | `internal/infra/*`、`internal/app/*`、`internal/api/`（servergrpc / serverhttp / consolegrpc）、`cli/`、`worker/`、`tests/acceptance/` | `internal/infra/documentdb/postgres_test.go`；`tests/acceptance/p0_acceptance_test.go` |
| 依赖外部 daemon | 是 + Docker daemon | `dispatcher/`、`internal/infra/storage/` | `dispatcher/daemon_integration_test.go`（`dockerAvailable(t)` 探测 daemon，不可达 `t.Skip`）；`internal/infra/storage/minio_integration_test.go`（`TORCHWOOD_TEST_MINIO_ENDPOINT` 未设跳过） |

通用约定：

- 同包白盒测试为主（如 `package servergrpc`）；仅 `tests/acceptance/` 使用外部测试包（`package acceptance_test`）。
- DB 集成测试函数首行固定 `if testing.Short() { t.Skip("skipping integration test") }`（全仓约 150 个测试文件）；部分文件随后再查 DSN 未配置时二次 `t.Skip`。
- 断言用 `github.com/stretchr/testify/require`；gRPC 错误用 `status.Code(err)` + `codes.*`。
- API handler / app 用例测试用最小 stub 端口注入 Principal，不连 DB；连真实 DB 的文件多以 `_integration_test.go` / `_e2e_test.go` 后缀命名。

## 2. `mise run test` 是唯一入口

`mise.toml` 实际定义：

```toml
[tasks.test]
description = "全量门禁：lint + sdk 测试 + go test -race（.env 已自动加载）"
depends = ["lint:go", "lint:golangci", "test:sdk-go", "test:sdk-ts"]
run = "go test -race -v ./... -cover"
```

| 子任务 | 实际命令 |
|--------|----------|
| `lint:go` | `go vet ./...` + `test -z "$(gofmt -l .)"`（gofmt 零差异） |
| `lint:golangci` | `golangci-lint run ./...`（全量门禁） |
| `test:sdk-go` | `sdk/go` 内 `go test -v ./... -cover` |
| `test:sdk-ts` | `sdk/typescript` 内 `npm ci && npm run test`（tsc 编译后 `node --test`） |
| 主测 | 根 module `go test -race -v ./... -cover`（单元 + 集成全量） |

- `mise.toml` 的 `[env] _.file = ".env"` 对全部任务生效：测试 DSN 由仓库根 `.env` 注入（`.env.example` 已含两个 `TORCHWOOD_TEST_*` 键，复制即用）。**直接裸跑 `go test ./...` 会在 `SetupTestDB` 处 `t.Fatal`**（缺 DSN fail fast，见 §3.1）。
- 手工等价：导出 `TORCHWOOD_TEST_DATABASE_SOURCE` 与 `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` 后再 `go test ./...`。
- 前置：`mise run docker:up` 启动本地三件套（镜像与 CI service 完全一致，见 §5.1）。
- 工具链由 `mise.toml [tools]` 全钉版（go 1.26.5 / node 24.19.0 / pnpm 11.20.0 / buf 1.65.0 / protoc 31.1 / protoc-gen-go 1.36.11 / golangci-lint 2.12.2），CI 与本地同源。
- 并行度：`go test` 的 `-p` 未显式指定（本地与 CI 均取默认值）；testutil 的并行安全契约按"`-p N` 每包一独立进程"模型设计，转出验收口径为 `go test ./... -p 4` 连续全绿（见 §3.2 与 `15-exit-poc.md` A6）。

## 3. 集成测试数据库（internal/pkg/testutil）

### 3.1 环境变量

| 变量 | 示例 | 说明 |
|------|------|------|
| `TORCHWOOD_TEST_DATABASE_SOURCE` | `postgres://torchwood:torchwood@127.0.0.1:5432/TORCHWOOD_test?sslmode=disable` | 测试 DSN 模板，**库名段会被替换**为隔离库 |
| `TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE` | `postgres://torchwood:torchwood@127.0.0.1:5432/postgres?sslmode=disable` | 维护库 DSN（建库 / 删库 / advisory lock） |

无硬编码回退：任一变量缺失时 `SetupTestDB` 直接 `t.Fatal`，提示经 `mise run test`（自动加载 `.env`）或手工导出。testutil 自身的迁移循环测试与双账号测试（`migrations_cycle_test.go`、`nonsuperuser_test.go`）在 DSN 未设时 `t.Skip` 而非 Fatal。

两个测试 DSN 保持 **owner 引导账号**（superuser）：testutil 的建隔离库 + 跑全量迁移是双账号契约的迁移侧（`CREATE EXTENSION vector`、public 控制面建表、membership GRANT 都是引导面）。非 superuser 运行态形态由独立测试端到端锁定——`TestNonSuperuserAuthenticator_MigrateAndSmoke`（`testutil/nonsuperuser_test.go`）：owner 跑迁移 + 建 authenticator，再以 authenticator 完成 roles_sig 同步、项目 / 业务库 / 集合创建与文档读写冒烟，并断言 `rolsuper=false`。

### 3.2 SetupTestDB 生命周期

```go
db := testutil.SetupTestDB(t)
defer func() { _ = db.Close() }()
```

1. 校验两个 DSN 非空（缺失即 `t.Fatal`）；
2. 派生唯一库名：`<原库名>_<pid>_<seq>_<UnixNano 十六进制>`，强制小写（PG 标识符小写折叠；时间成分使进程被杀后的孤儿库撞名概率归零）；
3. 建库与迁移段持**集群级 advisory lock**（key `"twlif_db"`，`pg_try_advisory_lock` 100ms 轮询取锁，解锁用独立短超时 ctx 保证必然释放）——`-p N` 下每包一独立进程，进程内锁无效，互斥点必须选在 PG 服务端；
4. 经进程级共享 admin 连接池（pgdriver，读超时 10s）`CREATE DATABASE`；对集群瞬时过载（连接打满 / i/o timeout 等）指数退避重试（≤5 次，250ms→4s），重试撞"库已存在"（42P04）视为成功；
5. 执行迁移：`db/migrations/*.up.sql` 按文件名排序逐条执行（pgdriver simple protocol 单隐式事务，失败全回滚、重放安全），**不依赖 golang-migrate**；
6. 锁外初始化 roles_sig：`InitRolesSigKey(TestRolesSigMaster)` + `SyncRolesSigKey` 落本库 `tw_secrets`（漏接则所有 tw_app RLS 查询 fail-closed）；
7. 返回 `*clients.Database`（bun + pgdialect，写缓冲放宽到 2MiB 防大文档截断，`MaxOpenConns(16)` 防池无界扩张）；`t.Cleanup` 注册连接关闭与删库（锁内 `pg_terminate_backend` 杀残留连接 → `DROP DATABASE IF EXISTS`，同样带瞬时过载重试），测试结束自动删库无残留。

**并行安全契约**（全文见 `testutil/db.go` 包注释）：锁只包建库 / 迁移 / 删库段，库内用例执行不持锁，并行度不受影响；迁移 000004 的三角色（`tw_owner`/`tw_app`/`tw_system`）为集群级共享对象——up 原子幂等创建（duplicate 容错）、down 仅清理本库（REASSIGN/DROP OWNED + REVOKE membership）不 DROP ROLE。admin 池进程内单例（`sync.Once`），避免每测试新建池的认证洪峰。

### 3.3 常用 Fixture

| 函数 | 说明 |
|------|------|
| `CreateTestProject(ctx,db)` | 插入项目 + `projectschema.Apply` 全量迁移，返回 `(projectID, internalID, cleanup)` |
| `CreateTestProjectThrough(ctx,db,maxVersion)` | 同上，只 Apply 不超过 maxVersion 的迁移（`maxVersion<=0` 为全量）；apply 带瞬时过退避重试、按版本表断点续跑 |
| `CreateTestProjectT(ctx,t,db)` | `CreateTestProject` 的 `(*testing.T)` 变体：失败走 `t.Fatal` 而非 panic，新代码一律用本变体 |
| `CatalogIdent(projectID)` / `CatalogQuoted(projectID)` | 项目数据面 schema 的 `bun.Ident` / 引号安全字符串（测试拼 Raw SQL 用） |
| `InsertCatalogDatabase(ctx,db,...)` | 向 public 全局 `catalog_databases` 插一行 |
| `CreateTestAdmin` / `SignAdminToken` / `GrantAdminProject` | console admin 建号 / 签 JWT / 项目授权 |
| `CreateTestAPIKey(ctx,db,projectID,scopes)` | 插入 API Key，返回 `(rawSecret, cleanup)`；scopes 为空缺省四 scope |
| `NewMemObjectStore` | 内存 `ObjectStore`（Put/Get/Delete/Compose/List/Ping + `SetObjectTime` 模拟历史对象） |
| `NewInterceptorEnv(db,cfg,docDB)` | 装配 clientInfo + auth + ratelimit + audit 拦截器（与生产同链路序），小 PolicySet 注入；`InvokeUnary` 跑完整鉴权链，`InvokeUnaryHandler` 可挂真实 use-case handler |
| `NewInterceptorEnvWithExecutionTokens` | 上一条 + 函数执行 token 服务（`twx_` 执行身份回访链路） |
| `AuditLogCount` / `LatestAuditLog` | 审计行计数 / 最新一行断言 |
| `Eventually(t,timeout,check)` | 50ms 间隔轮询等待异步条件（事件分发 / worker 消费），超时 `t.Fatal` 附最后状态 |
| `FakeRateLimiter` | `domainauth.RateLimiter` 内存假实现（固定窗口计数；`Err` 注入模拟后端故障 fail-closed） |
| `SeedSystemDocumentCollections` | 在 sentinel 上重建七张系统集合文档表（users/sessions/identities/buckets/files/groups/memberships；仅供测试，生产系统资源是静态表） |
| `TestRolesSigMaster` | 集成测试 roles 签名主密钥常量 |

### 3.4 示例

```go
func TestPostgresDocumentDatabase_CRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProjectThrough(ctx, db, 8)
	defer cleanup()

	docDB := NewPostgresDocumentDB(db, nil)
	require.NoError(t, docDB.CreateDatabase(ctx, projectID, "app", "Application DB"))
	require.NoError(t, docDB.CreateCollection(ctx, projectID, "app", "posts", "Posts",
		[]databases.Attribute{{ID: "title", Key: "title", Type: "string", Size: 256}}, nil, nil, true))
	// ... CRUD / 权限 / 分页断言
}
```

## 4. Lint 全量门禁

本地与 CI 均为**全量门禁**（`--new-from-rev` 棘轮不存在）：

```bash
golangci-lint run ./...
```

- golangci-lint v2 配置（`.golangci.yml`）：`standard` 五件套之外启用 `bodyclose` / `gosec` / `noctx` / `sqlclosecheck`；`issues` 上限全开（`max-issues-per-linter: 0` / `max-same-issues: 0`），保证门禁确定性。
- 仅存**逐文件精确圈定**的 exclusions 暂挂项（多为 gosec 低危存量：G115 分页整型转换、G304 读取调用方指定路径等，每条带行内修复方案注释）；**禁止向清单追加新路径**，新代码必须过全量门禁。
- 版本由 mise 钉定（golangci-lint 2.12.2），与 CI 同版本。
- `mise run lint` 聚合四任务：`lint:go`（根 module vet + gofmt）→ `lint:golangci` → `lint:sdk-go`（`sdk/go` 独立 module vet + gofmt）→ `lint:console`（console eslint）。
- `mise run lint:proto` = `buf lint` + `buf breaking --against '.git#branch=origin/main'`（CI 亦单独跑）。

提交前：`mise run lint && mise run test`（或至少 `mise run lint` + `go test -short ./...`）。`mise run test` 的 deps 已含两个 lint 任务——本地裸 `go test` 全绿但 lint 红仍会翻车。

## 5. CI 流水线

`.github/workflows/ci.yml`：`push` 到 `main` + 全部 `pull_request` 触发；`concurrency` 按 ref 取消旧跑。

### 5.1 backend job（ubuntu-latest）

Services（均带 healthcheck，与 `docker/local/docker-compose.yml` 同镜像）：

| 服务 | 镜像 | 备注 |
|------|------|------|
| postgres | `percona/percona-distribution-postgresql:18` | 发行版基座内置 pgvector 0.8.3；`POSTGRES_INITDB_ARGS="--locale=C --encoding=UTF8"`——locale=C 时 initdb 默认 SQL_ASCII，pgdriver 握手校验 client_encoding=UTF8 失败直接拒连 |
| minio | `pgsty/silo:RELEASE.2026-09-03T13-18-01Z` | MinIO 社区延续分支，钉 release tag |
| redis | `redis:7-alpine` | 供 dispatcher 函数实例注册表真 Redis 集成测试 |

job 级 env：`TORCHWOOD_TEST_DATABASE_SOURCE`、`TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE`、`TORCHWOOD_TEST_REDIS_ADDR`（`dispatcher/registry_redis_integration_test.go`、`worker/event_triggers_test.go` 消费）、`TORCHWOOD_TEST_MINIO_ENDPOINT`（`internal/infra/storage/minio_integration_test.go` 消费）、`TORCHWOOD_RUN_DOCKER_TESTS="1"`（当前无 Go 代码读取，仅 CI 侧预留开关；docker 集成测试实际用 `dockerAvailable(t)` 探测 daemon）。

步骤（按 ci.yml 顺序）：

1. checkout（`fetch-depth: 0`，buf breaking 需 origin/main 可解析）→ `jdx/mise-action` 按 `mise.toml` 装齐工具链 → Go 模块 / 构建缓存与 pnpm store 缓存；
2. `buf lint` → **`buf breaking --against '.git#branch=origin/main'`**；
3. 预拉 `node:18-alpine`（functions runner 基镜像，docker 集成测试用）；
4. `mise run lint:go` / `mise run lint:golangci`（全量门禁）；
5. `go test -race -covermode=atomic -coverprofile=coverage.out ./...`（单元 + 集成；race 与 coverage 合并跑避免重复集成成本）；
6. **RLS 相对基准**（转出门禁 A7）：`go test ./internal/infra/documentdb/ -run 'TestRLS_RelativeBenchmark|TestRLS_ExplainInitPlanGate' -count=1 -v`；
7. **Coverage gate**：`go tool cover -func` 总覆盖率 ≥48%，只升不降（阈值依据 main 实测 51.1% 向下取整留 3 点缓冲）；
8. functions docker e2e 断言：`go test -v -count=1 -run 'TestDockerExecutor_' ./internal/infra/functions` 后断言输出无 `--- SKIP`（防静默跳过）。**注意：v1 docker 执行器移除（dispatcher 成为唯一执行路径）后，`TestDockerExecutor_` 前缀当前无匹配测试，该步骤空转过**；daemon 依赖的真集成测试已迁至 `dispatcher/`（`dockerAvailable` 探测）；
9. SDK Go tests（独立 module）：`sdk/go` 内 `go test -race ./...`；
10. **Codegen 漂移门禁**：`mise run generate:all`（generate:proto + generate:config + wire:all，protoc 31.1 / protoc-gen-go 1.36.11 由 mise 钉版）后 `git diff --exit-code -- genproto internal/pkg/config cmd go.mod go.sum`；
11. TS SDK test：`sdk/typescript` 内 `npm ci && npm run test` → `mise run sdk:demo-build`；
12. console 依赖安装（`pnpm install --frozen-lockfile`）→ `mise run build`（console:build + 五二进制 go build，验证 embed 链路）。

### 5.2 frontend job（ubuntu-latest）

`console/` 内依次：`pnpm install --frozen-lockfile` → `pnpm lint`（eslint）→ `pnpm test`（vitest run）→ `pnpm build`（tsc -b + vite build）。

## 6. 门禁速查

| 门禁 | 命令 / 载体 | 失败含义 |
|------|-------------|----------|
| Proto 兼容性 | `buf breaking --against '.git#branch=origin/main'` | 破坏性变更（删字段未 reserved、改字段号等） |
| Lint 全量 | `golangci-lint run ./...` | 全仓任意 lint 问题 |
| 生成物一致性 | 钉版本 `mise run generate:all` 后 `git diff --exit-code` | 生成物未提交或被手改 |
| 授权矩阵文档 | `TestAuthzMatrixDoc`（随主 `go test` 跑） | `docs/developer/authz-matrix.md` 与策略注册表漂移，需 `mise run gen:authz-matrix` |
| SDK scope 词表 | `TestSDKScopesContract_AlignedWithVocabulary` | SDK 常量与 `domainauth.AllScopeResources` 派生词表失配 |
| RLS 相对基准 | `TestRLS_RelativeBenchmark` / `TestRLS_ExplainInitPlanGate` | RLS 开/关 COUNT 比值 ≥30x（InitPlan 化失效或逐行 SubPlan 退化） |
| Coverage gate | CI 步骤，总覆盖率 ≥48% 只升不降 | 覆盖率跌破下限 |
| functions docker e2e 断言 | CI 步骤断言无 `--- SKIP` | 防静默跳过；**当前 `-run` 模式无匹配测试，步骤空转（见 §5.1 第 8 步）** |

本地复现：

```bash
buf breaking --against '.git#branch=origin/main'
golangci-lint run ./...
mise run generate:all && git diff --exit-code -- genproto internal/pkg/config cmd go.mod go.sum
mise run gen:authz-matrix && git diff --exit-code -- docs/developer/authz-matrix.md
```

## 7. 健康检查与可观测（测试相关）

- `internal/infra/health/checks.go`：`DependencyChecker` 实现 lynx `Checker`（默认超时 2s）；`NewCheckers(db, rdb, obj)` 覆盖 postgres（PingContext）/ redis（Ping）/ minio（BucketExists）。`Details(ctx)` 并行探测（各自超时 + panic 兜底为 unavailable），结果带 10s 快照缓存（singleflight 防并发打穿，`CacheTTL` 可覆盖）；readiness 实时语义仍由 lynx 聚合。
- 端点：`GET /v1/health`（别名 `/v1/server/health`，service_auth `ACCESS_PUBLIC`）返回 `{status, dependencies}`——依赖不可用时 gRPC 返回码仍为 OK，503 语义由 readiness 端点承担；`GET /healthz/readiness`（lynxhttp `WithHealthCheckers`）任一依赖失败 503（`TestHealthz_Readiness` 锁定 200/503 行为）；`GET /v1/server/health/version` 返回构建注入的 `{version, commit, date}`（`mise run build` ldflags 注入，`pkg/buildinfo`）。
- 慢查询：`internal/infra/clients/dbhook.go` 的 `SlowQueryHook`（bun QueryHook），阈值 `data.database.slow_query_threshold`——空串默认 500ms，`"0"` 禁用，解析失败 Warn 并禁用；`debug=true` 时记录全量 SQL。

## 8. 本地验证清单

```bash
mise run docker:up    # Postgres / Redis / MinIO（与 CI 同镜像）
mise run lint         # lint:go + lint:golangci + lint:sdk-go + lint:console
mise run test         # lint + sdk-go + sdk-ts + go test -race -cover（自动加载 .env）
mise run build        # console:build + go build 五个二进制（ldflags 注入 version/commit/date）
# 手工：curl /v1/health  /v1/server/health/version  /healthz/readiness
```

## 相关文档

- `02-quickstart.md` — 测试环境搭建（`.env` 中的 `TORCHWOOD_TEST_*` 测试 DSN）
- `06-databases.md` §12 — DocumentDB 测试地图与参考
- `13-operations.md` — 生产部署的健康检查与规模观测
- `15-exit-poc.md` — 转出 POC 门禁账本（A6 `-p 4` 并行稳定性、A7 RLS 相对基准的登记处）
- `authz-matrix.md` — 授权矩阵（生成物，漂移由 `TestAuthzMatrixDoc` 把关）
