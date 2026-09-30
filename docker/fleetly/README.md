# Torchwood —— fleetly 部署指南（IMPL-T2-4）

| 状态 | 日期 | 关联 |
|---|---|---|
| **待使用者执行窗口**（本环境无 staging 凭据/访问权：真机步骤未执行、未虚构结果；本地等价实证见 [cutover-runbook.md](cutover-runbook.md) 附录 A） | 2026-09-28 | [割接 runbook](cutover-runbook.md)（逐步命令）；[dokploy 现役形态](../dokploy/README.md)；fleetly 设计档 DT-8/DT-9/OT-2/OT-3 |

本目录是 torchwood 栈的 **fleetly（受控子集平台）部署形态**：六个常驻服务
（redis + minio + server + worker + dispatcher + packer）+ 三个一次性 init job
（migrate / db-bootstrap / roles-sig），postgres 落 fleetly 托管实例，域名经平台
域名资源 API 声明、运行时配置文件经平台 Config 资源提供、密钥与环境相关值经平台
env 注入。`docker/dokploy/` 的现役 Dokploy 栈在割接验收前**并行保留**（回滚 =
dokploy 栈未拆）。

- 受控子集校验（零告警）：`fleetly validate docker/fleetly/docker-compose.yml`
- 割接执行：[`cutover-runbook.md`](cutover-runbook.md)
- 真机段（托管库创建、dump/restore、卷搬运、DNS 切换、ACME 签发、函数端到端）
  在本环境无 staging 凭据/访问权，均标注「待使用者执行窗口」，未虚构结果。

## 1. 部署形态总览

| 项 | 形态 |
|---|---|
| app 名 | `torchwood`（compose 顶层 `name`；应用随首次部署自动创建） |
| 常驻服务 | `redis`（AOF 卷）、`minio`（SILO，`torchwood-storage` + `torchwood-functions` 桶）、`server`（9080 HTTP + 9060 gRPC h2c + healthz）、`worker`（Functions 异步消费者）、`dispatcher`（fleetly Tasks/build 客户端）、`packer`（git 部署源打包） |
| 一次性作业 | `migrate`（控制面迁移，`fleetly.job: init`）、`db-bootstrap`（运行账号 + 授权）、`roles-sig`（roles 签名密钥落库）——发布管线在晋级前执行，失败即发布失败 |
| 数据库 | **栈内无 postgres**：托管实例 `torchwood-pg`（模板 `percona-postgresql-18`，含 pgvector 0.8.6，torchwood 现役发行版）；服务经 `fleetly.databases: "torchwood-pg"` label 物化 `FLEETLY_DB_TORCHWOOD_PG_*` env 并挂库共享网络 |
| 网络 | 平台 per-app overlay；**服务别名 = compose 服务名**（`redis` / `minio` / `dispatcher` / `packer` 按名互访）；库网络由 label 牵线；跨 app 回访（mlbridge → server）经项目网别名 `torchwood-server`（需 `fleetly projects network attach torchwood`） |
| 入口 | 两域名（见 §3）：HTTP `http` 9080、gRPC `h2c` 9060；TLS 在平台边缘终结（ACME HTTP-01） |
| 配置 | compose 字面量（非敏感基线）+ 平台 env（密钥/环境值）+ Config 资源 `config.yaml` ×3 服务挂载 + `bootstrap-runtime.sql` / `bootstrap-roles.sql`（init job 用） |
| 镜像 | 应用镜像 `ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4`；迁移源 ref 与镜像同 commit（裁决见 §5） |
| 数据面 | postgres 落托管（dump/restore，runbook §3.1）；redis/minio 留栈内 named volume（搬运或明示重置，runbook §3.2/§3.3） |

与 dokploy 形态的**行为差异**（割接验收时如实核对）：

1. **80→443 重定向不做**（平台 v0.1 明确不做，OT-2）：客户端直接使用 `https://`；
2. **无宿主管端口**：gRPC 走域名 h2c 直连（`--tls`）；隧道兜底退回 T3-1 opt-in；
3. **编排顺序**：`depends_on` 被受控子集拒绝，顺序归发布管线；redis/minio 未就绪的
   秒级窗口内 server/worker 崩溃重启并自愈（启动顺序量化基线见 T2-0④ spike）；
   一次性作业链的顺序由 init job + 有界等待环承担（见 §2 #12）；
