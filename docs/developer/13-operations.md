# Torchwood 部署与运维指南

> 基于当前实现编写（以 `cmd/server/main.go`、`Taskfile.yml`、`docker/local/docker-compose.yml`、`internal/pkg/config/config.proto`、`internal/infra/health/checks.go` 为准）。
> 关联：`docs/developer/11-testing.md`（测试与门禁）、`AGENTS.md`。
> 修订记录：2026-08-23 重写（核对 Lynx 三进程、compose 三件套、`task build` 含 `console:build`、`TORCHWOOD_SECURITY_*`、`TORCHWOOD_ENV` 排水、`/healthz`/`/metrics`/`migrate`/`backup`）；2026-09-05 增补 §6.6 vector（pgvector）启用 runbook（转出门禁 A3：镜像预装/superuser 引导两路径 + 验证 SQL + 实测记录），§2 镜像行同步 pgvector 基座。

---

## 1. 运行形态：Lynx 三进程

| 进程 | 入口 | 职责 |
|------|------|------|
| **server** | `cmd/server/main.go` | gRPC（`server.grpc.addr` 127.0.0.1:9060）+ grpc-gateway HTTP `/v1/*` + 独立 `serverhttp`（Storage 上传下载、OAuth/Functions/Payments）+ Metrics + Admin Console SPA（`console.Dist`）+ 健康/版本 |
| **worker** | `cmd/worker/main.go` | 函数异步执行消费者：`BRPOP torchwood:queue:functions-executions`，4 goroutine 并发；启动对账——超 1h 的 `queued/building/running` 标 `failed`；瞬时失败重入队最多 3 次（`maxProcessAttempts`） |
| **CLI** | `cmd/client/main.go` | `bin/torchwood[.exe]`，经 `sdk/go/server` 的 `InvokeJSON` 走 gRPC 调 Server API（不直连 `genproto`） |

本地开发：

```bash
task dev:server   # go run ./cmd/server
task dev:worker       # go run ./cmd/worker
```

> 仅使用数据库/存储时 `server` 单进程即可；启用 Functions 才需 `worker`。

### 1.1 端口（`configs/config.yaml.template` 默认）

| 端口 | 用途 | 配置键 |
|------|------|--------|
| `:9099` | HTTP（gateway + `/console/`） | `server.http.addr` |
| `127.0.0.1:9060` | gRPC（回环，gateway 同机转发） | `server.grpc.addr` |
| `127.0.0.1:9040` | Prometheus `/metrics`（留空回退同值） | `server.metrics.addr` |

`cmd/server/main.go:16` 注入 `lynx.WithDrainTimeout`（见 §4.3）与 `lynx.WithShutdownTimeout(30s)`，`OnStop` 后才执行 `cleanup`（避免排水期关连接池）。

---

## 2. 外部依赖（`docker/local/docker-compose.yml`）

`task docker:up` / `task docker:down` / `task docker:purge`（`-v` 删卷）一键启停。

| 依赖 | 镜像 | 端口 | 用途 | 健康检查 |
|------|------|------|------|----------|
| PostgreSQL | `pgvector/pgvector:0.8.6-pg18`（pgvector 预装基座，锁 0.8.6 + PG18） | `5432` | 元数据静态表（`bun` + `golang-migrate`）与动态文档层（含 vector 列/HNSW） | `pg_isready` |
| Redis | `redis:7-alpine` | `6379` | 队列/上传会话/ID 生成 | `redis-cli ping` |
| MinIO | `minio:RELEASE.2024-11-07T00-52-20Z` | `9000`/`9001` | S3 兼容对象存储 | `exec 3<>/dev/tcp/127.0.0.1/9000` |

- 均挂 `postgres_data`/`redis_data`/`minio_data` 卷；
- 生产可将 `storage.provider: "s3"` 指向任意 S3 兼容服务；
- 环境变量均支持 `${POSTGRES_USER:-torchwood}` 等覆盖（见 `compose` 中 `environment` 与 `ports` 模板）；
- PostgreSQL 为 pgvector 预装基座（vector 非 trusted extension，启用路径与验证见 §6.6）；
- **字符串排序语义（locale=C，现状注记）**：compose 与 CI 显式 `POSTGRES_INITDB_ARGS="--locale=C"`（61ac141；Debian/glibc 基座默认 `en_US.utf8` 与原 musl 基座行为不一致，锁 C 恢复跨镜像/跨平台确定性）——**string 列的 `ORDER BY`/范围比较 = UTF-8 码点字节序**，非语言学序（中文不按拼音、`'Z'<'a'` 大小写混排）；等值与 equal/filter 语义不受影响。决策背景与选项见 `docs/developer/15-exit-poc.md` A9（推荐维持 C，待拍板）。initdb 参数仅首次建库生效，已有卷不受影响；改 locale 须整库 dump→重建→restore 并 REINDEX 全部 text 索引。

---

## 3. 构建与发布

### 3.1 `task build`（`Taskfile.yml:127`）

```yaml
build:
  deps:
    - task: console:build
  cmds:
    - go build -ldflags "-X main.version={{.VERSION}} -X main.commit={{.COMMIT}} -X main.date={{.DATE}}" -o ./bin/ ./cmd/server
    - go build -ldflags "..." -o ./bin/ ./cmd/worker
    - go build -ldflags "..." -o ./bin/torchwood{{if eq .OS "Windows_NT"}}.exe{{end}} ./cmd/client
```

