# 13 部署与运维

面向运维 / 部署负责人：运行形态（五进程）、外部依赖、构建发布、生产配置要点（双账号契约、roles-sig 部署时序、Functions 运行面默认值）、健康检查、规模预警线、备份与升级、运维操作 runbook、多机部署。

> 事实源：`cmd/server/main.go`、`cmd/worker/provides.go`、`cmd/dispatcher`、`cmd/packer`、`mise.toml`、`configs/config.yaml.template`、`internal/pkg/config/config.proto`、`internal/infra/health/checks.go`、`docker/local/docker-compose.yml`、`docker/dokploy/`（README / docker-compose.yml / bootstrap-roles.sql）、`.github/workflows/image.yml`。

## 1. 运行形态：五进程

系统由五个独立进程组成：server / worker / dispatcher / packer 四个常驻服务 + torchwood CLI。四个服务端入口启动时经 `godotenv.Load()` 加载部署环境的 `.env`；torchwood CLI **不加载 `.env`**（CLI 常在不可信仓库目录运行，自动加载会被恶意 `.env` 重定向 endpoint/DSN 窃取凭据，`TORCHWOOD_CLI_*` 环境变量需自行 export）。五进程共用同一 config schema（`internal/pkg/config/config.proto`），各自只消费自己的分段：

| 进程 | 入口 | 职责 | 启动期校验 |
|------|------|------|-----------|
| **server** | `cmd/server` | Lynx Runner，监听三组端口：gRPC `127.0.0.1:9060`、HTTP `:9080`（grpc-gateway `/v1/*` + 自定义 serverhttp（Storage 上传下载、OAuth / Functions / Payments 回调、函数触发器公开路由）+ `/v1/realtime` WebSocket + Admin Console SPA `/console/` + landing 页）、metrics `127.0.0.1:9040`。装配在 `cmd/server/internal/runtime/`，服务注册顺序 grpc → gateway → realtime-subscriber → metrics | `security.jwt.secret` 必填；authz 策略语义断言失败即启动失败；`functions.dispatcher.url` 必填 |
| **worker** | `cmd/worker` | 后台作业常驻进程，12 个 lynx 服务组件（完整清单与节奏见 §1.2）：Functions 执行队列消费（内含孤儿恢复、执行记录清理、cron 调度、事件触发器消费）、outbox 事件分发、chunk 清理、Stream 修剪、支付关单、资产过期、订阅计费、用量聚合、leaderboards 结榜与清理、analytics rollup 与维护。与 server 共享 `app/domain/infra`，Wire 装配独立、无 `api` 层 | `data.database.source` 必填；`functions.dispatcher.url` 必填；主密钥强度校验与 server 同口径 |
| **dispatcher** | `cmd/dispatcher` | Functions 执行常驻进程（仓库根 `dispatcher/`），**唯一 docker.sock 持有方**：Build / Execute / RemoveImage 全部 daemon 操作经它分发，resident 实例池 + 租约认领，多节点模型（node_id + Redis registry + 执行路由）。`:9070` 单 HTTP API 面（`POST /v1/dispatch/*`；`GET /healthz`、`GET /metrics` 豁免共享密钥）。零 Postgres 依赖（仅 Redis + docker.sock） | `routing_mode` 值合法性；`registry` 模式要求 `registry_push=true` 且 `node_url` 非空（§7.2） |
| **packer** | `cmd/packer` | git 部署源打包常驻进程（仓库根 `packer/`）：专职承载不可信 git 输入的重资源操作（浅克隆 + worktree 核算 + 子目录物化为 zip 回传 server），把 `url@ref[:directory]` 归一为与 zip 源同构的代码包；server / worker 零 git 流量。无 Redis / DB / docker 依赖。`:9071` 单 HTTP API 面（`POST /v1/pack/git` + `/healthz`） | `functions.packer.url` 为空 = git 部署源未启用（app 层报明确错误，zip 源不受影响） |
| **torchwood CLI** | `cmd/torchwood` | `bin/torchwood`，经 `sdk/go/server` 的 InvokeJSON 按 protoregistry 动态分发调用 Server API（不直连 genproto）；命令实现随仓库根 `cli/`；退出码契约 0 成功 / 1 参数与校验错 / 2=40x / 3=5xx / 4=429。部署期作业（`admin sync-roles-sig`）与项目级备份（`admin export` / `admin import`）直连元数据库、不经 API 面 | `TORCHWOOD_CLI_*` 环境覆盖 |

本地开发：

```bash
mise run dev:server   # go run ./cmd/server
mise run dev:worker   # go run ./cmd/worker
```

> worker 承载支付关单、订阅计费、资产过期、用量落表、outbox 分发、排行榜结榜 / 清理、analytics 聚合等周期作业——**任何生产部署都需要 worker**，不只 Functions。dispatcher 在启用 Functions 的部署里必配（server / worker 执行函数的唯一通路，缺失直接拒绝启动）；packer 仅在需要 git 部署源时部署。本地仅调试数据库 / 存储时可不跑 dispatcher（Functions 执行走不通而已）。

### 1.1 端口（config.yaml.template 默认）

| 端口 | 用途 | 配置键 |
|------|------|--------|
| `:9080` | HTTP（gateway + serverhttp + `/console/` + landing） | `server.http.addr` |
| `127.0.0.1:9060` | gRPC（回环，gateway 同机转发；Dokploy compose 中改为 `:9060` 监听全部网卡、经 Traefik TLS h2c 对外，宿主回环端口仅作 SSH 隧道兜底） | `server.grpc.addr` |
| `127.0.0.1:9040` | Prometheus `/metrics`（无鉴权，仅回环；生产走反代 + 网络策略） | `server.metrics.addr` |
| `:9070` | dispatcher HTTP（分发 API + healthz + metrics） | `functions.dispatcher.addr` |
| `:9071` | packer HTTP（打包 API + healthz，与 dispatcher 缺省端口错开） | `functions.packer.addr` |

关停行为：server 注入 `lynx.WithDrainTimeout`（§4.3）与 `lynx.WithShutdownTimeout(30s)`；worker / dispatcher / packer 无排水窗口（无 LB 摘流面），仅 `WithShutdownTimeout(30s)` 有界关停。server 的 cleanup（关闭 DB / Redis 连接池等）挂 `OnPostStop`——所有服务 Stop 之后才执行，不掐排水期在途请求的连接。

### 1.2 worker 常驻作业清单（12 组件）

组件集合由 `cmd/worker/provides.go` 的 `NewComponents` 固定为 12 个 lynx 服务：functions-worker、chunk-cleaner、stream-trimmer、outbox-worker、payment-closer、asset-expirer、subscription-biller、usage-rollup、leaderboards-cleaner、leaderboards-settler、analytics-rollup、analytics-maintenance。