4. **平台 env 是 app 级**（注入全部服务）：JWT secret / DB DSN 等键在 redis/minio
   容器内也可见——平台现状的边界（dokploy 是逐服务注入），README 不掩饰；
5. **postgres 管理面归平台**：psql 终端/备份/恢复经 `fleetly databases` 子命令与
   Console，不再是栈内容器（`docker exec` 路径不存在）。

## 2. 改写清单（dokploy 现状 → fleetly 终态）

逐行核对 `docker/dokploy/docker-compose.yml` 与受控子集白名单
（fleetly 仓 `internal/compose/validate.go` + `testdata/whitelist.golden`）后的终态；
「承接面」= 该诉求在平台由哪个 API/资源/规则承载。终态 compose 过
`fleetly validate` **零告警**（原始输出见实施记录）。

| # | dokploy 现状 | fleetly 终态 | 承接面 |
|---|---|---|---|
| 1 | 顶层 `name: torchwood` | 保留 | app 标识（命名公式 `fleetly-<team>-<prj>-torchwood-<service>`） |
| 2 | `x-app-env` / `x-app-image` YAML 锚点 | 内联为各服务 `environment` | `x-*` 被 loader 移入 Extensions（平台不可见）；插值禁用后锚点无共享需求 |
| 3 | postgres 服务（镜像/env/卷/initdb bind/healthcheck） | **整段删除** | 托管实例 `fleetly databases create --template percona-postgresql-18 torchwood-pg`（DT-9）；`FLEETLY_DB_TORCHWOOD_PG_*` env 由 label 物化，库网络由 label 牵线 |
| 4 | postgres `POSTGRES_INITDB_ARGS: --locale=C --encoding=UTF8` | **平台模板无此参数** | percona 镜像 locale=POSIX → 实例 `server_encoding=SQL_ASCII`；`bootstrap-runtime.sql` 以 `ALTER DATABASE … SET client_encoding='UTF8'` 兜底（本地实证）；**建议平台模板补 `--encoding=UTF8`**（runbook §9） |
| 5 | `initdb/01-authenticator.sh`（创建 tw_authenticator） | **删副本，职责入 Config** | `docker/fleetly/bootstrap-runtime.sql`（幂等 DO 块 + `ALTER ROLE … PASSWORD :'auth_password'`，口令经平台 env 注入） |
| 6 | `bootstrap-roles.sql` bind | **改 Config 资源** | 平台 `configs:` external + 服务级 `{source,target}`；db-bootstrap 作业 psql 执行 |
| 7 | `postgres_data` 卷 | **删除** | 数据经 dump/restore 进托管实例（runbook §3.1）；旧卷保留到验收（回滚） |
| 8 | redis（image/`command --appendonly yes`/卷/healthcheck） | 保留 | 平台卷注册表（`fleetly-<app>-redis_data-<appid8>`）+ 平台健康门 |
| 9 | redis `restart: unless-stopped` | **删除** | 平台重启策略缺省 `any`/delay 5s（不在服务白名单） |
| 10 | minio（image/`command server /data …`/卷/healthcheck） | 保留；`command` 改为 `silo server /data …` | 平台 `command` 覆盖镜像 ENTRYPOINT → 必须直接调用 `/usr/bin/silo`（本地实证）；`MINIO_ROOT_*` 由平台 env 注入（compose 留 `CHANGE-ME` 占位） |
| 11 | minio `MINIO_ROOT_USER/PASSWORD: ${…}` 插值 | **删插值，平台 env** | `fleetly env set torchwood MINIO_ROOT_USER/MINIO_ROOT_PASSWORD`（app 级），server 的 S3 凭据同值注入 |
| 12 | `migrate`（镜像 + `-path=/migrations` bind + `${POSTGRES_*}` DSN + depends_on） | **`fleetly.job: init`**；源改 `github://torchwoodcloud/torchwood/db/migrations#78ea1a4`、DSN 改 `$FLEETLY_DB_TORCHWOOD_PG_URL?sslmode=disable`（容器 shell 展开）、depends_on 删除 | DT-4 init job（失败即发布失败）；**GHCR 镜像不含 `db/migrations`（一手镜像检查实证）**，故用公开仓库钉 commit 源；平台 init job 并行 → 下游作业用有界等待环（#13/#14） |
| 13 | `db-grants`（percona psql + bootstrap-roles.sql bind + `${POSTGRES_*}` env + depends_on） | **`db-bootstrap` init job**（percona 镜像作 psql 客户端；两个 SQL 走 Config；DSN/label 同 #12；5s×48 有界等待环） | 托管实例无 initdb 钩子：账号创建 + 授权由本作业承接；等待环覆盖「平台多 init job 并行」缺序（runbook §9 注明）；`TORCHWOOD_AUTH_PASSWORD` 空值 fail-closed |
| 14 | `roles-sig`（`entrypoint: torchwood` + `command: admin sync-roles-sig` + `*app-env` + owner DSN + depends_on） | **`roles-sig` init job**（`command: [sh, -c, …]` 内显式调用 `/usr/local/bin/torchwood`；DSN/JWT 同源；有界等待环） | B15 部署时序（迁移 000004 → 落钥 → 启动）；平台 `command` 覆盖 ENTRYPOINT，故不用 entrypoint 键（也不在白名单） |
| 15 | server `container_name: torchwood-server` | **删除** | 拒绝清单（容器/服务名平台管理）；函数回访改用任务网别名 `torchwood-server`（`TORCHWOOD_FUNCTIONS_EXECUTION_API_BASE_URL`） |
| 16 | server `networks: [default, dokploy-network]` | **全删** | 平台为 app 建专属 overlay；库网络由 `fleetly.databases` label 牵线（受控子集禁 external 网络） |
| 17 | server Traefik label ×11（HTTP/gRPC/80 跳转） | **全部删除** | 域名资源 API：两条 `fleetly domains add`（§3）；80→443 不做 |
| 18 | server `ports: 127.0.0.1:9060` | **删除** | 宿主管端口退 T3-1（P2 opt-in）；gRPC 走域名 h2c |
| 19 | server `./config.yaml` bind | **Config 资源** | 顶层 `configs: {config.yaml: external: true}` + `target: /app/configs/config.yaml`（容器内路径不变） |
| 20 | server `depends_on` ×4（postgres/redis/minio/roles-sig） | **删除** | 发布管线编排 + 应用侧重连自愈；一次性作业链见 #12–#14 |
| 21 | server `TORCHWOOD_FUNCTIONS_EXECUTION_API_BASE_URL: http://torchwood-server:9080` | 保留字面量 | 任务网细名即 `torchwood-server`（app 名 + 服务名；`network_members` 挂靠后生效，runbook §6.4） |
| 22 | 三应用服务 `pull_policy: always` + `${TORCHWOOD_IMAGE:-…:latest}` | **删 pull_policy；镜像钉 sha tag** | DT-2 部署期 tag→digest 钉定 + Redeploy 重解析 = `always` 等价物；升级 = 改 tag 重部署（§5） |
| 23 | worker（`entrypoint: worker` + `*app-env` + config bind + depends_on） | `command: ["/usr/local/bin/worker"]` + Config + 基线 env；**新增 `kill -0 1` 进程存活探针** | 平台 command 覆盖 ENTRYPOINT；镜像无 HTTP 健康面，进程存活探针（零告警要求；诚实标注见 §7）。**不用 `pgrep -x worker`**：fleetly 形态下 argv[0]=全路径 `/usr/local/bin/worker`，pgrep -x 按名匹配恒不中→健康门永不过（2026-09-29 staging 真机实证→E_HEALTH_TIMEOUT 循环）；`kill -0 1` 是 shell 内建、两形态通用 |
| 24 | dispatcher（docker.sock 挂载/`user: root`/多节点 env — T2-3 已改） | **零 sock、零 docker client**；config bind → Config；T2-3 的 fleetly env 保留（endpoint/token 平台 env 注入，app/members 字面量）；`/healthz` 探针保留；`depends_on: redis healthy` 删除 | IMPL-T2-3 契约；`TORCHWOOD_FUNCTIONS_FLEETLY_*` 见 §4 |
| 25 | packer（`entrypoint: packer` + `*app-env`） | `command: ["/usr/local/bin/packer"]` + 基线 env（无 config 挂载，现役同） | 平台 command 覆盖 ENTRYPOINT；`/healthz` 探针保留 |
| 26 | 顶层 `networks: dokploy-network: external` | **删除** | external 网络在拒绝清单（平台建网） |
| 27 | 顶层 `volumes: postgres_data` | **删除**（#7）；`redis_data` / `minio_data` 保留 | 平台卷注册表（`fleetly placement show` 可见） |
| 28 | 部署时序契约（`depends_on` 链：迁移 → 授权 → roles_sig → 启动） | **三个 init job**（平台并行 + 后两者有界等待环；任一失败 → 发布失败 + `release.job_*` 事件） | DT-4；平台唯一「先于长驻服务」排序原语（T2-0④ spike 结论） |
| 29 | Dokploy Environment 变量（`${VAR:?}` 必填守卫） | **平台 env（app 级）**；compose 不含 `${}`（插值禁用，会被当字面量）；必填缺失由应用侧 fail-closed + runbook §0 清单承担 | `fleetly env set`（§4）；DT-10 `fleetly.env.required` preflight **未实现**（平台挂账），见 §7 |
| 30 | `TORCHWOOD_HTTP_DOMAIN` / `TORCHWOOD_GRPC_DOMAIN` / `TORCHWOOD_GRPC_PORT` | **删除** | 域名资源（§3）；宿主管端口不做 |
| 31 | Dokploy psql 终端 / SSH 隧道（README §4/§9） | 平台面替代 | `fleetly databases`（托管库生命周期/备份）+ `fleetly domains verify` + `fleetly logs history`；隧道兜底退 T3-1 |