- `console:build`（`Taskfile.yml:81`）为 `pnpm run build`（`tsc -b && vite build`），产物 `console/dist/` 再被 `console/embed.go:8` 的 `//go:embed dist` 打进二进制，由 `internal/infra/server/console.go:7` 的 `NewConsoleHandler` 在 `/console/` 下 serve（含 SPA fallback 与 `X-Frame-Options: DENY`/CSP 等安全头）；
- 版本：`VERSION=$(git describe --tags --always)`、`COMMIT=$(git rev-parse --short HEAD)`、`DATE=$(date +%Y%m%d%H%M%S)`，注入 `main.version`/`main.commit`/`main.date`（全小写），由 `GET /v1/server/health/version` 暴露；
- Windows 产物为 `bin/server.exe` / `bin/worker.exe` / `bin/torchwood.exe`；
- **修改 Console 后必先 `task console:build` 再 `task build`**，否则 embed 旧 `dist/`。

### 3.2 Docker 镜像（`task docker:build`）

`Taskfile.yml:195` 已定义：`DOCKER_IMAGE='torchwood:1.0.0-{{GIT_VERSION}}-{{TIMESTAMP}}'`，命令为 `docker build -t {{.DOCKER_IMAGE}} .`。仓库根已提供多阶段 `Dockerfile`（builder 构 console + 三二进制，runner 含最小运行时），可直接：

```bash
task docker:build
docker run --env-file .env -p 9099:9099 -p 9060:9060 torchwood:1.0.0-xxx-yyy
```

---

## 4. 生产配置要点

配置 schema `internal/pkg/config/config.proto`，模板 `configs/config.yaml.template`。`cmd/server/main.go:8` 先 `godotenv.Load()` 再 `config.NewBindConfigFunc()` 从 `./configs` 绑定；环境变量前缀 `TORCHWOOD_`，点号路径转下划线大写（如 `data.database.source` → `TORCHWOOD_DATA_DATABASE_SOURCE`）。

### 4.1 必配项

| 配置 | 环境变量 | 说明 |
|------|----------|------|
| JWT secret | `TORCHWOOD_SECURITY_JWT_SECRET` | ≥32 字符，含弱子串（`change-me`/`secret`/`torchwood`/`minioadmin`）拒绝启动；`access_ttl` 默认 `15m`、`refresh_ttl` 默认 `7d` |
| Setup token | `TORCHWOOD_SECURITY_SETUP_TOKEN` | 未设置时首次 `SignUp` 被拒（`internal/app/console/setup.go`），生成：`openssl rand -hex 32` |
| DB 连接串 | `TORCHWOOD_DATA_DATABASE_SOURCE` | Postgres DSN；`task db:migrate` 优先读同一变量。运行态必须为非 superuser authenticator（§4.5），迁移作业用 owner 引导账号 |
| S3 凭据 | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` / `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` | 生产务必覆盖本地 `minioadmin` |
| API Key 头 | `security.api_key.header` | 默认 `x-api-key` |

其它：`storage.endpoint`/`bucket`/`region`（默认 `us-east-1`）、`data.redis.addr/password`、`storage.provider`（`s3`/`minio`/`local`）、`idgen.default_strategy`（`uuid`/`ulid`/`snowflake`/`sequence`/`random`）、`telemetry.enabled`（当前未接线，保持 `false`）、`messaging.smtp`（`dev_log_otp`/`dev_log_sms` 为无 SMTP 时的开发兜底）。

### 4.2 反向代理与真实 IP

```yaml
security:
  trusted_proxies: []  # 默认不信任 X-Forwarded-For / X-Real-Ip
```

- 需恢复真实 IP：`TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32,10.0.0.0/8`（逗号分隔 CIDR，`internal/grpc/interceptor/trusted_proxy.go`）；
- 仅直连 peer 命中可信网段时才采信 `X-Forwarded-For` 首跳；gateway 与 gRPC 同进程部署时须包含 `127.0.0.1/32`。

### 4.3 关停排水：`TORCHWOOD_ENV`

`cmd/server/main.go:13` 在 `NewRunner` 前即确定 `drainTimeout`（`config.CurrentDrainTimeout()`）：

| `TORCHWOOD_ENV` | `drainTimeout` | 说明 |
|-----------------|----------------|------|
| `development` | `0` | 本地 `Stop` 立即关 |
| `production`（默认/未设） | `30s` | 先排水 30s 再停，给 LB 摘流 |
| 任意值 + `TORCHWOOD_SERVER_DRAIN_TIMEOUT` | 显式覆盖 | 如 `15s`、`0` |

- Lynx 在绑定 YAML 前就需要该值，故不进 `config.proto`；
- K8s `terminationGracePeriodSeconds` 应大于 `drainTimeout + shutdownTimeout(30s)`，否则排水未完即被 `SIGKILL`。

### 4.4 ID 生成策略的 fail-closed 语义

`random`/`sequence` 等需读项目设置的策略会先取 `settings.idgen.*`（30s 缓存），**读取失败（DB 抖动）宁可报错也不静默回退**到平台默认——否则会破坏全局唯一性/顺序语义。现象为 `resolve idgen strategy` 错误伴随 `/v1/health` 的 postgres `unavailable`，DB 恢复后自愈。

### 4.5 应用 DSN 与权限：非 superuser authenticator（转出 POC 门禁 A2）

生产部署采用**双账号形态**（PostgREST authenticator 模式，06-databases 不变量 #14）：

| 账号 | 身份 | 用途 | 不出现在 |
|------|------|------|----------|
| **owner 引导账号**（如 compose/CI 的 `POSTGRES_USER`） | superuser（initdb/bootstrap 创建） | 仅 `task db:migrate` 与扩展引导（§6.6） | 运行时配置 |
| **`tw_authenticator`** | 非 superuser、无 BYPASSRLS/CREATEDB/CREATEROLE | server/worker 运行态 DSN（`data.database.source`） | 迁移作业 |

本地 compose 的 `POSTGRES_USER` 是基础设施引导账号这一事实保留（initdb 创建，本地开发照旧）；**运行时配置不再示范/使用它**。

#### 为什么运行 DSN 不能是 superuser（反例）

文档面的权限判定唯一执行点是每集合物理表上的 RLS policy（`tw_visible`，`internal/infra/documentdb/rls_policy.go`）。superuser 隐式 BYPASSRLS，**绕过全部 policy**：每请求 `SET LOCAL ROLE` + `app.roles` 注入、roles_sig 验签（000029）、"漏注入 → 恒 false"的 fail-closed 语义全部失效，任何 SQL 逃逸直接升级为跨租户全量读写 + 任意 DDL。因此 superuser 只允许出现在迁移/引导作业，运行态一律用下面的非 superuser authenticator。

#### 一次性引导（owner 引导账号执行；可复制 SQL）

```sql
-- ① 登录账号：非 superuser、无任何特权位（密码走密管/环境注入，勿落库明文）
CREATE ROLE tw_authenticator LOGIN PASSWORD '<强随机口令>'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;