| 组件 | 职责 | 节奏 |
|------|------|------|
| **functions-worker** | Functions 执行队列消费（Redis Stream `torchwood:queue:functions-executions`，XREADGROUP，单进程 4 goroutine；瞬时失败重抛回队 ≤3 次，超限兜底标 failed） | 常驻 |
| ├ 孤儿恢复 | queued / building / running 超 staleAfter 判 failed（兜底 Redis 重启丢任务、进程崩溃孤儿）；staleAfter = 行内 `timeout_seconds` + 120s，NULL 回退 1h | 1min（启动即跑一轮） |
| ├ 执行记录保留策略清理 | 跨全部 active 项目按保留策略删除执行记录 | 10min |
| ├ cron 触发器调度 | `ClaimDueCron`（先 CAS 后入队），单轮全局预算 100；misfire 宽限 90s 与扫描周期同源 | 1min（启动即跑一轮，重启后 catch_up_once 首轮收敛） |
| └ 事件触发器消费 | `torchwood:events` 的 functions-triggers 消费组；订阅匹配器快照 15s 刷新；XREADGROUP Block 1s（配合优雅退出） | 常驻 |
| **chunk-cleaner** | Storage multipart 分片残留清理 | 1h（启动延迟 1min 跑首轮） |
| **stream-trimmer** | 执行队列 Stream `XTRIM` APPROX ~100k（XADD 不设 MaxLen 保未投递消息，裁剪由本任务低频驱动） | 10min |
| **outbox-worker** | outbox 行领取后 XADD 进 `torchwood:events`（LISTEN `tw_outbox` 唤醒 + 5s 兜底轮询；低频 ticker 清理已发布 / 死信行） | 常驻 |
| **payment-closer** | 超时未付订单关单 | 1min |
| **asset-expirer** | 资产到期核销 | 1min |
| **subscription-biller** | 订阅计费周期（续费 / past_due / expired 流转） | 1min |
| **usage-rollup** | 用量小时 bucket 落表 | 5min |
| **leaderboards-settler** | 结榜发奖结算扫描：按状态（已封榜 && 无结算行）而非定时投放，停机恢复自动补算，慢一轮无影响 | 10min |
| **leaderboards-cleaner** | 排行榜 retention 期清理 | 30min |
| **analytics-rollup** | analytics 日聚合幂等重算（昨日终算 + 当日刷新；带耗时 / 失败 Prometheus 指标） | 1h（启动即跑一轮） |
| **analytics-maintenance** | analytics_events 月分区预建 + retention 裁剪（24h）与 tombstone 清洗（6h）；启动即各跑一轮补齐错过的窗口 | 双 ticker |

## 2. 外部依赖

`mise run docker:up` / `docker:down` / `docker:purge`（`-v` 删卷）一键启停本地三件套：

| 依赖 | 镜像 | 端口 | 用途 |
|------|------|------|------|
| PostgreSQL | `percona/percona-distribution-postgresql:18`（发行版基座自带 pgvector 0.8.3 及 pg_stat_monitor / pgaudit 等常用扩展；**数据目录为 `/data/db` 而非官方系 `/var/lib/postgresql`，换镜像须同步改挂载点**） | 5432 | 元数据静态表 + 动态文档层（含 vector / HNSW） |
| Redis | `redis:7-alpine` | 6379 | 队列 / 上传会话 / 限流计数 / refresh 轮换记录 |
| MinIO | `pgsty/silo`（SILO，MinIO 社区延续分支，S3 API / 变量 / 磁盘格式兼容，钉 release tag） | 9000 / 9001 | S3 兼容对象存储（用户桶 + 函数部署代码包桶） |

- 均挂数据卷；环境变量支持 `${POSTGRES_USER:-torchwood}` 形式覆盖。
- 生产可将 `storage.s3.*` 指向任意 S3 兼容服务。
- **字符串排序语义（locale=C）**：compose 与 CI 显式 `POSTGRES_INITDB_ARGS="--locale=C --encoding=UTF8"`（跨镜像 / 跨平台确定性；**`--encoding=UTF8` 不可省**——locale=C 下 initdb 默认落 SQL_ASCII 编码，pgdriver 握手期直接拒连）。**string 列的 ORDER BY / 范围比较 = UTF-8 码点字节序**，非语言学序（中文不按拼音、`'Z'<'a'`）；等值与 filter 语义不受影响。initdb 参数仅首次建库生效；改 locale 须整库 dump → 重建 → restore 并 REINDEX 全部 text 索引。

## 3. 构建与发布

### 3.1 mise run build

```bash
mise run build
# = console:build + go build 五个二进制（server / worker / dispatcher / packer / torchwood）到 ./bin/
# ldflags 注入 VERSION/COMMIT/DATE（git describe / rev-parse / date），由 GET /v1/server/health/version 暴露
```

- `console:build` 产物 `console/dist/` 经 `//go:embed` 打进 server 二进制，在 `/console/` 下 serve（SPA fallback + 安全头）。
- **修改 Console 后必先 `mise run console:build` 再 `mise run build`**，否则 embed 旧 dist。
- Windows 产物为对应 `.exe`。

### 3.2 Docker 镜像与 Dokploy

```bash
mise run docker:build   # 多阶段 Dockerfile：builder 构 console + 五个二进制，runner 最小运行时
docker run --env-file .env -p 9080:9080 -p 9060:9060 torchwood:<tag>
```

本地镜像命名 `torchwood:1.0.0-<git>-<ts>`（git describe + 时间戳）。镜像内置全部五个二进制（`/usr/local/bin/{server,worker,dispatcher,packer,torchwood}`）与 `configs/` 基线，缺省 USER `torchwood`，ENTRYPOINT 为 server。

**Dokploy 一键部署**见 `docker/dokploy/README.md`。单 Compose 栈拓扑：

| 服务 | 形态 | 说明 |
|------|------|------|
| postgres / redis / minio | 常驻 | PG 为 percona PG18 pgvector 基座（`/data/db`）；Redis 带 `--appendonly yes`（refresh 轮换记录持久化，§6.1）；MinIO 为 SILO 钉版 |
| migrate → db-grants → roles-sig | 一次性作业链（`depends_on: service_completed_successfully`） | golang-migrate（owner DSN）→ `bootstrap-roles.sql` 补齐 authenticator 授权面（即 §4.5 的 ②③④④'，幂等可重跑）→ `torchwood admin sync-roles-sig`（owner DSN，用镜像内置 CLI）。作业全部幂等，镜像变更触发重建时自动重跑；面板上 Exited(0) 属预期 |
| server / worker | 常驻 | server 钉 `container_name: torchwood-server`（函数网络 DNS 回访锚点，§4.6）；worker 与 server 成对部署（§7.1 M5 拓扑） |
| dispatcher | 常驻 | `user: root` + 挂载 docker.sock（镜像缺省 torchwood 用户读不了宿主 `root:docker` 的 sock，permission denied 会让函数构建必失败）；⚠ sock 等同宿主 root 权限，仅在可信环境启用 |
| packer | 常驻 | 无状态（git clone + 物化 zip 回传 server），无 sock、无业务依赖 |

应用镜像由 GitHub Actions 预构建推 GHCR（`.github/workflows/image.yml`，部署机仅 pull 不编译，`pull_policy: always`）：main push → `latest` + `sha-<短commit>`；`v*` tag → `vX.Y.Z` / `vX.Y`。回滚 = Environment 里 `TORCHWOOD_IMAGE` 钉到 sha tag 后 Redeploy（迁移只进不退，回滚不回退 schema）。CI 末段部署 job 在镜像推送成功后回调 Dokploy webhook 触发部署（Dokploy 侧需关闭 Auto Deploy、保持 Autodeploy 总闸开启，触发源唯一化避免竞态）。