环境变量逐键映射（dokploy `Environment` → fleetly 落点）：

| dokploy 变量 | fleetly 落点 | 说明 |
|---|---|---|
| `TORCHWOOD_SECURITY_JWT_SECRET` | **平台 env（必填）** | 缺失 server/worker fail-closed；roles-sig 作业显式点名 |
| `TORCHWOOD_SECURITY_SETUP_TOKEN` | **平台 env（必填）** | Console 首个管理员引导令牌 |
| `TORCHWOOD_SERVER_HTTP_PUBLIC_URL` | 平台 env（必填，覆盖 compose 占位） | 占位 `https://change-me.example.com`，忘配时 OAuth/回调错误可见 |
| `POSTGRES_PASSWORD`（owner 引导） | **删除**（托管实例平台生成） | 需要时 `fleetly databases reveal torchwood-pg`；割接 dump/restore 用 |
| `POSTGRES_USER` / `POSTGRES_DB` | **删除** | 托管实例 user=`fleetly`、db=`torchwood_pg`（实例名 '-'→'_'） |
| `TORCHWOOD_AUTH_PASSWORD` | **平台 env（必填）** | 运行态 `tw_authenticator` 口令；db-bootstrap 作业写入数据库 |
| —— | `TORCHWOOD_DATA_DATABASE_SOURCE` | **平台 env（必填）**：`postgres://tw_authenticator:<口令>@torchwood-pg:5432/torchwood_pg?sslmode=disable`（显式 env set：物化 env 的用户是 owner `fleetly`，运行态必须非 superuser） |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | **平台 env（必填）** + compose 占位 | 同时作为 server/worker 的 S3 凭据（`TORCHWOOD_STORAGE_S3_*`） |
| `TORCHWOOD_FUNCTIONS_DISPATCHER_SHARED_TOKEN` / `TORCHWOOD_FUNCTIONS_PACKER_SHARED_TOKEN` | **平台 env（必填）** | server/worker（客户端）与 dispatcher/packer（服务端）同值 |
| `TORCHWOOD_FUNCTIONS_FLEETLY_ENDPOINT` / `_TOKEN` | **平台 env（必填）** | fleetlyd gRPC 端点（容器内可达）+ 机具令牌（scope tasks,build，§4）。endpoint 传输模式 scheme 显式选择（gRPC 社区约定 grpc/grpcs）：裸 `host:port` 或 `grpc://host:port` = 明文（既有部署缺省）；`grpcs://host:port` = TLS + 系统 CA 校验（ServerName 缺省跟随拨号主机名，对 LE 等公共证书透明验证）；`grpcs://host:port?insecure=true` = TLS 跳过校验（按 IP 直连等无 SAN 形态）；`grpcs://host:port?server_name=<name>` = 覆盖 SNI——未知 scheme 启动期 fail-closed |
| `TORCHWOOD_FUNCTIONS_FLEETLY_APP` / `_NETWORK_MEMBERS` | compose 字面量 | `torchwood` / `dispatcher,server`（受控回访挂靠声明） |
| `TORCHWOOD_SECURITY_ENCRYPTION_KEY` | 平台 env（可选） | 未设置回退 jwt.secret（启动告警） |
| `TORCHWOOD_SERVER_HTTP_CORS_ALLOW_HEADERS` 等非敏感基线 | compose 字面量 | 与现役同值；需要时平台 env 覆盖（`W_ENV_PLATFORM_OVERRIDE` 可见） |
| `TORCHWOOD_IMAGE` | **删除** | 镜像钉在 compose（§5） |