-- ② 000026 授权面：三角色 membership（每请求 SET LOCAL ROLE 的变色龙源头）
GRANT tw_owner, tw_app, tw_system TO tw_authenticator;

-- ③ 库级权限：CONNECT + CREATE（tw_<project.id> schema 供给——
--    projectschema.Apply 以 base identity CREATE SCHEMA，对齐 000026 对 tw_owner 的 CREATE ON DATABASE）
GRANT CONNECT, CREATE ON DATABASE <数据库名> TO tw_authenticator;
GRANT USAGE ON SCHEMA public TO tw_authenticator;

-- ④ 控制面静态表 DML（边界邻居面，base identity 直查的表）：public 全表
--    排除 catalog 两表（仅经角色可达，与 000026 授权面一致）与 tw_secrets（⑤ 单独授）
DO $do$ DECLARE t text; BEGIN
    FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
        AND tablename NOT IN ('catalog_databases', 'catalog_collections', 'tw_secrets')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO tw_authenticator', t);
    END LOOP;
END $do$;

-- ④' projectschema 静态迁移的 FK 面（000002/000004/000005/000006/000007 的
--    REFERENCES public.projects(id)：建外键要求被引用表上的 REFERENCES 权限）
GRANT REFERENCES ON public.projects TO tw_authenticator;

-- ⑤ roles_sig 密钥落库面（bootkit RolesSigKeySyncHook → clients.SyncRolesSigKey
--    的降级/落位/裁剪四语句，门禁 A4）：需要全量四权，残余风险见下节
GRANT SELECT, INSERT, UPDATE, DELETE ON public.tw_secrets TO tw_authenticator;
```

后续新迁移新增 public 表时，需以 owner 身份补授（或预建 `ALTER DEFAULT PRIVILEGES FOR ROLE <owner引导账号> IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tw_authenticator;`）。

#### 验证命令

```sql
-- rolsuper 必须 false（A2 完成判据核心语句）；五个特权位应全为 f
SELECT rolname, rolsuper, rolcreatedb, rolcreaterole, rolbypassrls, rolreplication
FROM pg_roles WHERE rolname = 'tw_authenticator';

-- 000026 membership 恰好三行
SELECT r.rolname FROM pg_auth_members m
JOIN pg_roles r ON r.oid = m.roleid
JOIN pg_roles a ON a.oid = m.member
WHERE a.rolname = 'tw_authenticator' ORDER BY 1;
-- 期望：tw_app / tw_owner / tw_system
```

```bash
# SET ROLE 三角色可达性（psql 以 authenticator 连接，各返回对应 current_user）
psql "<authenticator DSN>" -c "SET ROLE tw_owner; SELECT current_user; RESET ROLE;" \
  -c "SET ROLE tw_app; SELECT current_user; RESET ROLE;" \
  -c "SET ROLE tw_system; SELECT current_user; RESET ROLE;"

# 反例①：untrusted 扩展安装是 superuser 引导面——在迁移前的空库上必须被拒
#（vector 已随 000030 装好后 IF NOT EXISTS 会幂等跳过权限检查，注意别用它自证）
psql "<authenticator DSN>" -d <未迁移的空库> -c "CREATE EXTENSION vector;"
# 期望：ERROR: permission denied to create extension "vector"