对外面：HTTP 域名直达 9080（Traefik label 直接声明在 compose，不走 Domains UI；80 自动 301 → https）；gRPC 域名由 Traefik 终结 TLS 后以 **h2c** 转发 9060（gRPC 要求端到端 HTTP/2）。`/v1/server/health/version` 可验证部署到的确切版本。

## 4. 生产配置要点

配置 schema `internal/pkg/config/config.proto`，模板 `configs/config.yaml.template`。入口先 `godotenv.Load()` 再从 `./configs` 绑定；环境变量映射规则见 `03-configuration.md`。

### 4.1 必配项

| 配置 | 环境变量 | 说明 |
|------|----------|------|
| JWT secret | `TORCHWOOD_SECURITY_JWT_SECRET` | ≥32 字符；命中弱子串黑名单（change-me / changeme / minioadmin / secret / password / torchwood）拒绝启动；`access_ttl` 默认 15m、`refresh_ttl` 默认 7d |
| Setup token | `TORCHWOOD_SECURITY_SETUP_TOKEN` | 未设置时首次 SignUp 被拒；生成：`openssl rand -hex 32` |
| DB 连接串 | `TORCHWOOD_DATA_DATABASE_SOURCE` | Postgres DSN；**运行态必须为非 superuser authenticator（§4.5）**，迁移作业用 owner 引导账号 |
| S3 凭据 | `TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID` / `TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY` | 生产务必覆盖本地 minioadmin；MinIO/S3 同时是 Functions 部署包持久桶的硬依赖（§4.6） |
| dispatcher 地址 | `TORCHWOOD_FUNCTIONS_DISPATCHER_URL` | server / worker 必填（缺失启动失败） |

其它：`security.encryption_key`（可选，OAuth / TOTP 静态字段加密的独立密钥；未配置回退 `jwt.secret` 并启动告警）、`storage.s3.endpoint/bucket/region`、`data.redis.addr/password`、`idgen.default_strategy`、`messaging.smtp`（`dev_log_otp` / `dev_log_sms` 仅 development 生效，生产发送路径直接拒绝，防 OTP 进日志）、`security.rate_limit`（通用限流，内置默认 ip 300/60s、user 1000/60s、api_key 6000/60s、functions_execution 6000/60s）。

### 4.2 反向代理与真实 IP

```yaml
security:
  trusted_proxies: []   # 默认不信任 X-Forwarded-For / X-Real-Ip
```

- 需恢复真实 IP：`TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32,10.0.0.0/8`（逗号分隔 CIDR）；
- 仅直连 peer 命中可信网段时才采信 X-Forwarded-For 首跳；gateway 与 gRPC 同进程部署时须包含 `127.0.0.1/32`。Dokploy compose 已配 `127.0.0.1/32,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`（采信 docker 私网段 XFF 首跳）。

### 4.3 关停排水：TORCHWOOD_ENV

server 在 `lynx.NewRunner` 前即确定 `drainTimeout`（详见 `03-configuration.md` §4）：

| `TORCHWOOD_ENV` | drainTimeout | 说明 |
|-----------------|--------------|------|
| development（dev/local/test） | 0 | 本地 Stop 立即关 |
| production（默认 / 未设） | 30s | 先排水 30s 再停，给 LB 摘流 |
| 任意 + `TORCHWOOD_SERVER_DRAIN_TIMEOUT` | 显式覆盖 | 如 `15s`、`0s` |

K8s 的 `terminationGracePeriodSeconds` 应大于 `drainTimeout + shutdownTimeout(30s)`，否则排水未完即被 SIGKILL。排水窗口只作用于 server；worker / dispatcher / packer 仅有 30s 有界关停（§1.1）。

### 4.4 ID 生成策略的 fail-closed 语义

`random` / `sequence` 等需读项目设置的策略会先取 `settings.idgen.*`（30s 进程内缓存），**读取失败（DB 抖动）宁可报错也不静默回退**到平台默认——否则破坏全局唯一性 / 顺序语义。现象为 `resolve idgen strategy` 错误伴随 `/v1/health` 的 postgres unavailable，DB 恢复后自愈。

### 4.5 应用 DSN 与权限：非 superuser authenticator（双账号契约）

生产部署采用**双账号形态**（PostgREST authenticator 模式）：

| 账号 | 身份 | 用途 | 不出现在 |
|------|------|------|----------|
| **owner 引导账号**（如 compose / CI 的 `POSTGRES_USER`） | superuser | 仅 `mise run db:migrate` 与扩展引导（§6.6） | 运行时配置 |
| **`tw_authenticator`** | 非 superuser、无 BYPASSRLS / CREATEDB / CREATEROLE | server / worker 运行态 DSN | 迁移作业 |

**为什么运行 DSN 不能是 superuser**：文档面权限判定的唯一执行点是每集合物理表上的 RLS policy。superuser 隐式 BYPASSRLS，**绕过全部 policy**——每请求 `SET LOCAL ROLE` + `app.roles` 注入、roles_sig 验签、"漏注入 → 恒 false"的 fail-closed 语义全部失效，任何 SQL 逃逸直接升级为跨租户全量读写 + 任意 DDL。

#### 一次性引导（owner 引导账号执行）

```sql
-- ① 登录账号：非 superuser、无任何特权位（密码走密管/环境注入，勿落库明文）
CREATE ROLE tw_authenticator LOGIN PASSWORD '<强随机口令>'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;

-- ② 三角色 membership（每请求 SET LOCAL ROLE 的变色龙源头）
GRANT tw_owner, tw_app, tw_system TO tw_authenticator;

-- ③ 库级权限：CONNECT + CREATE（tw_<project.id> schema 供给）
GRANT CONNECT, CREATE ON DATABASE <数据库名> TO tw_authenticator;
GRANT USAGE ON SCHEMA public TO tw_authenticator;

-- ④ 控制面静态表 DML：public 全表排除 catalog 两表（仅经角色可达）
--    与 tw_secrets（运行账号对密钥表零权限，永不授予）
DO $do$ DECLARE t text; BEGIN
    FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
        AND tablename NOT IN ('catalog_databases', 'catalog_collections', 'tw_secrets')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO tw_authenticator', t);
    END LOOP;
END $do$;

-- ④' projectschema 静态迁移的 FK 面（REFERENCES public.projects）
GRANT REFERENCES ON public.projects TO tw_authenticator;

-- ⑤ roles_sig 密钥面：**零授权**——迁移 000004 已 REVOKE 对 tw_secrets 的全部
--    权限；密钥落库由部署期 owner 一次性作业完成（§6.1 部署时序）。
```

后续新迁移新增 public 表时需以 owner 身份补授，或预建 default privileges 一劳永逸。本地开发与 Dokploy 路径分别见 `02-quickstart.md` 步骤 2.5 与 `docker/dokploy/bootstrap-roles.sql`（内容即本节 ②③④④'，由 compose 的 db-grants 作业自动执行）。

#### 验证命令