## 3. 域名声明（两条命令）

平台不再有 Traefik label；域名是 state 资源，经 CLI/API 写入并经入口收敛即时生效
（OT-2；同一 host 全局独占）。两条域名的 `service` 都是 compose 服务名 `server`，
端口/协议逐条不同（9080 http + 9060 h2c）：

```bash
# 变量（按环境替换）：FLEETLY_ADDR/FLEETLY_TOKEN/FLEETLY_PROJECT 见 cutover-runbook.md §0
fleetly domains add --service server --port 9080 --protocol http torchwood "<http-host>"   # Console/REST/Storage/healthz
fleetly domains add --service server --port 9060 --protocol h2c  torchwood "<grpc-host>"   # gRPC（TLS 终结 → h2c 回源 9060）
fleetly domains list torchwood
fleetly domains verify torchwood   # 平台侧解析 + 80/443 探测 + 证书材料（真机窗口执行）
```

- `--protocol http` 是纯 HTTP 后端（Console/Storage/healthz 走 9080）；
- `h2c` 表示 TLS 终结后以明文 HTTP/2 回源（gRPC 要求端到端 HTTP/2）；
- `--cert-mode http01`（缺省）：证书经 ACME HTTP-01 签发，**签发前提 = 域名解析已
  指向 fleetly 边缘**（DNS 切换与证书签发同在割接窗口内完成）；