# 反例②：控制面 DDL 面归属——public schema 禁建（PG15 起 public 无 PUBLIC CREATE）
psql "<authenticator DSN>" -c "CREATE TABLE public.tw_nope (x int);"
# 期望：ERROR: permission denied for schema public
```

实测记录（2026-09-05，本地 docker `pgvector/pgvector:0.8.6-pg18`）：临时库按序应用全部 30 个 up 迁移（psql）后按上述 SQL 引导，`rolsuper=f` 五特权位全 f、membership 三行、`SET ROLE` 三角色可达、两反例如期报错；以该账号完成 roles_sig 密钥落库 + 建项目/业务库/集合 + 文档读写（tw_system 写、tw_app+sig RLS 读）冒烟。集成测试锁定：`internal/testutil/nonsuperuser_test.go::TestNonSuperuserAuthenticator_MigrateAndSmoke`（PASS，count=1）。

#### 授权面边界与残余风险（迁移账号 vs 运行账号）

- **迁移账号（owner）独占的引导面**：`CREATE EXTENSION vector`（000030，非 trusted）；`GRANT CREATE ON SCHEMA public TO tw_system` 与 `ALTER FUNCTION ... OWNER TO tw_system`（000029）；`GRANT tw_owner, tw_app, tw_system TO CURRENT_USER`（000026，authenticator 无 ADMIN OPTION 无法自授）。authenticator 跑迁移会在最早的 `CREATE TABLE public` 处即失败（PG15 起 public schema 无 PUBLIC CREATE）——数据库自身强制这一边界。
- **运行账号（authenticator）的授权面**：0026 三角色 membership（业务文档 DDL 走 `tw_owner`、读写走 `tw_app`、内部旁路走 `tw_system`，均在事务内 `SET LOCAL ROLE`）；public 静态表 DML（控制面边界邻居面）；`CREATE ON DATABASE`（项目 schema 供给）；`REFERENCES ON projects`（静态迁移 FK）；`tw_secrets` 四权（roles_sig 落库）。
- **残余风险（tw_secrets 四权，A4 连带）**：`clients.SyncRolesSigKey` 的双钥四语句（降级 previous/落位 current/裁剪 third，注释见 `internal/infra/clients/tx.go`）以运行 DSN 执行，其中子查询与冲突位读使 **SELECT 不可省**——DSN 账号可读 roles_sig 密钥（000029 "tw_app 不可读防自签"的防线对 base identity 失效：持 SQL 会话者可伪造 `app.roles` GUC）并可删钥（fail-closed DoS）。这是"密钥落库挂在运行时启动钩子"形态下的必然代价，消除路径是把密钥落库改为部署期 owner 账号一次性作业（需代码变更，挂账 `docs/developer/15-exit-poc.md` A2 闭环注记）。相较 superuser DSN 的全域旁路（policy 绕过 + 任意 DDL + 集群级操作），该残余面显著收窄且不含 RLS 旁路。

#### 撤销与重建

回收 authenticator：逐库 `DROP OWNED BY tw_authenticator;`（撤销其对象特权；authenticator 名下的项目 schema 随其库/项目删除路径处理）后 `DROP ROLE tw_authenticator;`（membership 随之撤销）。角色是集群级对象，多库部署逐库处理（与 §6.6/A8 的跨库处置纪律一致）。

---


## 5. 健康检查与可观测

`internal/infra/health/checks.go` 并行探测 **postgres / redis / minio**（`ObjectStore.Ping` → `BucketExists`），各自 `2s` 超时，panic 兜底 `unavailable`。

| 端点 | 说明 |
|------|------|
| `GET /v1/health`（别名 `/v1/server/health`，`ACCESS_PUBLIC`） | `{status:"ok"\|"unavailable", dependencies:[{name,status,error?}]}`，整体状态由任一失败决定，gRPC 码保持 200 |
| `GET /v1/server/health/version` | `{version,commit,date}` |
| `/healthz/liveness` | Lynx 常驻 200 |
| `/healthz/readiness` | Lynx 驱动：全健康 200 / 任一失败 503 |
| `grpc.health.v1.Health` | gRPC 侧 10s 轮询快照 |

**Metrics**：`internal/infra/server/metrics.go` 独立 HTTP，`GET /metrics`（`promhttp.Handler()`），`server.metrics.addr`（默认 `127.0.0.1:9040`）。除 runtime 采集器外还有自定义业务指标（realtime Hub/Stream、documentdb 列授权 reconcile、projectschema ensure——规模预警线三指标见 §5.1）。

**日志**：统一 `slog`（`lynx` + `lynxzap`），`--log-level` 控制；gateway 请求日志为 `Debug`（`lynxhttp.WithRequestLog(true)`，`grpc_gateway.go`），`RequestURL` 含完整 query（含 OAuth code），生产开 debug 前需评估；认证拒绝由 `internal/grpc/interceptor/jwt.go:logAuthFailure` 输出 Warn（无 token 明文）。

**慢查询**：`internal/infra/clients/dbhook.go` 的 `SlowQueryHook`（`bun.QueryHook`）

| `data.database.slow_query_threshold` | 行为 |
|--------------------------------------|------|
| `"500ms"`（默认） | 超阈值 Warn `slow query`（`operation/query/duration/error`） |
| `"0"` | 禁用 |
| 非法格式 | Warn 并禁用 |
| `data.database.debug: true` | 全量 SQL Debug（覆盖阈值） |

环境覆盖：`TORCHWOOD_DATA_DATABASE_SLOW_QUERY_THRESHOLD`；`e.Query` 为含内联参数的格式化 SQL，可能含 PII。

### 5.1 规模预警线（schema-per-project SLO，转出 POC 门禁 B12）

出处：redesign §3.1 缓解 3 / §4.7——schema-per-project 布局需要量化预警线，超限触发**多集群分片规划评估**（project → cluster 路由抽象，§11-G1 决议：catalog 定位 cluster 内全局；排期承诺见 15-exit-poc.md C7），不改存储形态、不动产品语义。§11-A3 的观察项（policy × 集合规模对 plan cache/relcache 的影响）随本组指标可评估。

#### 指标与采集形态

| 指标 | 形态 | 语义 | 采集点 |
|------|------|------|--------|
| `torchwood_documentdb_tables_total{kind}` | GaugeVec | `pg_class` × `pg_namespace` 聚合物理表计数（relkind r/p）。`kind=catalog`=public 控制面+全局 catalog；`kind=project_schema`=一段式 `tw_<project.id>` 静态平面；`kind=business`=两段式 `tw_<p>_<db>` 业务文档面 | server 启动钩子同步采集一次 + 进程内小时级刷新（`cmd/server` `NewScaleMetricsHook` → `documentdb.CollectScaleMetrics`，单语句系统目录聚合） |
| `torchwood_documentdb_pgdump_duration_seconds` | Gauge（骨架） | 最近一次全库 `pg_dump` 耗时。**打点契约在进程外**：外部 cron/运维脚本执行 `pg_dump` 计时后经 **Prometheus Pushgateway** 或 node_exporter **文本文件 collector** 上报；应用内 `/metrics` 序列恒 0（占位），告警规则应作用于 Pushgateway/textfile 侧的同名序列。未来若内置调度器，经 `documentdb.ObservePgDumpDuration` 填充 | 外部 cron（见下方契约） |
| `torchwood_documentdb_schema_migrate_duration_seconds` | Gauge | 最近一次项目 schema 迁移 Apply 耗时（就绪缓存命中直通不刷新；含 advisory 锁等待与迁移事务，成功/失败都刷新；EnsureAll 多路并行时最后写赢） | `internal/infra/projectschema/migrator.go` `applyUpTo` 埋点 |

**pg_dump 上报契约**（选型说明：pg_dump 是重 IO 会话级作业，放 server 进程内调度会与关停排水/健康探测耦合，POC 阶段不做调度器——由运维脚本持有节奏，Prometheus 侧消费其结果）：

```bash
# cron 示例：全库逻辑备份计时后推 Pushgateway（job 名区分来源）
/usr/bin/time -f '%e' -o /tmp/pgdump_secs \
  pg_dump "$TORCHWOOD_DATA_DATABASE_SOURCE" -Fc -f /backup/torchwood.dump