```sql
-- rolsuper 必须 false；五个特权位应全为 f
SELECT rolname, rolsuper, rolcreatedb, rolcreaterole, rolbypassrls, rolreplication
FROM pg_roles WHERE rolname = 'tw_authenticator';

-- 000004 membership 恰好三行（期望：tw_app / tw_owner / tw_system）
SELECT r.rolname FROM pg_auth_members m
JOIN pg_roles r ON r.oid = m.roleid
JOIN pg_roles a ON a.oid = m.member
WHERE a.rolname = 'tw_authenticator' ORDER BY 1;

-- tw_secrets 零权限判据（七特权位全 f）
SELECT has_table_privilege('tw_authenticator', 'public.tw_secrets', priv) AS granted
FROM (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER')) v(priv);
```

```bash
# SET ROLE 三角色可达性（各返回对应 current_user）
psql "<authenticator DSN>" -c "SET ROLE tw_owner; SELECT current_user; RESET ROLE;" \
  -c "SET ROLE tw_app; SELECT current_user; RESET ROLE;" \
  -c "SET ROLE tw_system; SELECT current_user; RESET ROLE;"

# 反例①：扩展安装是 superuser 引导面，迁移前的空库上必须被拒
psql "<authenticator DSN>" -d <未迁移的空库> -c "CREATE EXTENSION vector;"
# 期望：ERROR: permission denied to create extension "vector"

# 反例②：public schema 禁建（PG15 起 public 无 PUBLIC CREATE）
psql "<authenticator DSN>" -c "CREATE TABLE public.tw_nope (x int);"
# 期望：ERROR: permission denied for schema public
```

集成测试端到端锁定以上形态：`internal/pkg/testutil/nonsuperuser_test.go`（owner 跑迁移 + 建 authenticator，再以 authenticator 完成 roles_sig 同步、项目 / 业务库 / 集合创建与文档读写冒烟，断言 `rolsuper=false` 与 tw_secrets 七特权位全 false）。

#### 授权面边界

- **迁移账号（owner）独占的引导面**：`CREATE EXTENSION vector`（非 trusted）；`GRANT CREATE ON SCHEMA public TO tw_system` 等迁移内授权；三角色 membership 的初始 GRANT；**roles_sig 密钥落库**（`torchwood admin sync-roles-sig`，写 `public.tw_secrets`）。authenticator 跑迁移会在最早的 public 建表处即失败——数据库自身强制这一边界。
- **运行账号（authenticator）的授权面**：三角色 membership（业务文档 DDL 走 `tw_owner`、读写走 `tw_app`、内部旁路走 `tw_system`，均在事务内 `SET LOCAL ROLE`）；public 静态表 DML；`CREATE ON DATABASE`；`REFERENCES ON projects`。**不含 `tw_secrets`**。
- **密钥面结论**：运行 DSN 泄漏不再可读 roles_sig 密钥——`app.roles` / `app.roles_sig` GUC 伪造通道封死，也无法删钥制造 fail-closed DoS。残余暴露面仅剩进程内存中的派生钥（攻击者需先攻陷运行进程，而非仅 DSN）。

#### 部署时序契约

部署 / 换钥顺序固定为：**迁移（含 000004）→ `torchwood admin sync-roles-sig`（owner / 引导 DSN）→ server / worker 启动**。Dokploy 路径由 compose `depends_on` 链自动保证。

```bash
# DSN 与主密钥显式传参（推荐——作业 DSN 必须指向 owner 引导账号，
# 与运行态 data.database.source 分别注入）：
torchwood admin sync-roles-sig \
  --dsn "$OWNER_DSN" \
  --jwt-secret "$TORCHWOOD_SECURITY_JWT_SECRET"
# flags 缺省分别读 TORCHWOOD_DATA_DATABASE_SOURCE 与 TORCHWOOD_SECURITY_JWT_SECRET
```

- `--jwt-secret` **必须与运行态 `security.jwt.secret` 同源同值**（派生钥 = HMAC-SHA256(主密钥, "tw-roles-guc-v1")；不一致时 `tw_roles()` 验签 fail-closed，文档查询不可用而非静默放行）。
- 幂等：重跑安全（同钥整体 no-op）。**换钥 = 改运行态 `security.jwt.secret` → 重跑本作业 → 滚动重启**，换钥窗口内旧进程签发的 sig 经 previous 槽验签（双钥语义）。
- **fail-closed 属预期**：首次部署或换钥后未跑 sync 作业前启动服务，文档查询因零角色不可见——跑完作业即恢复，无需重启服务（验签按语句实时读 `tw_secrets`）。

#### 撤销与重建

回收 authenticator：逐库 `DROP OWNED BY tw_authenticator;` 后 `DROP ROLE tw_authenticator;`（membership 随之撤销）。角色是集群级对象，多库部署逐库处理（与 §6.7 的跨库处置纪律一致）。

### 4.6 Functions 运行面：容量默认值与镜像缺失自愈

**分发通路**：函数执行统一经 dispatcher 分发（server / worker 零 docker.sock 依赖）；`functions.dispatcher.shared_token` 为可选内网共享密钥（与 dispatcher 进程同值，空 = 不校验，仅限可信内网）。函数容器网络缺省 per-project 隔离（`tw-func-<project_id>`，不可信函数另入 `tw-func-<project_id>-int` 无 NAT 出网变体）；`functions.execution.api_base_url` 注入 `TW_API_BASE_URL` 供函数回访 Server API，须为函数网络内可达的 server 地址（compose 配 `callback_container: torchwood-server` 由 dispatcher 把 server 容器 attach 进函数网络，经容器名 DNS 解析）。

**容量与超时默认值**（`configs/config.yaml.template` / `dispatcher/pool.go`）：

| 键 | 默认 | 语义 |
|----|------|------|
| `functions.client_invoke.per_user_concurrency` | 8 | 客户端调用每用户并发闸门（进程内 keyed 信号量；多实例部署为近似全局，全局上限 = 上限 × 实例数）。超限有界排队，队首超时 10s 报 ResourceExhausted |
| `functions.dispatcher.max_resident_instances` | 16 | 每 daemon 常驻实例总量上限（防单项目耗尽宿主内存）。内存敞口 = 实例数 × spec 内存：全 shared-1x（0.5 CPU / 256MiB）≈4GiB、全 shared-2x（1 CPU / 512MiB）≈8GiB，按宿主余量调整 |
| 单函数 `max_instances` 缺省 | 4 | 函数未显式配置时 dispatcher 归一的突发并发上限（`dispatcher/pool.go` MaxInstancesDefault；下限 1，超限有界排队） |
| `functions.dispatcher.queue_depth` | 32 | 单函数有界排队深度（超限立即 ResourceExhausted） |
| `functions.dispatcher.queue_head_timeout` | 10s | 等待空闲实例的队首超时 |
| `functions.dispatcher.boot_timeout` | 60s | 实例启动健康探针等待上限（超时回收并报错） |
| `functions.dispatcher.build_timeout` | 5m | 部署构建整体超时（与客户端断开解耦；Go 冷构建在全新环境可逼近默认值，必要时调大） |
| `functions.dispatcher.timeout_budget` | 5 | 实例累计超时熔断阈值（超时不杀实例模式下，累计超时达到阈值即杀实例重建，防僵尸负载） |
| `functions.dispatcher.verify_build` | 开 | 部署后验证 spawn：build 成功后起池外实例轮询 `/_tw/health` 的质量门 |
| `functions.trigger.http_ip_per_minute` | 3000 | HTTP 触发器公开端点（`/f/{project}/{token}`）per-IP 限频（独立于全局 300/min 默认，容纳回调出口 IP 集中的场景） |