- 上限不变（≤5/服务、≤10/app）；host 冲突 409 点名。

## 4. 平台 env、Config 与机具令牌清单

```bash
# —— 必填平台 env（密钥面；值不进仓库/对话/日志）——
TW_AUTH_PW="$(openssl rand -hex 24)"    # 仅 [A-Za-z0-9]，避免 URL 特殊字符
fleetly env set torchwood TORCHWOOD_SECURITY_JWT_SECRET  "$(openssl rand -hex 32)"
fleetly env set torchwood TORCHWOOD_SECURITY_SETUP_TOKEN "$(openssl rand -hex 32)"
fleetly env set torchwood TORCHWOOD_SERVER_HTTP_PUBLIC_URL "https://<http-host>"
fleetly env set torchwood TORCHWOOD_AUTH_PASSWORD "$TW_AUTH_PW"
fleetly env set torchwood TORCHWOOD_DATA_DATABASE_SOURCE \
  "postgres://tw_authenticator:${TW_AUTH_PW}@torchwood-pg:5432/torchwood_pg?sslmode=disable"
fleetly env set torchwood TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID     "<MINIO_ROOT_USER>"
fleetly env set torchwood TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY "<MINIO_ROOT_PASSWORD>"
fleetly env set torchwood MINIO_ROOT_USER     "<MINIO_ROOT_USER>"
fleetly env set torchwood MINIO_ROOT_PASSWORD "<MINIO_ROOT_PASSWORD>"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_DISPATCHER_SHARED_TOKEN "$(openssl rand -hex 32)"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_PACKER_SHARED_TOKEN     "$(openssl rand -hex 32)"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_ENDPOINT "<fleetlyd gRPC 容器可达端点>"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_TOKEN    "<机具令牌 see below>"
fleetly env list torchwood

# —— 可选 ——
fleetly env set torchwood TORCHWOOD_SECURITY_ENCRYPTION_KEY "$(openssl rand -hex 32)"
fleetly env set torchwood TORCHWOOD_SERVER_HTTP_CORS_ALLOW_HEADERS \
  "Content-Type,Authorization,X-Api-Key,X-Torchwood-Project,X-Request-Id"

# —— Config 资源（明文可回读；内容变更 = 新对象 + 引用服务滚动）——
fleetly configs set --from-file docker/fleetly/config.yaml           torchwood config.yaml
fleetly configs set --from-file docker/fleetly/bootstrap-runtime.sql torchwood bootstrap-runtime.sql
fleetly configs set --from-file docker/fleetly/bootstrap-roles.sql   torchwood bootstrap-roles.sql
fleetly configs ls torchwood
```