cat <<EOF | curl --data-binary @- http://pushgateway:9091/metrics/job/torchwood-pgdump/instance/$(hostname)
torchwood_documentdb_pgdump_duration_seconds $(cat /tmp/pgdump_secs)
EOF
```

文本文件 collector 等价形态：`echo "torchwood_documentdb_pgdump_duration_seconds $(cat /tmp/pgdump_secs)" > /var/lib/node_exporter/textfile/pgdump.prom`。两种形态二选一；告警表达式相同（指标名一致，靠 `job`/`instance` label 区分来源）。

#### 阈值与告警规则

| 指标 | Warn | Crit | 阈值来源 |
|------|------|------|----------|
| `torchwood_documentdb_tables_total{kind="project_schema",kind="business"}`（按集群合计，`catalog` 为基线不参与） | > 500 | > 1500 | redesign §3.1 社区阈值：几百 schema 舒适、1–2 千起劣化（pg_dump 24h+、relcache 膨胀、autovacuum XID 风险）。表计数是 schema 数的先行量（一个全新项目 schema 携带 ~17 张静态/账务表，每业务库每集合再 +1），500/1500 对齐社区谱系的两档 |
| `torchwood_documentdb_pgdump_duration_seconds` | > 3600（1h） | > 14400（4h） | §3.1 记录的社区劣化谱系终点是 pg_dump 24h+（Appwrite 规模），取其 1/24 与 1/6 作为早期信号档；本地基线（健康库）应为分钟级，超 1h 即显著劣化 |
| `torchwood_documentdb_schema_migrate_duration_seconds` | > 60（/项目） | > 300 | 经验基线：健康库上全项目迁移集（17 个迁移文件）重放为亚秒级；60s≈两个数量级劣化，通常意味 advisory 锁排队或每项目 DDL 对象数膨胀——结合 tables_total 定位 |

Prometheus 规则示例（`tables_total` 以 server 实例 scrape 为准；`pgdump` 以 Pushgateway/textfile 序列为准——按 `job` label 选择，应用内恒 0 序列不参与）：

```yaml
groups:
  - name: torchwood-scale-warning
    rules:
      - alert: TorchwoodScaleTablesWarn
        expr: sum(torchwood_documentdb_tables_total{kind=~"project_schema|business"}) > 500
        for: 30m
        labels: {severity: warning}
        annotations:
          summary: "物理表计数超预警线（>500）——评估多集群分片规划（redesign §3.1 / §11-G1）"
      - alert: TorchwoodScaleTablesCrit
        expr: sum(torchwood_documentdb_tables_total{kind=~"project_schema|business"}) > 1500
        for: 30m
        labels: {severity: critical}
        annotations:
          summary: "物理表计数进入劣化区（>1500）——启动分片规划评估，勿扩容单集群硬扛"
      - alert: TorchwoodPgDumpSlow
        expr: torchwood_documentdb_pgdump_duration_seconds{job="torchwood-pgdump"} > 3600
        labels: {severity: warning}
        annotations:
          summary: "全库 pg_dump 超 1h——schema 规模劣化信号（§3.1），结合 tables_total 评估分片"
      - alert: TorchwoodSchemaMigrateSlow
        expr: max(torchwood_documentdb_schema_migrate_duration_seconds) > 60
        labels: {severity: warning}
        annotations:
          summary: "项目 schema 迁移重放超 60s——锁等待或对象数膨胀，评估分片（15-exit-poc C7）"