**镜像缺失自动重建（`functions.dispatcher.rebuild_on_missing_image`，默认开启）**：执行命中「部署镜像在本节点缺失」（宿主镜像被 `docker prune -a`、磁盘清理、环境迁移——Postgres ready 状态与宿主镜像存量漂移）时，server / worker 凭稳定错误标记识别并**异步触发该部署重建**；多副本并发触发（server 与 worker、同进程多实例）经 Redis SETNX 去重收敛为一次。重建源优先级：zip 源 = 盘上 zip → `functions.storage` 专用桶拉回复核 → git 源按行内源快照（URL + 钉死 SHA + 目录）经 packer 重新物化并复核 checksum；image 源为幂等 ImportImage。桶未启用且盘上 zip 缺失时退回声明边界——执行报错文案含 rebuild required，引导 redeploy。

**部署代码包持久层（`functions.storage.bucket`，缺省 `torchwood-functions`）**：部署 zip 在本地盘之外留对象存储副本（盘丢失 / 磁盘清理 / 多节点不共享盘时的重建源）。桶是平台内部资源：不进用户 bucket 命名空间（项目 buckets 表无行、API 不可见），连接复用 `storage.s3` 的 endpoint 与凭证；**写路径失败整体回滚（部署失败而非静默降级）——MinIO / S3 是 Functions 的硬依赖**。备份时 `mc mirror` 需连同该桶（§6.3）。桶在首次函数部署时按需创建。

**packer（git 部署源，可选）**：`functions.packer.url` 空 = git 源未启用。资源预算：单次 fetch + 物化 120s（超时 504）、克隆 worktree 200MiB、物化 zip 50MiB、并发 4（饱和立即 429）；`allow_insecure` 默认 false（放行 `http://` 与私网 / 回环目标，仅自托管内网 forge 场景显式开启，防 clone 打内网 / 云元数据端点）。

## 5. 健康检查与可观测

`internal/infra/health/checks.go` 并行探测 **postgres / redis / minio**（ObjectStore.Ping → BucketExists），各自 2s 超时，panic 兜底 unavailable；依赖明细带 10s 结果快照缓存（失效时并发刷新 singleflight，readiness 语义仍由 lynx 实时聚合）。

| 端点 | 说明 |
|------|------|
| `GET /v1/health`（别名 `/v1/server/health`，ACCESS_PUBLIC） | `{status:"ok"|"unavailable", dependencies:[{name,status,error?}]}`，HTTP 恒 200，状态在 body |
| `GET /v1/server/health/version` | `{version, commit, date}`（ldflags 注入） |
| `/healthz/liveness` | 常驻 200 |
| `/healthz/readiness` | 全健康 200 / 任一失败 503（compose healthcheck 消费） |
| `grpc.health.v1.Health` | gRPC 侧轮询快照 |
| dispatcher `GET :9070/healthz`、packer `GET :9071/healthz` | 各常驻进程存活面（豁免共享密钥，compose healthcheck 消费） |

**Metrics**：server 独立 HTTP（`server.metrics.addr`，默认 `127.0.0.1:9040`），`GET /metrics`。除 runtime 采集器外还有自定义业务指标：realtime Hub / Stream、documentdb 列授权 reconcile 与 schema 漂移对账、projectschema 迁移耗时、规模预警三指标（§5.1）。dispatcher 有独立 `/metrics`（池 / 实例 / 节点 / 容量指标）。

**日志**：统一 slog，`--log-level` 控制；gateway 请求日志为 Debug（RequestURL 含完整 query，含 OAuth code，生产开 debug 前需评估）；认证拒绝输出 Warn（无 token 明文）。

**慢查询**：`SlowQueryHook`（bun QueryHook）：

| `data.database.slow_query_threshold` | 行为 |
|--------------------------------------|------|
| `"500ms"`（默认） | 超阈值 Warn `slow query`（operation/query/duration/error） |
| `"0"` | 禁用 |
| 非法格式 | Warn 并禁用 |
| `data.database.debug: true` | 全量 SQL Debug（覆盖阈值） |

环境覆盖：`TORCHWOOD_DATA_DATABASE_SLOW_QUERY_THRESHOLD`。注意格式化 SQL 可能含 PII。

### 5.1 规模预警线（schema-per-project SLO）

schema-per-project 布局需要量化预警线，超限触发**多集群分片规划评估**（不改存储形态、不动产品语义）。

| 指标 | 形态 | 语义 | 采集点 |
|------|------|------|--------|
| `torchwood_documentdb_tables_total{kind}` | GaugeVec | `pg_class × pg_namespace` 聚合物理表计数。`kind=catalog`（控制面）/ `project_schema`（一段式静态平面）/ `business`（业务文档面） | server 启动钩子采集 + 进程内小时级刷新 |
| `torchwood_documentdb_pgdump_duration_seconds` | Gauge | 最近一次全库 pg_dump 耗时。**打点契约在进程外**：外部 cron 执行 pg_dump 计时后经 Pushgateway 或 node_exporter textfile 上报；应用内序列恒 0（占位），告警作用于外部序列 | 外部 cron（见下方契约） |
| `torchwood_documentdb_schema_migrate_duration_seconds` | Gauge | 最近一次项目 schema 迁移 Apply 耗时（含 advisory 锁等待，成功 / 失败都刷新） | projectschema migrator 埋点 |

**pg_dump 上报契约**（pg_dump 是重 IO 会话级作业，不在 server 进程内调度——由运维脚本持有节奏，Prometheus 消费其结果）：

```bash
# cron 示例：全库逻辑备份计时后推 Pushgateway
/usr/bin/time -f '%e' -o /tmp/pgdump_secs \
  pg_dump "$TORCHWOOD_DATA_DATABASE_SOURCE" -Fc -f /backup/torchwood.dump
cat <<EOF | curl --data-binary @- http://pushgateway:9091/metrics/job/torchwood-pgdump/instance/$(hostname)
torchwood_documentdb_pgdump_duration_seconds $(cat /tmp/pgdump_secs)
EOF
```

文本文件 collector 等价形态：写入 `/var/lib/node_exporter/textfile/pgdump.prom`。两种形态二选一，告警表达式相同。

**阈值与告警**：

| 指标 | Warn | Crit | 阈值依据 |
|------|------|------|----------|
| `tables_total`（project_schema + business 合计） | > 500 | > 1500 | 社区谱系：几百 schema 舒适、1–2 千起劣化（pg_dump 24h+、relcache 膨胀、autovacuum XID 风险）。表计数是 schema 数的先行量（一个项目 schema 随迁移集携带多张表，每业务库每集合再 +1） |
| `pgdump_duration_seconds` | > 3600（1h） | > 14400（4h） | 社区劣化谱系终点 24h+ 的 1/24 与 1/6 作为早期信号；健康库基线应为分钟级 |
| `schema_migrate_duration_seconds` | > 60 | > 300 | 健康库上全迁移集重放为亚秒级；60s 通常意味 advisory 锁排队或对象数膨胀 |