**机具令牌（scope `tasks,build`）铸造与注入**（dispatcher → fleetlyd）：

```bash
# 平台管理员执行；明文仅创建响应一次可见（服务端只存哈希）
fleetly tokens create --machine --scopes tasks,build \
  --note "torchwood dispatcher (fleetly Tasks/build)" --json
# 立即注入平台 env（不要落到仓库/对话/日志）：
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_TOKEN "<上一步输出的一次性明文>"
# 轮换：fleetly tokens revoke <id> → 重新 create → env set → 重新部署
```

**dokploy → fleetly 变量对照**（同键同名，无需改名）：`TORCHWOOD_SECURITY_JWT_SECRET`、
`TORCHWOOD_SECURITY_SETUP_TOKEN`、`TORCHWOOD_SERVER_HTTP_PUBLIC_URL`、
`TORCHWOOD_AUTH_PASSWORD`、`TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID/SECRET_ACCESS_KEY`、
`MINIO_ROOT_USER/PASSWORD`、`TORCHWOOD_FUNCTIONS_DISPATCHER_SHARED_TOKEN`、
`TORCHWOOD_FUNCTIONS_PACKER_SHARED_TOKEN`、`TORCHWOOD_FUNCTIONS_FLEETLY_*`
（T2-3 引入）、`TORCHWOOD_SECURITY_ENCRYPTION_KEY`。新增键仅
`TORCHWOOD_DATA_DATABASE_SOURCE`（#2 映射表）。

注意：

- **平台 env 是 app 级**（注入每个服务；见 §1 差异 4）。同键覆盖 compose 层并在
  部署输出 `W_ENV_PLATFORM_OVERRIDE` 警告（可见可查）；
- **首次部署的鸡与蛋**：应用随首次部署创建，而 env/Config/域名写入需要应用已存在
  → 首部署预期失败（config 前哨 `E_CONFIG_NOT_FOUND`），失败后按本清单写入，再部署
  即成功（runbook §2 有逐步命令与预期输出）；
- Config 面向**单文件**：`db/migrations` 目录不映射（OT-3 明文条款），迁移由
  migrate init job 从公开仓库钉 commit 拉取（§5）。

## 5. 镜像引用形态裁决（tag vs digest）

**裁决：应用镜像钉 sha tag `ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4`，不用
`latest`、不用裸 digest；migrate 的迁移源 ref 钉同一个 commit（`#78ea1a4`）。**
依据与操作口径：

- 平台已把「可变引用」收敛为部署期钉定（DT-2）：部署时经 registry API 把 tag
  解析为 digest、以 digest 进 revision spec 与漂移对账；**Redeploy 重解析 =
  `pull_policy: always` 的干净等价物**——compose 写 tag 不丢 reproducibility；
- **不用 `latest`**：Redeploy 会静默换版（现役 dokploy 的既有风险面），部署形态
  应显式表达「部署哪个版本」；
- **迁移源 = 应用镜像同 commit**：迁移文件在应用镜像里**不存在**（一手检查
  `ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4`：`/app` 仅 `configs/`，无
  `db/migrations`），从公开仓库 `github://torchwoodcloud/torchwood/db/migrations#<commit>`
  拉取；**升级纪律：改镜像 tag 时必须同改 migrate 的 `#ref`**（两处成对更新，
  迁移与代码同版本）；