```

**告警语义**：任一指标越线不构成可用性故障，处置动作是**触发多集群分片规划评估**（§3.1 缓解 4 / §11-G1：project → cluster 路由表存控制面，项目迁移 = schema + catalog 行 + 路由重指成套搬迁；跨集群视图降级为控制面聚合指标）。分片出口必须有排期承诺、不能永远停在预警线（§9.3 教训 5，反面教材 Appwrite #6968）——触发后按 15-exit-poc.md C7 的决议记录推进。

---

## 6. 运维操作

### 6.1 迁移

```bash
task db:migrate
```

DSN 优先级：`TORCHWOOD_DATA_DATABASE_SOURCE` → `postgres://torchwood:torchwood@127.0.0.1:5432/torchwood?sslmode=disable`（可用 `POSTGRES_USER/PASSWORD/HOST/PORT/DB` 覆盖，`Taskfile.yml:48`）。发布前先迁移再启动新进程。

**双账号契约（§4.5）**：迁移 DSN 必须是 **owner 引导账号**（superuser/bootstrap）——`CREATE EXTENSION vector`（§6.6）与 public schema 建表等引导面只有它可执行，authenticator 跑迁移会在最早期即失败（fail-safe）；生产中迁移作业与 server/worker 运行时的 `TORCHWOOD_DATA_DATABASE_SOURCE` 分别注入，运行态配置永不使用引导账号。

### 6.2 首次引导（bootstrap）

全新库上启动 `server` 后打开 `/console/`，登录页自动切为初始化表单（依赖 `TORCHWOOD_SECURITY_SETUP_TOKEN`，`auth.go:59` 的 `GetSetupStatus`）。填写首个管理员（固定 `owner`，仅 `admins` 为空时可用）+ `project_id` + `database_id`，将创建项目（含系统 `default` 库）与业务库；API Key 登录后在 Console 自行创建（secret 仅展示一次）。重置：删 `admins`/`projects`/`api_keys` 后重启引导。

### 6.3 备份

| 数据 | 位置 | 建议 |
|------|------|------|
| 元数据 + 动态文档 | Postgres（`postgres_data`） | `pg_dump` 或卷快照，需覆盖全部 `tw_*` schema；项目级逻辑备份用 `torchwood admin export`（§6.3.1） |
| 对象 | MinIO（`minio_data`） | `mc mirror` 到异地 S3 或卷快照 |
| Redis（`redis_data`） | 仅队列/缓存/ID | 丢失可由 `worker` 启动对账兜底（超 1h 标 `failed`） |

#### 6.3.1 项目级备份与恢复：`torchwood admin export` / `import`（转出 POC 门禁 B5）

`cmd/client` 的 admin 子命令**直连元数据库**（不经 API 面/gRPC；POC 运维工具属性）：

```bash
# 导出项目文档面：catalog 快照 + 每集合全行 NDJSON（to_jsonb 形态，含 _acl/_version）+ snapshot_seq
bin/torchwood admin export --project <project_id> --out /backup/p1 \
  --dsn "$TORCHWOOD_DATA_DATABASE_SOURCE"        # 缺省读 TORCHWOOD_DATA_DATABASE_SOURCE

# 产物布局
#   /backup/p1/manifest.json                 快照与索引（最后写出；无 manifest = 半成品，导入器拒收）
#   /backup/p1/data/collection-NNNNNN.ndjson 每集合一个文件，行 = to_jsonb(d.*)

# 恢复（目标项目须已存在——项目行/静态平面属控制面，不在文档面往返范围；
# 对 manifest 中每个集合先清位 DROP TABLE + 删 catalog 行再重建重灌，可重跑幂等）
bin/torchwood admin import --project <project_id> --in /backup/p1 --dsn "$TORCHWOOD_DATA_DATABASE_SOURCE"
```

**snapshot_seq 与增量续接**：导出在单一 `REPEATABLE READ` 快照事务内读取 outbox 全局 `max(seq)`（snapshot_seq）、catalog 两表与全部集合行——快照后提交的写入不在导出行中、其 seq 必大于 snapshot_seq。因此恢复后执行 `:changes?since_seq=<snapshot_seq>`（`import` 结束输出 ResumeHint）即无缝续接导出后的增量（文档 create/update/delete 事件，tombstone 语义见 `:changes` 契约），本地副本/下游可精确收敛；outbox 表在 `public`，不受 `DROP SCHEMA` 影响，重放窗口即 outbox 保留窗口。

**物理名策略**：导入**沿用导出的 physical_name**（`c_<base32(8)>`），数据文件按逻辑 (database, collection) 寻址；集合表经与在线 `CreateCollection` 相同的 DDL 汇聚点重建（`_version` 列、默认时间索引、`_acl` GIN、RLS policy + FORCE、列级 GRANT 全走现役代码路径），行导入以 `tw_system` 身份直写（`_acl`/`_version`/时间戳原样保真，分批事务）。