Prometheus 规则示例：

```yaml
groups:
  - name: torchwood-scale-warning
    rules:
      - alert: TorchwoodScaleTablesWarn
        expr: sum(torchwood_documentdb_tables_total{kind=~"project_schema|business"}) > 500
        for: 30m
        labels: {severity: warning}
      - alert: TorchwoodScaleTablesCrit
        expr: sum(torchwood_documentdb_tables_total{kind=~"project_schema|business"}) > 1500
        for: 30m
        labels: {severity: critical}
      - alert: TorchwoodPgDumpSlow
        expr: torchwood_documentdb_pgdump_duration_seconds{job="torchwood-pgdump"} > 3600
        labels: {severity: warning}
      - alert: TorchwoodSchemaMigrateSlow
        expr: max(torchwood_documentdb_schema_migrate_duration_seconds) > 60
        labels: {severity: warning}
```

**告警语义**：任一指标越线不构成可用性故障，处置动作是**触发多集群分片规划评估**（project → cluster 路由抽象：项目迁移 = schema + catalog 行 + 路由重指成套搬迁；跨集群视图降级为控制面聚合指标）。分片出口必须有排期承诺、不能永远停在预警线——触发后按 `15-exit-poc.md` C7 的决议记录推进。

## 6. 运维操作

### 6.1 迁移

```bash
mise run db:migrate
```

DSN 优先级：`MIGRATE_DSN` → `TORCHWOOD_DATA_DATABASE_SOURCE` → 由 `POSTGRES_*` 拼接（`MIGRATE_DSN` 是不入 `.env` 的一次性覆盖口，见 `02-quickstart.md`）。发布前先迁移再启动新进程。

**roles_sig 部署时序契约**：迁移（含 000004）→ `torchwood admin sync-roles-sig`（owner / 引导 DSN，§4.5）→ server / worker 启动。首次部署或换钥后未跑 sync 作业前，文档查询 fail-closed 属预期，跑完作业即恢复。换钥流程：改运行态 `security.jwt.secret` → 重跑 sync 作业 → 滚动重启（previous 槽保换钥窗口）。Dokploy 路径该时序由 compose 作业链自动执行。

**双账号契约**：迁移 DSN 必须是 **owner 引导账号**——`CREATE EXTENSION vector`（§6.6）与 public schema 建表等引导面只有它可执行，authenticator 跑迁移会在最早期即失败（fail-safe）。生产中迁移作业与 server / worker 运行时的 DSN 分别注入。

**Console / 端用户 refresh 轮换与 Redis 易失性**：两类会话的 refresh token 逐次轮换，轮换记录存 Redis。Redis 数据丢失（无持久化卷重启、换实例）= 轮换记录全丢，全部已登录会话在下一次刷新时报 "session expired" 失效，**重新登录即恢复，不是故障**；换 `security.jwt.secret` 等价全量失效。Dokploy compose 的 Redis 已带 `--appendonly yes`，部署建议一律配持久化（AOF / RDB）。多标签页并发刷新与刷新响应丢失后的重试由宽限窗口兜底（60s）：窗口内旧 token id 按当前链续签（不判重用、不连坐撤销）；窗口外恢复重放判定（mismatch → 撤销该用户全部 token / 删会话）。新登录开新链并清宽限槽。前端侧，401 强制跳转带 `?expired=1` 标记，Login 页看到该标记不自动跳回 Console——防止存在持续 401 源（如陈旧 cookie 副本）时 console↔login 无限弹跳。

### 6.2 首次引导（bootstrap）

全新库上启动 server 后打开 `/console/`，登录页自动切为初始化表单（依赖 `TORCHWOOD_SECURITY_SETUP_TOKEN`）。填写首个管理员（固定 `owner`，仅 `admins` 为空时可用，并发重复注册由 advisory lock 串行化拦截）+ `project_id` + `database_id`（两者必填，创建项目及其首个业务库；常规 CreateProject 缺省首库名为 `app`）。API Key 登录后在 Console 自行创建（secret 仅展示一次）。重置：删 `admins` / `projects` / `api_keys` 后重启引导。

### 6.3 备份

| 数据 | 位置 | 建议 |
|------|------|------|
| 元数据 + 动态文档 | Postgres | `pg_dump` 或卷快照，需覆盖全部 `tw_*` schema；项目级逻辑备份用 `torchwood admin export`（§6.3.1） |
| 对象（用户桶 + `torchwood-functions` 部署包桶） | MinIO | `mc mirror` 到异地 S3 或卷快照，**须连同函数部署代码包桶一起备份**（镜像缺失自动重建的源，§4.6） |
| Redis | 仅队列 / 缓存 / 计数 / refresh 轮换记录 | 队列与计数丢失由 worker 启动对账兜底（超 1h 标 failed）；refresh 记录丢失 = 会话下次刷新失效（§6.1） |

#### 6.3.1 项目级备份与恢复：torchwood admin export / import

admin 子命令**直连元数据库**（不经 API 面 / gRPC；运维工具属性）：

```bash
# 导出项目文档面：catalog 快照 + 每集合全行 NDJSON（to_jsonb 形态，含 _acl/_version）+ snapshot_seq
bin/torchwood admin export --project <project_id> --out /backup/p1 \
  --dsn "$TORCHWOOD_DATA_DATABASE_SOURCE"        # 缺省读 TORCHWOOD_DATA_DATABASE_SOURCE
# 产物布局
#   /backup/p1/manifest.json                 快照与索引（最后写出；无 manifest = 半成品，导入器拒收）
#   /backup/p1/data/collection-NNNNNN.ndjson 每集合一个文件

# 恢复（目标项目须已存在——项目行/静态平面属控制面，不在文档面往返范围；
# 对 manifest 中每个集合先清位 DROP TABLE + 删 catalog 行再重建重灌，可重跑幂等）
bin/torchwood admin import --project <project_id> --in /backup/p1 --dsn "$TORCHWOOD_DATA_DATABASE_SOURCE"
```

**snapshot_seq 与增量续接**：导出在单一 `REPEATABLE READ` 快照事务内读取 outbox 全局 `max(seq)`（snapshot_seq）、catalog 两表与全部集合行——快照后提交的写入不在导出行中、其 seq 必大于 snapshot_seq。因此恢复后执行 `:changes?since_seq=<snapshot_seq>`（import 结束输出 ResumeHint）即无缝续接导出后的增量；重放窗口即 outbox 保留窗口。

**物理名策略**：物理表名 = collectionID（逻辑即物理），导入按 catalog 行重建；集合表经与在线 `CreateCollection` 相同的 DDL 汇聚点重建（`_version` 列、默认索引、`_acl` GIN、RLS policy + FORCE、列级 GRANT 全走现役代码路径），行导入以 `tw_system` 身份直写（`_acl` / `_version` / 时间戳原样保真，分批事务）。