- 割接锚点（2026-09-28 经 GHCR 查证）：`sha-78ea1a4`（= main tip，含 IMPL-T2-3）
  digest `sha256:fe5314d776163cd5ff366914de30c13307697474959d9e800a526ab901474e78`，
  与 `latest` 同 digest；
- 硬冻结备选：逐行替换为 `@sha256:fe53…`（免部署期解析、airgap 快路径）；
- 第三方镜像保留人读 tag（DT-2 部署期钉 digest）：`redis:7-alpine`、
  `pgsty/silo:RELEASE.2026-09-03T13-18-01Z`、`migrate/migrate:v4.18.1`、
  `percona/percona-distribution-postgresql:18`（与托管实例模板同发行版，作 psql
  客户端面）；
- 升级/回滚：改 compose 的 tag（+ migrate `#ref`）→ `fleetly deploy`；回滚 =
  改回旧 tag 重部署，或 `fleetly rollback torchwood`（revision 级，runbook §8）。

**迁移源的端到端形态（本地实证）**：`migrate/migrate:v4.18.1` 内置 `github`
source driver，匿名 GitHub API 有 **每 IP 60 req/h** 限额；首次全量迁移 ~13 次
请求、后续部署 ~1–2 次。runbook §0 有 rate-limit preflight，§9 有超限时的宿主机
`file://` 等价命令（仓库内有 `db/migrations`）。若日后成为经常性摩擦，建议改为
发布一个把 `db/migrations` 烘进镜像的 `torchwood-migrations`（CI 一处 Dockerfile
+ 一个 workflow 步骤）——本票未做（超出票面产出物，登记为建议）。

## 6. 割接步骤概要（详见 [cutover-runbook.md](cutover-runbook.md)）

| 步 | 内容 | 关键命令 |
|---|---|---|
| 0 | 前置检查 | `fleetly databases create --template percona-postgresql-18 torchwood-pg` → 等 ready；镜像/GitHub 配额/旧栈盘点/DNS TTL |
| B | 首部署（预期失败：config 前哨；只建 app） | `fleetly deploy docker/fleetly/docker-compose.yml` → `E_CONFIG_NOT_FOUND` |
| C | 平台 env + Config + 域名 + 机具令牌 | §4 清单 + §3 两条 `domains add` |
| D | 数据面准备（宿主机，栈内零服务占用） | 手动等价 init：migrate（`file://`）+ `bootstrap-runtime.sql` → `pg_dump`/`pg_restore --clean --if-exists --no-owner` → redis/minio 卷复制或明示重置 |
| E | 正式部署 | `fleetly deploy …` → init job 幂等重跑 → 长驻服务起 |
| F | DNS 切换 + 证书收敛 | `dig` 回切记录 → `fleetly domains set/verify` |
| G | 验收（含函数端到端） | runbook §6：健康门/数据面/函数（部署 zip→执行→回收）/域名 |
| H | 割接记录（数据决策二选一必填） | runbook §7 |
| I | 回滚（仅故障时） | runbook §8：dokploy 栈未拆 + 旧卷只读复制 |

## 7. 验收探针与诚实标注

验收探针的逐步命令与断言在 [cutover-runbook.md](cutover-runbook.md) §6：
`fleetly apps get`（六常驻健康）→ 数据面（`schema_migrations`/账号/业务行）→
函数端到端（`functions create` → `deployments create --code` → `executions create`
→ `fleetly tasks ls` 回收）→ `fleetly domains verify` + h2 握手 + gRPC 往返。

诚实标注（不隐藏的平台缺口/已知边界）：

- **percona 模板编码**：托管实例 `server_encoding=SQL_ASCII`（模板未带 initdb 编码
  参数），`client_encoding` 兜底后 torchwood 可正常连接（本地实证），但字符函数语义
  按字节计；彻底修复 = 平台模板补 `--encoding=UTF8`（建议，runbook §9）；
- **worker 健康门 = 进程存活**（`kill -0 1`，PID-1 存活、两形态通用）：镜像无 HTTP 健康面，队列消费
  深度检查靠 runbook §6 验收探针；