**工具身份要求**：运行账号需三角色 membership（`tw_system` 读行/写行、`tw_owner` DDL/catalog，同 §4.5 的 authenticator 形态即可）；vector 列恢复要求目标库已启用 pgvector（§6.6）。

#### 6.3.2 与 `pg_dump -n tw_<project>` 的对照

| 维度 | `torchwood admin export/import`（逻辑备份，推荐日常项目级） | `pg_dump -n tw_<project>_<db>`（schema 级） |
|------|------|------|
| 范围 | 一项目跨**全部业务库**（catalog 行 + 数据行） | 单个两段式 schema 的物理对象；多库项目需逐 schema dump，且 catalog 行在 `public`，**不在** dump 内 |
| 恢复方式 | `import` 重建 catalog + 表 + 行（幂等清位重灌） | 需手工处理 catalog 两表的配套行，否则同名库/集合无法重建（F4-2） |
| `_acl`/RLS | 行内 `_acl` 原样保真，RLS/列授权由现役 DDL 路径重建 | policy/GRANT 随 dump 还原，但对象属主/角色名需目标库一致 |
| 物理名 | 沿用导出值（数据文件与物理名解耦） | 原样还原（含物理名） |
| 增量续接 | snapshot_seq + `:changes` 闭合 | 无（配合逻辑复制/触发器自建） |
| 适用场景 | 项目迁移、重建路径（`poc-to-release-migration.md` A5）、单项目时间点备份 | 整库快速快照、schema 结构审计、DBA 习惯的全量兜底 |

运行级建议：全实例物理兜底用 `pg_dump -Fc`（全库，覆盖 `public` 控制面与全部 `tw_*`），项目级/跨实例搬迁用 export/import；两者不互斥（§5.1 的 `pg_dump` 计时指标继续作为规模预警信号）。


### 6.4 升级

1. 备份 PG + MinIO → 2. `task db:migrate` → 3. 滚动 `server`（校验 `/healthz/readiness` 200 与 `/v1/server/health/version`）→ 4. 重启 `worker` → 5. 灰度验证 Client/Server API → 6. 摘旧实例。

### 6.5 排障

- 健康 `unavailable`：看 `dependencies[].name/error` 定位 PG/Redis/MinIO；
- 代理后登录异常：检查 `trusted_proxies` 是否含代理网段；
- Console 旧页面：`task console:build && task build`；
- 慢查询无日志：确认阈值非 `"0"` 且级别 ≥ Warn；
- 首次引导被拒：确认 `TORCHWOOD_SECURITY_SETUP_TOKEN` 已设且进程已重启。

### 6.6 启用 vector（pgvector）

vector 属性类型（`VECTOR(dims)` 列、HNSW 索引、`vectorSearch` 算子）依赖 pgvector 扩展；迁移 `db/migrations/000030_pgvector.up.sql` 在元数据库执行 `CREATE EXTENSION IF NOT EXISTS vector;`。**vector 非 trusted extension**：非 superuser 即使身为库 owner 也会被拒（permission denied），因此启用方式取决于迁移执行身份，按下述两条路径二选一。当前架构为单 PG database 多 schema（控制面 `public` + 项目/文档面均走 `CREATE SCHEMA`，`internal/infra/projectschema/migrator.go:85`），故只需对 `TORCHWOOD_DATA_DATABASE_SOURCE` 指向的这一个库启用一次。

**验证 SQL**（两路径通用，连接目标库执行，任意身份可查）：

```sql
SELECT extname, extversion FROM pg_extension WHERE extname='vector';
-- 期望 1 行：vector | 0.8.6；返回 0 行 = 本库未启用
```

**路径一（推荐）：pgvector 预装镜像 + superuser 迁移身份**

适用于 docker/local、CI，以及迁移 DSN 即镜像 bootstrap 超管（`POSTGRES_USER` 创建的角色）的自管部署。`pgvector/pgvector:0.8.6-pg18`（docker/local 与 `.github/workflows/ci.yml` 已统一）把扩展文件预装进镜像，000030 由迁移身份直接执行成功，无额外步骤。

实测记录（2026-09-05，本地 compose）：

```text
$ docker exec torchwood-postgres psql -U torchwood -d torchwood \
    -c "SELECT current_user, usesuper FROM pg_user WHERE usename=current_user;" \
    -c "SELECT extname, extversion FROM pg_extension WHERE extname='vector';"
 current_user | usesuper
--------------+----------
 torchwood    | t
(1 row)

 extname | extversion
---------+------------
 vector  | 0.8.6
(1 row)
```

**路径二：自备 PG 实例，迁移身份为非 superuser（authenticator 形态）**

前提确认——迁移用户应为库 owner 且非 superuser（期望返回 `f`）：

```sql
SELECT rolsuper FROM pg_roles WHERE rolname = '<迁移用户>';   -- 期望 f
```

- **第 1 步（每库一次）**：由 DBA 或一次性引导容器以 superuser 在目标库执行 `CREATE EXTENSION IF NOT EXISTS vector;`；
- **第 2 步**：非 superuser 迁移身份照常 `task db:migrate`。000030 命中 `IF NOT EXISTS` 幂等分支，输出 `NOTICE: extension "vector" already exists, skipping` 后继续，迁移不报错。