**工具身份要求**：运行账号需三角色 membership（同 §4.5 的 authenticator 形态即可）；vector 列恢复要求目标库已启用 pgvector（§6.6）。

#### 6.3.2 与 pg_dump -n 的对照

| 维度 | `torchwood admin export/import`（推荐日常项目级） | `pg_dump -n tw_<project>_<db>`（schema 级） |
|------|------|------|
| 范围 | 一项目跨**全部业务库**（catalog 行 + 数据行） | 单个 schema 的物理对象；多库项目需逐 schema dump，且 catalog 行在 public，**不在** dump 内 |
| 恢复方式 | import 重建 catalog + 表 + 行（幂等清位重灌） | 需手工处理 catalog 两表的配套行，否则同名库 / 集合无法重建 |
| `_acl` / RLS | 行内 `_acl` 原样保真，RLS / 列授权由现役 DDL 路径重建 | policy / GRANT 随 dump 还原，但对象属主 / 角色名需目标库一致 |
| 增量续接 | snapshot_seq + `:changes` 闭合 | 无（需自建） |
| 适用场景 | 项目迁移、重建路径、单项目时间点备份 | 整库快速快照、schema 结构审计、DBA 习惯的全量兜底 |

运行级建议：全实例物理兜底用 `pg_dump -Fc`（全库，覆盖 public 控制面与全部 `tw_*`），项目级 / 跨实例搬迁用 export / import；两者不互斥（§5.1 的 pg_dump 计时指标继续作为规模预警信号）。

### 6.4 升级

1. 备份 PG + MinIO（含 `torchwood-functions` 桶）；
2. `mise run db:migrate`（+ roles_sig 时序，见 §6.1）；
3. 滚动 `server`（校验 `/healthz/readiness` 200 与 `/v1/server/health/version`）；
4. 重启 `worker`；同批滚动 `dispatcher` 与 `packer`；
5. 灰度验证 Client / Server API；
6. 摘旧实例。

Dokploy 路径以上收敛为一次 **Redeploy**：拉最新镜像（`pull_policy: always`）→ 重跑迁移（增量）→ 授权 / roles-sig 幂等 no-op → 滚动替换 server / worker（`depends_on` 链保证时序）。

### 6.5 排障

| 现象 | 处置 |
|------|------|
| 健康 unavailable | 看 `dependencies[].name/error` 定位 PG / Redis / MinIO |
| 代理后登录异常 | 检查 `trusted_proxies` 是否含代理网段 |
| Console 旧页面 | `mise run console:build && mise run build` |
| 慢查询无日志 | 确认阈值非 `"0"` 且日志级别 ≥ Warn |
| 首次引导被拒 | 确认 `TORCHWOOD_SECURITY_SETUP_TOKEN` 已设且进程已重启 |
| 文档查询全不可见 | roles_sig 时序未走完（§6.1）或 `--jwt-secret` 与运行态不同值 |
| 函数执行报 rebuild required | 该部署的 zip 源无持久桶副本且盘上 zip 缺失（桶上线前的存量部署），redeploy 重建；新部署确认 `functions.storage.bucket` 可用（§4.6） |
| 函数构建 permission denied（docker.sock） | dispatcher 未以 root 运行或 sock 未挂载（§3.2 compose dispatcher 服务） |

### 6.6 启用 vector（pgvector）

vector 属性类型（`VECTOR(dims)` 列、HNSW 索引、`vectorSearch` 算子）依赖 pgvector 扩展；迁移 000005 在元数据库执行 `CREATE EXTENSION IF NOT EXISTS vector;`。**vector 非 trusted extension**：非 superuser 即使身为库 owner 也会被拒，因此启用方式取决于迁移执行身份。当前架构为单 PG database 多 schema，只需对 DSN 指向的这一个库启用一次。

**验证 SQL**（两路径通用）：

```sql
SELECT extname, extversion FROM pg_extension WHERE extname='vector';
-- 期望 1 行（percona 基座 = 0.8.3）；0 行 = 本库未启用
```

**路径一（推荐）：pgvector 预装镜像 + superuser 迁移身份**

适用于 docker / local、CI，以及迁移 DSN 即镜像 bootstrap 超管的自管部署。percona 发行版基座预装 pgvector，000005 由迁移身份直接执行成功，无额外步骤。自管若换基座，initdb 参数必须带 `--encoding=UTF8`（§2）。

**路径二：自备 PG 实例，迁移身份为非 superuser**

1. **每库一次**：由 DBA 以 superuser 在目标库执行 `CREATE EXTENSION IF NOT EXISTS vector;`；
2. 非 superuser 迁移身份照常 `mise run db:migrate`——000005 命中 `IF NOT EXISTS` 幂等分支，输出 NOTICE 后继续，迁移不报错。

**失败自诊断**（`mise run db:migrate` 在 000005 失败时按报错形态分流）：

| 报错形态 | 原因 | 处置 |
|----------|------|------|
| `permission denied to create extension "vector"` | 迁移身份非 superuser 且目标库未启用扩展 | 走路径二第 1 步；失败事务已回滚、版本记录未推进，重放安全 |
| `extension "vector" is not available` | PG 实例基座不含 pgvector 扩展文件 | 换 pgvector 预装基座或按官方文档安装扩展文件，之后仍走路径二第 1 步 |

### 6.7 RBAC 角色生命周期

三角色 `tw_owner`（DDL / 迁移身份）、`tw_app`（运行时应用身份，GUC 注入 + RLS 判定）、`tw_system`（BYPASSRLS 信任根）由迁移 000004 **幂等创建**（duplicate 容错），并 GRANT 给引导账号作 membership；down **不 DROP ROLE**（只回滚本库作用域：REASSIGN / DROP OWNED + REVOKE membership）。

**探测查询**（DBA 常规巡检 / 清理前使用）：