- **编排自愈窗口**：同 §1 差异 3；`depends_on` 硬拒是平台语义（T2-0④ spike 结论）；
- **平台 env app 级**：同 §1 差异 4（密钥在全部服务容器内可见）；
- **迁移源 = 匿名 GitHub API**：60 req/h/IP 限额与兜底路径见 §5 与 runbook §9；
- **dispatcher → fleetlyd 传输模式双形态**（2026-09-29 起 endpoint scheme 显式
  选择，gRPC 社区约定 grpc/grpcs）：明文（裸 host:port / `grpc://`，栈内通路
  兼容）与 TLS（`grpcs://`，`?insecure=true` 跳过校验、`?server_name=` 覆盖
  SNI，直连 TLS 化控制面，无需外置明文桥/明文监听）都要支持——fleetlyd 的 grpc-go
  TLS 服务端要求 ALPN h2，任何不支持 ALPN 的外置桥（如 socat）都无法中继
  （2026-09-29 staging 实证）；平台物化的 `FLEETLY_CONTROL_GRPC_ADDR` 是
  端点缺席时的零配置回落；
- **DT-10 必填 env preflight 未实现**：漏配 env 的部署靠应用侧 fail-closed
  （表现是 crash-loop 噪音而非发布期点名拒绝）；
- **无 80→443 重定向 / 无宿主管端口**：同 §1 差异 1/2。

## 8. 日常运维（fleetly 形态）

| 操作 | 做法 |
|---|---|
| 升级 | 改 `docker-compose.yml` 镜像 tag + migrate `#ref`（同 commit）→ `fleetly deploy docker/fleetly/docker-compose.yml` |
| 回滚 | `fleetly rollback torchwood`（revision 级；迁移前向不回退）或改回旧 tag 重部署；dokploy 栈未拆 = 终极回滚 |
| 看状态 | `fleetly apps get torchwood` / `fleetly deployments list torchwood` / `fleetly placement show torchwood`（卷与节点） |
| 看日志 | `fleetly logs history --limit 100 torchwood`（服务归因）；init job 日志同归因（`release.job_*` 事件点名） |
| 数据库 | `fleetly databases show torchwood-pg` / `backup` / `backups` / `restore --snapshot <id> --confirm torchwood-pg torchwood-pg`（E4 生命周期与备份；restore 的 `--confirm` 带实例名做两段确认，位置参数实例名仍需尾随） |
| 密钥轮换 | `TORCHWOOD_AUTH_PASSWORD`：`env set` 新值 → 重部署（db-bootstrap 作业重写数据库侧口令 + DSN 同步换）；`JWT_SECRET`：`env set` → 重部署（roles-sig 自动重落库，双钥窗口旧 sig 不降级） |
| 函数底座 | dispatcher 常驻；镜像映射在 Redis（`torchwood:fnimg:*`，随 redis 卷持久化）；实例 = fleetly Tasks，`fleetly tasks ls` 可见 |
| 备份 | postgres 归平台（`fleetly databases backup`）；redis/minio 卷为残余栈内数据（DT-8 backlog：手动/Tasks BGSAVE），搬运步骤见 runbook §3 |

## 9. 文件清单

| 文件 | 用途 |
|---|---|
| `docker-compose.yml` | fleetly 受控子集编排（六常驻 + 三 init job；postgres 不在栈内） |
| `config.yaml` | Config 资源上传源（server/worker/dispatcher 的 `/app/configs/config.yaml`） |
| `bootstrap-runtime.sql` | db-bootstrap 作业：连接编码兜底 + `tw_authenticator` 创建/改密 |
| `bootstrap-roles.sql` | db-bootstrap 作业：三角色授权补齐（与 dokploy 同内容副本） |
| `cutover-runbook.md` | 割接 runbook（前置/两阶段 bootstrap/DT-8 数据面/函数端到端验收/回滚；真机段标注待窗口） |
| `README.md` | 本指南（改写清单、域名/env/Config/令牌、镜像裁决、诚实标注） |
| `../dokploy/` | 现役 Dokploy 形态（割接验收前并行保留，回滚路径） |