实测记录（2026-09-05，临时库 `vector_probe`，owner 为非 superuser 登录角色 `tw_migrator_probe`；验证完成后 `DROP DATABASE`/`DROP ROLE` 清理，主库无残留）。命令与输出摘要（按语句归纳）：

```text
-- 场景 a：未预装，非 superuser 迁移身份直接执行 000030 语句（失败形态）
$ psql -U tw_migrator_probe -d vector_probe -c "CREATE EXTENSION IF NOT EXISTS vector;"
ERROR:  permission denied to create extension "vector"
HINT:  Must be superuser to create this extension.

-- 场景 b：superuser 预装后，同一非 superuser 身份重跑同一语句
$ psql -U torchwood -d vector_probe -c "CREATE EXTENSION IF NOT EXISTS vector;"
CREATE EXTENSION
$ psql -U tw_migrator_probe -d vector_probe -c "CREATE EXTENSION IF NOT EXISTS vector;"
CREATE EXTENSION
NOTICE:  extension "vector" already exists, skipping
$ psql -U tw_migrator_probe -d vector_probe \
    -c "SELECT extname, extversion FROM pg_extension WHERE extname='vector';"
 extname | extversion
---------+------------
 vector  | 0.8.6
(1 row)
```

**失败自诊断**（`task db:migrate` 在 000030 失败时按报错形态分流）：

| 报错形态 | 原因 | 处置 |
|----------|------|------|
| `permission denied to create extension "vector"`（HINT: Must be superuser to create this extension.） | 迁移身份非 superuser 且目标库未启用扩展（场景 a 实测形态） | 走路径二第 1 步，superuser 预装后重跑迁移；golang-migrate 失败事务已回滚、版本记录未推进，重放安全 |
| `extension "vector" is not available`（HINT: The extension must first be installed on the system where PostgreSQL is running.） | PG 实例基座不含 pgvector 扩展文件（本会话以不存在扩展名实测同形态报错） | 换 pgvector 预装基座（如 `pgvector/pgvector:0.8.6-pg18`）或按 pgvector 官方文档为实例安装扩展文件，之后仍走路径二第 1 步 |

### 6.7 RBAC 角色生命周期（转出 POC 门禁 A8）

RBAC 三角色 `tw_owner`（DDL/迁移身份，SET ROLE 使用）、`tw_app`（运行时应用身份，GUC 注入 + RLS 判定）、`tw_system`（BYPASSRLS 信任根，内部读回/回填）由迁移 000026 **幂等创建**（duplicate_object 容错），并 GRANT 给引导账号（`POSTGRES_USER`）作 membership；down **不 DROP ROLE**（集群角色保留形态，只回滚本库作用域：REASSIGN/DROP OWNED + REVOKE membership）。

**探测查询**（DBA 常规巡检/清理前使用）：

```sql
-- 角色存在性与特权位（tw_system 必须 BYPASSRLS=t，tw_app/tw_owner 必须 f）
SELECT rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole
FROM pg_roles WHERE rolname IN ('tw_owner','tw_app','tw_system') ORDER BY rolname;

-- membership 现状（哪些账号持有哪些角色——集群级，跨库一致）
SELECT member.rolname AS member, grp.rolname AS group_role
FROM pg_auth_members m
JOIN pg_roles grp ON grp.oid = m.roleid
JOIN pg_roles member ON member.oid = m.member
WHERE grp.rolname LIKE 'tw\_%' ORDER BY 1,2;

-- 角色名下对象分布（清理前必查；对象属于某个库，随该库消亡）
SELECT n.nspname AS schema, c.relname, pg_get_userbyid(c.relowner) AS owner
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE pg_get_userbyid(c.relowner) IN ('tw_owner','tw_app','tw_system')
ORDER BY 1,2 LIMIT 50;

-- 依赖残留全景（DROP ROLE 报 2BP01 时按此定位）
SELECT * FROM pg_shdepend WHERE refobjid IN (
  SELECT oid FROM pg_roles WHERE rolname LIKE 'tw\_%');
```

**清理流程**（逐库；角色本身按需保留——up 会幂等重建）：

```sql
-- 在目标库执行（每库一次）
REASSIGN OWNED BY tw_owner, tw_app, tw_system TO <bootstrap_account>;
DROP OWNED BY tw_owner, tw_app, tw_system;
REVOKE tw_owner, tw_app, tw_system FROM <bootstrap_account>;
-- 角色保留不 DROP（up 幂等重建）；确认全集群弃用才执行：
-- DROP ROLE tw_app; DROP ROLE tw_owner; DROP ROLE tw_system;  -- 2BP01 → 回上一步
```

**跨库影响实测记录（2026-09-05，双沙箱库演练）**：`GRANT/REVOKE membership` 是**集群级**操作——在 sandbox_a 执行 000026 down 后，bootstrap 在 sandbox_b 的 membership 同步清空（pg_auth_members 0 行，跨库一致）；恢复方式 = 在任一库重新 `GRANT tw_app, tw_owner, tw_system TO <bootstrap>`（等价于重跑 000026 up 段，实测 membership 立即恢复）。**结论：共享集群上对任一库跑 down，等于对全集群撤销运行时身份——其他库需重新 GRANT 后方可继续服务**。另有实测注记：`tw_system` 的 BYPASSRLS 属性独立于 membership 存续（down 后仍为 t），属预期（信任根属性随角色定义保留）。