```sql
-- 角色存在性与特权位（tw_system 必须 BYPASSRLS=t，tw_app/tw_owner 必须 f）
SELECT rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole
FROM pg_roles WHERE rolname IN ('tw_owner','tw_app','tw_system') ORDER BY rolname;

-- membership 现状（集群级，跨库一致）
SELECT member.rolname AS member, grp.rolname AS group_role
FROM pg_auth_members m
JOIN pg_roles grp ON grp.oid = m.roleid
JOIN pg_roles member ON member.oid = m.member
WHERE grp.rolname LIKE 'tw\_%' ORDER BY 1,2;

-- 角色名下对象分布（清理前必查）
SELECT n.nspname AS schema, c.relname, pg_get_userbyid(c.relowner) AS owner
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE pg_get_userbyid(c.relowner) IN ('tw_owner','tw_app','tw_system')
ORDER BY 1,2 LIMIT 50;

-- 依赖残留全景（DROP ROLE 报 2BP01 时按此定位）
SELECT * FROM pg_shdepend WHERE refobjid IN (
  SELECT oid FROM pg_roles WHERE rolname LIKE 'tw\_%');

-- roles_sig 密钥面：tw_secrets 的 owner 与运行账号授权现状
--（期望：owner = 引导/迁移账号；authenticator 及三角色授权 0 行）
SELECT pg_get_userbyid(relowner) AS owner FROM pg_class
 WHERE oid = 'public.tw_secrets'::regclass;
SELECT grantee, privilege_type FROM information_schema.role_table_grants
 WHERE table_schema = 'public' AND table_name = 'tw_secrets';
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

**跨库影响**：`GRANT/REVOKE membership` 是**集群级**操作——在库 A 执行 000004 down 后，引导账号在库 B 的 membership 同步清空（跨库一致）；恢复方式 = 在任一库重新 GRANT（等价重跑 000004 up 段）。**结论：共享集群上对任一库跑 down，等于对全集群撤销运行时身份——其他库需重新 GRANT 后方可继续服务**。另注：`tw_system` 的 BYPASSRLS 属性独立于 membership 存续（down 后仍为 t），属预期。

## 7. 多机部署（细胞模型，四期）

设计源：`docs/design/functions-runtimes-and-sources.md` §4（M1-M8）；函数面语义见 `08-functions.md` §15。**细胞模型**：N 节点 ×（dispatcher 进程 + 本机 docker daemon），控制面共享（Redis：实例注册表 / spawn 锁 / 节点注册表 / 容量键；Postgres 元数据），**函数容器生命周期完全本机**——无 overlay 网络，函数容器只与本机 dispatcher 和本机 attach 的 server 回调容器通信。

### 7.1 拓扑与每节点一份 compose

`docker/dokploy/docker-compose.yml` 是**单节点模板**：多机时按节点复制部署（每节点独立 dokploy / compose 实例、各自 docker 网络隔离），仅以下服务只在**主节点**保留一份，其余节点整段删除、并把 `x-app-env` 中三个依赖地址指回主节点：

| 服务 | 部署位置 | 说明 |
|------|----------|------|
| postgres / redis / minio | 仅主节点 | 共享控制面；其余节点 DSN / Redis addr / S3 endpoint 指主节点 |
| migrate / db-grants / roles-sig | 仅主节点 | 一次性作业链（幂等），多节点重复跑无益 |
| server + worker | **每节点一份（成对）** | M6 回调多节点化：函数回调只达本节点 server；M5 worker 拓扑裁决——补构建固定读本节点盘上的部署 zip（构建亲和节点的本地快照，不跨节点迁移），worker 集中部署会让按 build_node 路由的补构建落空 |
| dispatcher | **每节点一份** | 唯一 docker.sock 持有方；`node_id` / `node_url` 各节点不同 |
| packer | 每节点一份或集中均可 | 无状态（git clone + 物化 zip 回传 server），无 docker.sock、无业务依赖，可独立多副本 |

各节点配置差异（其余同值）：

- `functions.dispatcher.node_id`：全集群唯一（缺省 hostname 可用但跨重启可能变，**显式配置**）；
- `functions.dispatcher.node_url`：本节点对等互达地址（如 `http://dispatcher-1:9070`）——**多机必须显式**，缺省的 `http://127.0.0.1:<port>` 推导对其他节点不可达；registry 模式启动期强制校验非空；
- `functions.dispatcher.max_resident_instances_global`（M4）：全集群常驻总量上限，各节点同值（缺省 0 = 不设全局上限）；
- container_name 冲突问题：`torchwood-server` 多机**不冲突**——各节点 compose 实例的 docker 网络互不相通，函数网络 DNS 各自解析**本节点**的 server（每节点一份 server，函数回调只达本节点，这是 M6「callback_container 语义 = 每节点可达的回调地址」的实现形态）。

### 7.2 路由模式选型（local vs registry）

| | `local`（缺省） | `registry`（完整形态） |
|------|------|------|
| 镜像分布 | 只在构建节点（不 push） | 构建后 push `functions.docker.registry`，任意节点按需 pull |
| 冷启动 | 转发构建节点（BuildNode 亲和） | 本地 spawn（spawn 前 EnsureImage，miss 即 pull） |
| 节点死时 | 该节点函数**全部不可用**，恢复 = 重新部署（重建即迁移） | **自动冷启动到幸存节点**（自愈，无需人工） |
| 额外组件 | 无 | 一个真实 registry（官方 `registry:2` 自包含，支持 filesystem 或 S3/MinIO 后端） |
| 启动期约束 | — | `registry_push=true` + `node_url` 非空（fail-fast 校验；非法 `routing_mode` 值三进程同口径拒绝启动） |

**何时切 registry**：要求「节点故障函数自动恢复」（可用性升级）或单节点容量不够、需要跨节点冷启动分流时。推荐路径：`local` 起步 → `registry`（把 `TORCHWOOD_FUNCTIONS_DOCKER_REGISTRY` 从命名前缀升格为真实 registry 地址 + `routing_mode=registry` + `registry_push=true`）。设计中的 `replicated` 实验档不在实现范围（§4 M7 降档裁决）。

### 7.3 容量共享（M4）与节点故障语义

- **两层上限**：`max_resident_instances`（每节点，进程内计数）→ `max_resident_instances_global`（全集群，Redis 容量键 `torchwood:fncap:resident:<node_id>` SET+TTL 求和，TTL 90s 与节点心跳同宽）。容量键刷新点：spawn 成功 +1 / terminate / reaper 回收 -1，reaper 每轮（15s）再无条件刷一次保 TTL；节点死后键在 TTL（90s）内消失，全局求和自动不再计入死节点。**Redis 不可用时全局检查 fail-open**（退化为仅本节点上限 + 限频告警）——容量门是可用性门不是安全门，与 M8 死节点收敛的 fail-safe（快照失败跳过收敛、绝不误删）语义方向相反。
- **节点注册表**：`torchwood:fnnodes:<node_id>` SET + TTL 90s，心跳周期 15s（TTL 的 1/6，容忍连续心跳失败）；最后一次心跳后 90s 键消失 = 节点失联判定基准，配合 reaper 死节点二次确认（连续两轮快照缺失）降噪，避免瞬时 Redis 抖动触发存活实例误删。
- **节点故障语义**：

| 故障 | local 模式 | registry 模式 |
|------|-----------|---------------|
| 节点死（机器/daemon） | 该节点函数不可用；实例随机器消失，记录由幸存节点按心跳过期收敛（M8）；恢复 = 重新部署 | 幸存节点 pull 镜像自动冷启动自愈 |
| dispatcher 进程崩（机器活） | 实例存活但无分发；重启后 reaper 对账收敛，或转发方按不可达处理 | 同左 |
| Redis 抖动 | 全局上限退化为仅本节点（fail-open），心跳/收敛停摆但不误删 | 同左 |

## 相关文档

- `02-quickstart.md` — 本地开发环境（含 authenticator 引导的最小流程）
- `03-configuration.md` — 配置项全景与环境变量映射
- `06-databases.md` — RLS 与角色注入语义（双账号契约的机制侧）
- `08-functions.md` §15 — 函数面多机语义（路由模式 / 容量共享 / reaper 收窄）
- `15-exit-poc.md` — 发布前门禁与决议记录
- `20-leaderboards.md` — 排行榜业务语义（§1.2 结榜 / 清理作业的领域侧）
- `docker/dokploy/README.md` — Dokploy 一键部署操作手册

