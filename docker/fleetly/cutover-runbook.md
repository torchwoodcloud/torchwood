# Torchwood 栈割接 runbook（dokploy → fleetly）

| 状态 | 日期 | 关联 |
|---|---|---|
| **待使用者执行窗口**（本环境无 staging 凭据/访问权，真机步骤未执行、未虚构结果；本地等价实证见附录 A） | 2026-09-28 | [README](README.md)（部署形态/改写清单/镜像裁决/env 与 Config 清单）；[Dokploy 形态基准](../dokploy/README.md) |
| 勘误+竞态注记（CLI 旗标前置纪律勘误 8 处；首发竞态两处入册——2026-09-29 staging 真机实证回填） | 2026-09-30 | fleetly CLI 为 Go flag 包：旗标一律在位置参数前，`cmd <app> --flag x` 形态实际解析失败 |

范围：把现役 Dokploy 栈（postgres + redis + minio + server + worker + dispatcher +
packer + migrate/db-grants/roles-sig）割接到 `docker/fleetly/` 的 fleetly 部署形态
（postgres 落托管实例；redis/minio 留栈内卷；三个一次性作业改 init job）。
**dokploy 栈并行保留至验收**，回滚 = 旧栈未拆（§8）。以下命令中 `<...>` 为占位符，
按实际环境替换；凭据只经环境变量传递，不进对话/日志/仓库。

前置阅读：[`README.md`](README.md) §2（改写清单）、§3（域名）、§4（env/Config/令牌）、
§5（镜像与迁移源裁决）。

执行假设（v0.1 单机同宿主语义，fleetly 文档同口径）：§0.4/§3 的 `docker` 命令在
**fleetlyd 所在宿主机**执行（dokploy 与 fleetly 同机；若分离，请把 §3 的数据搬运
拆到两台机器的对应侧，命令形态不变），且**工作目录 = torchwood 仓库根**
（§3 的 `-v "$PWD/db/migrations"` 等挂载依赖本仓 checkout）。§0–§8 假定在同一
shell 会话内执行（§3 定义的辅助函数与变量在后文复用）。

## 0. 前置检查（窗口前完成）

```bash
# 0.1 控制面凭据与项目上下文（真机窗口由使用者提供）
export FLEETLY_ADDR="<fleetlyd gRPC 地址>"          # 等价 --addr
export FLEETLY_TOKEN="<API token>"                  # 等价 --token；或 fleetly auth login
export FLEETLY_PROJECT="<team>/<prj>"               # 项目上下文（裸名或 team/prj）
# 控制面 TLS 时：export FLEETLY_TLS=true（或 --tls / --tls-insecure）

fleetly apps list                                   # 控制面可达；空列表合法
fleetly validate docker/fleetly/docker-compose.yml  # 文本形态应与本次仓库版本一致
# 预期：torchwood: valid (spec_hash …, 9 services, 2 volumes)，零告警

# 0.2 托管数据库实例（必须先于部署创建：compose 的 fleetly.databases label 要求实例存在）
# 实例名 torchwood-pg 是 compose/runbook 的契约名：改名需同步改 compose 的
# fleetly.databases label 与全部 FLEETLY_DB_TORCHWOOD_PG_* 引用（含本 runbook 命令）
fleetly databases create --template percona-postgresql-18 torchwood-pg
fleetly databases show torchwood-pg                  # 轮询到 status=ready
fleetly databases reveal torchwood-pg                # 记录 owner（fleetly）口令到环境变量
export OWNER_PW="<上一步 reveal 输出的 password>"
export OWNER_DSN_BASE="postgres://fleetly:${OWNER_PW}@torchwood-pg:5432/torchwood_pg"

# 库共享网络（别名 = 实例名；platform 在实例 provision 时创建）
export DB_NET="$(docker network ls --format '{{.Name}}' | grep '^fleetly-db-.*-torchwood-pg-net$' | head -n 1)"
echo "$DB_NET"
test -n "$DB_NET"

# 0.3 镜像与迁移源（两处必须成对：同一个 commit）
export TORCHWOOD_COMMIT="78ea1a4"
docker manifest inspect "ghcr.io/torchwoodcloud/torchwood:sha-${TORCHWOOD_COMMIT}" >/dev/null
# GitHub API 配额（匿名 60 req/h/IP；首次全量迁移 ~13 次请求）
curl -sS "https://api.github.com/rate_limit" | grep -E '"remaining"' | head -n 2
# 预期 core.remaining ≥ 30；不足时按 §9「迁移源限额兜底」预跑宿主机 file:// 迁移

# 0.4 旧栈盘点（在 dokploy 宿主机；容器名以 docker ps 实际为准）
export OLD_PG="<dokploy postgres 容器名>"
export OLD_REDIS="<dokploy redis 容器名>"
export OLD_MINIO="<dokploy minio 容器名>"
docker ps --format '{{.Names}} {{.Image}}' | grep -E 'torchwood|dokploy'
docker exec "$OLD_PG" psql -U torchwood -d torchwood -t -A -c \
  "SELECT 'migrations='||version||' dirty='||dirty FROM schema_migrations;"
# 预期 migrations=12 dirty=f（与 §0.3 的 commit 同版本；不一致先升级旧栈再割接）
docker exec "$OLD_PG" psql -U torchwood -d torchwood -t -A -c \
  "SELECT 'db_size='||pg_size_pretty(pg_database_size('torchwood'));"
docker exec "$OLD_PG" psql -U torchwood -d torchwood -t -A -c \
  "SELECT 'schemas='||count(*) FROM pg_namespace WHERE nspname LIKE 'tw%';"
docker exec "$OLD_REDIS" redis-cli DBSIZE
docker exec "$OLD_REDIS" redis-cli --scan --pattern 'torchwood:*' | head -n 5
docker exec "$OLD_MINIO" du -sh /data

# 0.5 DNS：两个生产域名 TTL 预先调低（建议 ≤300s，切割前 ≥1 个 TTL 提前做）；
#     记录当前 A/AAAA（回滚回切要用）
dig +short "<http-host>"
dig +short "<grpc-host>"

# 0.6 宿主机临时目录与辅助函数（空间 ≥ 2× 旧库 dump + minio 数据）
export DOCKER_TMP="/var/tmp/torchwood-cutover"
mkdir -p "$DOCKER_TMP"
df -h "$DOCKER_TMP"

# psql 辅助：psql 客户端 = percona 镜像（与实例同发行版），凭据经 env 不进 argv
psql_host() {
  docker run --rm -i --network "$DB_NET" \
    -e PGHOST="torchwood-pg" -e PGPORT="5432" -e PGUSER="fleetly" \
    -e PGPASSWORD="$OWNER_PW" -e PGDATABASE="torchwood_pg" -e PGSSLMODE="disable" \
    --entrypoint psql percona/percona-distribution-postgresql:18 -t -A -f -
}
```

## 1. 作业顺序总览

| 步 | 章节 | 内容 | 真机窗口 |
|---|---|---|---|
| B | §2 | 首部署（预期失败：Config 前哨；只建 app 行） | 是 |
| C | §2 | 平台 env + Config + 域名 + 机具令牌 | 是 |
| D | §3 | DT-8 数据面（旧栈冻结 → postgres dump/restore；redis/minio 回灌或明示清零） | 是 |
| E | §4 | 正式部署（init job 幂等重跑 → 六常驻起） | 是 |
| F | §5 | DNS 切换 + 证书收敛 | 是 |
| G | §6 | 验收探针（健康/数据面/函数端到端/域名） | 是 |
| H | §7 | 割接记录（数据决策二选一必填） | 是 |
| I | §8 | 回滚预案（仅故障时执行） | 按需 |

## 2. 两阶段 bootstrap（首次失败是预期行为）

**为什么两阶段**：应用随首次部署创建，而 `env set` / `configs set` / `domains add`
/ 机具令牌注入都要求应用已存在。首部署会在 preparing 前哨以
`E_CONFIG_NOT_FOUND` 失败（三个 Config 尚未上传）——这一步的净效果 = **创建 app
行**，不创建任何服务、不跑 init job、不触碰数据库。

```bash
# B. 首部署（预期失败）
fleetly deploy docker/fleetly/docker-compose.yml
# 预期：deployment … failed  error=E_CONFIG_NOT_FOUND（点名 config.yaml /
#       bootstrap-runtime.sql / bootstrap-roles.sql）
fleetly deployments list torchwood
fleetly apps get torchwood        # app 已存在，derived_state=down；无服务被创建

# C. 平台 env（README §4 全量；此处可直接跑）
export TW_AUTH_PW="$(openssl rand -hex 24)"
export TW_JWT_SECRET="$(openssl rand -hex 32)"
export TW_SETUP_TOKEN="$(openssl rand -hex 32)"
export TW_DISPATCHER_TOKEN="$(openssl rand -hex 32)"
export TW_PACKER_TOKEN="$(openssl rand -hex 32)"
fleetly env set torchwood TORCHWOOD_SECURITY_JWT_SECRET   "$TW_JWT_SECRET"
fleetly env set torchwood TORCHWOOD_SECURITY_SETUP_TOKEN  "$TW_SETUP_TOKEN"
fleetly env set torchwood TORCHWOOD_SERVER_HTTP_PUBLIC_URL "https://<http-host>"
fleetly env set torchwood TORCHWOOD_AUTH_PASSWORD "$TW_AUTH_PW"
fleetly env set torchwood TORCHWOOD_DATA_DATABASE_SOURCE \
  "postgres://tw_authenticator:${TW_AUTH_PW}@torchwood-pg:5432/torchwood_pg?sslmode=disable"
fleetly env set torchwood TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID     "<MINIO_ROOT_USER>"
fleetly env set torchwood TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY "<MINIO_ROOT_PASSWORD>"
fleetly env set torchwood MINIO_ROOT_USER     "<MINIO_ROOT_USER>"
fleetly env set torchwood MINIO_ROOT_PASSWORD "<MINIO_ROOT_PASSWORD>"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_DISPATCHER_SHARED_TOKEN "$TW_DISPATCHER_TOKEN"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_PACKER_SHARED_TOKEN     "$TW_PACKER_TOKEN"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_ENDPOINT "<fleetlyd gRPC 容器可达端点>"
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_TOKEN    "<机具令牌，见下>"
fleetly env list torchwood        # 全清单在场（status=pending，随下次部署生效）

# 机具令牌（scope tasks,build；明文只回一次）
fleetly tokens create --machine --scopes tasks,build \
  --note "torchwood dispatcher (fleetly Tasks/build)" --json
fleetly env set torchwood TORCHWOOD_FUNCTIONS_FLEETLY_TOKEN "<上一步一次性明文>"

# Config 资源（内容源 = 仓库 docker/fleetly/）
fleetly configs set --from-file docker/fleetly/config.yaml           torchwood config.yaml
fleetly configs set --from-file docker/fleetly/bootstrap-runtime.sql torchwood bootstrap-runtime.sql
fleetly configs set --from-file docker/fleetly/bootstrap-roles.sql   torchwood bootstrap-roles.sql
fleetly configs ls torchwood

# 域名（两条：9080 http + 9060 h2c；可提前声明，DNS 切换在 §5）
fleetly domains add --service server --port 9080 --protocol http torchwood "<http-host>"
fleetly domains add --service server --port 9060 --protocol h2c  torchwood "<grpc-host>"
fleetly domains list torchwood
```

## 3. DT-8 数据面（二选一，不许默认静默）

### 3.0 栈内零服务占用确认

```bash
fleetly apps get torchwood     # derived_state=down（首部署在 preparing 失败，零服务）
docker ps --format '{{.Names}}' | grep 'fleetly-.*-torchwood' || true
# 预期无输出（全栈 0 任务）
```

### 3.1 postgres：dump/restore 进托管实例

顺序 = 目标库预置（手动等价 init，仓库内文件直挂，免 GitHub 配额）→ 旧库 dump →
`pg_restore --clean`。本地等价实证（含 ACL/数据/版本断言）见附录 A。

```bash
# 3.1.0 旧栈冻结写入（保留 postgres/redis/minio 在线；停应用面容器）
docker stop "<dokploy worker 容器>" "<dokploy dispatcher 容器>" "<dokploy server 容器>"

# 3.1.1 旧库 dump（容器内 → 宿主机；默认 POSTGRES_USER/POSTGRES_DB=torchwood，按实际替换）
docker exec "$OLD_PG" pg_dump -Fc -U torchwood -d torchwood -f /tmp/tw-cutover.dump
docker cp "$OLD_PG":/tmp/tw-cutover.dump "$DOCKER_TMP/tw-cutover.dump"
ls -la "$DOCKER_TMP/tw-cutover.dump"

# 3.1.2 目标库预置①：迁移（file:// 源 = 仓库 db/migrations；与 §0.3 commit 同内容）
docker run --rm --network "$DB_NET" \
  -v "$PWD/db/migrations:/migrations:ro" \
  migrate/migrate:v4.18.1 \
  -path=/migrations \
  -database "${OWNER_DSN_BASE}?sslmode=disable" \
  up

# 3.1.3 目标库预置②：运行账号 + 连接编码（bootstrap-runtime.sql）
docker run --rm --network "$DB_NET" \
  -e DSN="$OWNER_DSN_BASE" \
  -e TORCHWOOD_AUTH_PASSWORD="$TW_AUTH_PW" \
  -v "$PWD/docker/fleetly:/etc/torchwood:ro" \
  --entrypoint sh \
  percona/percona-distribution-postgresql:18 \
  -c 'psql "${DSN}?sslmode=disable" -v ON_ERROR_STOP=1 -v dbname=torchwood_pg -v auth_password="$TORCHWOOD_AUTH_PASSWORD" -f /etc/torchwood/bootstrap-runtime.sql'

# 3.1.4 数据搬运：全量恢复
#   --clean   ：以旧库对象覆盖迁移产物（两库同为 schema 版本 12，对象集一致）
#   --if-exists：DROP 不存在对象时跳过（首跑空库合法）
#   --no-owner：旧对象属主（torchwood 角色）不存在于新集群，属主归恢复用户 fleetly
#   ACL 保留（引用 tw_owner/tw_app/tw_system/tw_authenticator——均由 3.1.2/3.1.3 创建）
docker run --rm --network "$DB_NET" \
  -e DSN="$OWNER_DSN_BASE" \
  -v "$DOCKER_TMP:/dump:ro" \
  --entrypoint sh \
  percona/percona-distribution-postgresql:18 \
  -c 'pg_restore --clean --if-exists --no-owner -d "${DSN}?sslmode=disable" /dump/tw-cutover.dump'

# 3.1.5 恢复后校验（三条决定性断言）
echo "SELECT 'migrations='||version||' dirty='||dirty FROM schema_migrations;" | psql_host
echo "SELECT 'projects='||count(*) FROM public.projects;" | psql_host
echo "SELECT 'schemas='||count(*) FROM pg_namespace WHERE nspname LIKE 'tw%';" | psql_host
echo "SELECT 'runtime_priv='||has_table_privilege('tw_authenticator','public.admins','SELECT')::text;" | psql_host
# 预期：migrations=12 dirty=f；projects/schemas 数与 §0.4 旧库一致；runtime_priv=true
```

> **为什么先迁移再恢复**：`pg_dump` 不含全局角色对象；恢复的 ACL 引用
> `tw_owner/tw_app/tw_system/tw_authenticator`——三角色由迁移 000004 创建、
> 运行账号由 3.1.3 创建，全部就位后 ACL 才能落上（本地实证：缺角色时
> `pg_restore` 报 "role … does not exist"）。**`ALTER DATABASE … SET
> client_encoding='UTF8'` 是数据库级设置，不随对象恢复丢失**。

### 3.2 redis 卷：回灌（推荐）或明示清零

```bash
# 3.2.0 旧 redis 基线（割接记录对照用）
docker exec "$OLD_REDIS" redis-cli DBSIZE
docker exec "$OLD_REDIS" redis-cli --scan --pattern 'torchwood:*' | head -n 5

# 3.2.1 预建平台命名的目标卷（平台卷无 label、按命名约定归属；Swarm 按名复用同名卷）
export APP_ID="$(fleetly apps get --json torchwood | jq -r .id)"   # 无 jq 时从裸输出 "id: <…>" 行读取
echo "$APP_ID"
test -n "$APP_ID"
export APP_ID8="$(printf '%s' "$APP_ID" | cut -c1-8)"
export NEW_REDIS_VOLUME="fleetly-torchwood-redis_data-${APP_ID8}"
export NEW_MINIO_VOLUME="fleetly-torchwood-minio_data-${APP_ID8}"
docker volume create "$NEW_REDIS_VOLUME"
docker volume create "$NEW_MINIO_VOLUME"

# 3.2.2 冻结旧 redis 写入并压缩 AOF（redis 可在线；应用面容器已在 3.1.0 停）
docker exec "$OLD_REDIS" redis-cli BGREWRITEAOF
docker exec "$OLD_REDIS" redis-cli INFO persistence | grep -E 'aof_rewrite_in_progress|aof_last_bgrewrite_status'
# 重复 BGREWRITEAOF/INFO，直到 in_progress:0 且 status:ok

# 3.2.3 复制旧 /data（只读复制，不改旧卷）→ 灌入目标卷
rm -rf "$DOCKER_TMP/redis-data"
docker cp "$OLD_REDIS":/data "$DOCKER_TMP/redis-data"
ls -la "$DOCKER_TMP/redis-data" "$DOCKER_TMP/redis-data/appendonlydir"
docker ps -a --filter volume="$NEW_REDIS_VOLUME" --format '{{.Names}} {{.Status}}'
# 预期无输出（无任务占用）
docker run --rm -v "$NEW_REDIS_VOLUME":/data -v "$DOCKER_TMP/redis-data":/src:ro redis:7-alpine \
  sh -c 'rm -rf /data/appendonlydir /data/dump.rdb && cp -a /src/. /data/ && chown -R redis:redis /data && ls -la /data'
```

> **为什么整目录复制**：Redis 7 在 `--appendonly yes` 下若发现 `appendonlydir`
> 就只认 AOF、**不加载 `dump.rdb`**——只放 RDB 会静默空库（T1-5 本地实证）。
> 回灌决定必须写进 §7 割接记录；§6.3 的 DBSIZE/键样本比对承担「回灌成功」判据。

**明示清零（备选）**：跳过 3.2.2/3.2.3（预建卷保持空）。代价 = 已登录会话的
refresh 轮换记录与函数镜像映射（`torchwood:fnimg:*`）丢失（用户重新登录；函数
部署在下次执行时自动重建，桶副本在 minio 卷内）。**选择清零必须在 §7 写明**。

### 3.3 minio 卷：回灌（推荐）或明示清零

```bash
# 3.3.1 旧 minio 基线（割接记录对照用）
docker exec "$OLD_MINIO" du -sh /data
docker exec "$OLD_MINIO" sh -c 'ls -1 /data'
# 预期含 torchwood-storage / torchwood-functions 两个桶目录

# 3.3.2 停旧 minio（停写，保证 /data 一致）→ 复制 → 灌入目标卷
docker stop "$OLD_MINIO"
rm -rf "$DOCKER_TMP/minio-data"
docker cp "$OLD_MINIO":/data "$DOCKER_TMP/minio-data"
du -sh "$DOCKER_TMP/minio-data"
docker run --rm -v "$NEW_MINIO_VOLUME":/data -v "$DOCKER_TMP/minio-data":/src:ro \
  --entrypoint sh pgsty/silo:RELEASE.2026-09-03T13-18-01Z \
  -c 'cp -a /src/. /data/ && ls -1 /data'
# 预期输出与 3.3.1 的桶列表一致（silo 容器默认 root 运行，属主无需改）
```

**明示清零（备选）**：跳过 3.3.2。代价 = 全部用户对象与函数部署代码包清零
（函数需重新部署）。**必须在 §7 写明**。

## 4. 正式部署

```bash
fleetly deploy --timeout 20m docker/fleetly/docker-compose.yml
# 预期：init job 三条并行执行（migrate=no change；db-bootstrap 幂等重授权；
#       roles-sig 重落钥）→ 六常驻服务创建 → 健康门通过 → deployment succeeded
fleetly apps get torchwood               # derived_state=running
fleetly logs history --limit 100 torchwood
```

> 若 init job 失败：`release.job_failed` 事件点名作业与原因；常见三类——(1) GitHub
> 配额（§9 兜底）；(2) 缺必填平台 env（作业 fail-closed 的 echo 点名键名）；
> (3) **首发竞态（已知一次性，无需人工干预数据库）**：init job 与常驻服务并行
> 启动，首窗 DB 角色/编码尚未就绪时 db-bootstrap/roles-sig 的有界等待环
> （240s）可能输给竞态 → job 失败。处置 = 等托管库状态收敛（ready）后
> 重新 `deploy`——init job 全幂等，二次跑秒级通过（2026-09-29 staging 真机
> 实证：首发失败 → redeploy 即绿，init 4s）。

## 5. 域名声明与 DNS 切换

```bash
# 5.1 域名资源已在 §2 C 声明；幂等核对
fleetly domains list torchwood

# 5.2 DNS 切换（DNS 服务商控制台；把两域名指向 fleetly 边缘地址）
dig +short "<http-host>"
dig +short "<grpc-host>"

# 5.3 触发入口收敛 + 证书签发（ACME HTTP-01 要求解析已指向 fleetly 边缘）
fleetly domains set torchwood "<http-host>"    # 空 flag = 保持现值，仅触发收敛
fleetly domains set torchwood "<grpc-host>"
fleetly domains verify torchwood               # 期望：解析、:80、:443、证书 SAN/有效期正常
```

> 已知行为差异：平台 **不做 80→443 重定向**（客户端直接使用 `https://`）；
> 证书签发依赖 DNS 已切至 fleetly 边缘，切换前 443 可能无证书（可用
> `--resolve` 预探后端）。

## 6. 验收探针（全部通过才算割接完成）

### 6.1 全栈健康

```bash
fleetly apps get torchwood
fleetly deployments list torchwood | head -n 5
docker ps --format '{{.Names}} {{.Status}}' | grep 'fleetly-.*-torchwood'
# 预期六个常驻服务任务 running/healthy（redis/minio/server/worker/dispatcher/packer）
fleetly logs history --service server --limit 50 torchwood
fleetly logs history --service worker --limit 50 torchwood
fleetly logs history --service dispatcher --limit 50 torchwood
```

### 6.2 数据面（postgres；决定性）

```bash
echo "SELECT 'migrations='||version||' dirty='||dirty FROM schema_migrations;" | psql_host
echo "SELECT 'projects='||count(*) FROM public.projects;" | psql_host
echo "SELECT 'admins='||count(*) FROM public.admins;" | psql_host
echo "SELECT 'runtime_priv='||has_table_privilege('tw_authenticator','public.admins','SELECT')::text;" | psql_host
# 运行态账号连通（真实 pgdriver 客户端 + client_encoding 兜底 + 角色 membership）
export RUNTIME_DSN_BASE="postgres://tw_authenticator:${TW_AUTH_PW}@torchwood-pg:5432/torchwood_pg"
docker run --rm --network "$DB_NET" -e DSN="$RUNTIME_DSN_BASE" \
  --entrypoint sh ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4 \
  -c 'exec /usr/local/bin/torchwood admin schema repair --dry-run --dsn "${DSN}?sslmode=disable"'
# 预期 exit 0（本地等价实证见附录 A）
```

### 6.3 数据面（redis / minio）

```bash
export NEW_REDIS="$(docker ps --format '{{.Names}}' | grep 'fleetly-.*-torchwood-redis' | head -n 1)"
echo "$NEW_REDIS"
docker exec "$NEW_REDIS" redis-cli DBSIZE                       # 回灌：与 §3.2.0 记录一致（有流量时 ≥）
docker exec "$NEW_REDIS" redis-cli --scan --pattern 'torchwood:fnimg:*' | head -n 5
export NEW_MINIO="$(docker ps --format '{{.Names}}' | grep 'fleetly-.*-torchwood-minio' | head -n 1)"
echo "$NEW_MINIO"
docker exec "$NEW_MINIO" sh -c 'ls -1 /data && du -sh /data'     # 回灌：与 §3.3.1 桶列表一致
# 明示清零的判据：DBSIZE=0 / 桶目录为空，且 §7 已写明
```

### 6.4 函数端到端（部署 zip → 执行 → 回收）

```bash
# 前置：仓库根 `mise run build` 生成 ./bin/torchwood（或使用已安装的 torchwood CLI）；
#       Torchwood Console 建一个项目 API Key（functions.write 权限），并导出
export TW_ENDPOINT="<grpc-host>:443"
export TW_API_KEY="<sk-...>"

# 6.4.1 最小 node 函数包（CJS：exports.main；runtime id 取 functions runtimes 缺省项）
mkdir -p /tmp/t24-fn
cat > /tmp/t24-fn/index.js <<'JS'
exports.main = async (data, ctx) => ({ ok: true, echo: data, via: "fleetly-tasks" });
JS
( cd /tmp/t24-fn && zip -q -r ../t24-fn.zip . )
ls -la /tmp/t24-fn.zip

# 6.4.2 创建函数 + 池策略（单请求自退 + 短 idle TTL，便于观察回收）
./bin/torchwood functions create --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  --id t24-cutover-probe --name t24-cutover-probe --runtime "node-24.0" --enabled=true
./bin/torchwood functions update --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  --max-requests-per-instance 1 --idle-ttl-seconds 30 t24-cutover-probe
./bin/torchwood functions runtimes --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" | head -n 20

# 6.4.3 部署 zip（走 dispatcher → fleetly BuildFromUpload → 平台 zot）
./bin/torchwood functions deployments create --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  --code /tmp/t24-fn.zip t24-cutover-probe
./bin/torchwood functions deployments list --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  t24-cutover-probe
# 轮询到最新 deployment status=ready；失败时看 dispatcher 日志（§6.1）
# 注：首个函数的首次部署/执行可能撞「挂靠竞态」一次性失败（CLI 见
#     `Post …/v1/dispatch/builds: EOF`，而 fleetlyd 侧 build 实际 succeeded
#     ——挂靠滚动把在途请求的 dispatcher 容器换掉了）；挂靠一次完成后
#     重试即绿，详见 §9。

# 6.4.4 执行（spawn → health 握手 → 分发）
./bin/torchwood functions executions create --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  --input '{"probe":"t24"}' t24-cutover-probe
./bin/torchwood functions executions list --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  t24-cutover-probe | head -n 20
# 预期 status=completed，输出含 {"ok":true,"echo":{"probe":"t24"}}

# 6.4.5 平台侧回收观测（函数实例 = fleetly Tasks；max_requests=1 → 自退 → 平台回收）
fleetly tasks ls
fleetly tasks ls --all
# 预期：任务先 running（或 stopped）后消失/终态；TTL 兜底 86400s（T2-3）
# 首次函数操作还会触发任务网成员挂靠（EnsureTaskNetwork → 平台重部署 torchwood：
# server/dispatcher 挂 fleetly-taskgroup-* 网，别名 torchwood-server），
# 会有一次服务滚动 —— 属预期（runbook §9）。

# 6.4.6 清理探针函数
./bin/torchwood functions deployments list --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  t24-cutover-probe
./bin/torchwood functions deployments delete --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  t24-cutover-probe "<deployment-id>"
./bin/torchwood functions delete --endpoint "$TW_ENDPOINT" --tls --api-key "$TW_API_KEY" \
  t24-cutover-probe
```

### 6.5 域名与传输连通

```bash
fleetly domains verify torchwood
# ① HTTP（Console/REST/healthz）：TLS 域名 + 9080 后端
curl -sS -o /dev/null -w '%{http_code}\n' --resolve "<http-host>:443:<fleetly 边缘 IP>" \
  "https://<http-host>/healthz/readiness"
# 预期 200
# ② gRPC：TLS 域名 + h2c 后端（ALPN=h2）
echo | openssl s_client -connect "<fleetly 边缘 IP>:443" -servername "<grpc-host>" -alpn h2 2>&1 \
  | grep -E 'ALPN|Verify return code'
# ③ gRPC 功能往返（无 API key 的健康检查；需要 gRPC 域名公网可达）
./bin/torchwood health get --endpoint "$TW_ENDPOINT" --tls
```

## 7. 割接记录（必填；数据决策三栏 + 证据）

| 项 | 值 |
|---|---|
| 割接窗口 | `<起止时间>` |
| 镜像/迁移源 | `ghcr.io/torchwoodcloud/torchwood:sha-<commit>` / `github://…db/migrations#<commit>`（或 §9 兜底的 file://） |
| postgres 数据 | ☐ dump/restore（§3.1） ☐ 重建空库（仅测试） |
| 旧库 → 新库 | migrations `<12|f>` → `<12|f>`；projects `<n>` → `<n>`；schemas `<n>` → `<n>` |
| redis 数据 | ☐ 回灌（§3.2） ☐ 明示清零（DBSIZE `<n>` → `<n>`） |
| minio 数据 | ☐ 回灌（§3.3） ☐ 明示清零（桶列表 `<...>` → `<...>`） |
| init job 结果 | migrate / db-bootstrap / roles-sig = `<ok/failed + 事件摘要>` |
| 函数端到端 | deployment `<id/ready>`；execution `<id/completed>`；任务回收 `<observed>` |
| 域名与证书 | `<domains verify 摘要>` |
| 回滚点 | dokploy 栈未拆；旧卷只读复制；DNS 回切值 `<...>` |

## 8. 回滚（dokploy 栈未拆）

```bash
# 触发条件：§6 任一探针失败，或切换后业务验收不通过。
# 8.1 启动旧栈（旧 redis/minio 数据未被修改：§3 只读复制）
docker start "<dokploy server 容器>" "<dokploy worker 容器>" "<dokploy dispatcher 容器>"
docker start "$OLD_MINIO"
# 8.2 DNS 回切到 dokploy 边缘（§0.5 记录的 A/AAAA）
# 8.3 新栈处置（可选，保留现场以便复盘）：停止在途部署或保持原样即可；
#     新栈域名资源保留（再次割接可复用，domains set 触发收敛）
fleetly deployments list torchwood
fleetly rollback torchwood        # 新栈已起过版本时的 revision 级回滚
```

## 9. 待使用者执行窗口项与已知风险（如实标注）

- 真机凭据/地址（`FLEETLY_ADDR/TOKEN/PROJECT`、`MINIO_ROOT_*`、项目 API Key）；
- §0.2 托管实例创建与 reveal；§3 dump/restore、卷搬运；§5 DNS 切换 + ACME
  HTTP-01 签发；§6.4 函数端到端与任务回收；§6.5 gRPC h2c 连通；
- **迁移源限额兜底**：`migrate` init job 走匿名 GitHub API（60 req/h/IP）。
  窗口前用 §0.3 检查配额；若部署时命中 403，用 §3.1.2 的宿主机 `file://`
  命令预跑（挂仓库 `db/migrations`），随后重新部署（migrate 变为 no change，
  仅 ~2 次 API 调用）。长期方案建议见 README §5；
- **dispatcher → fleetlyd 端点前置**：`TORCHWOOD_FUNCTIONS_FLEETLY_ENDPOINT`
  必须容器内可达，且与 T2-3 的明文 gRPC 客户端兼容（T2-3 已登记
  `dispatcher→fleetlyd 暂明文`）。**staging 控制面若开 TLS，需先落 TLS 票或
  暴露一个明文监听（绑定地址可达容器网段）；未就绪则 dispatcher fail-closed、
  发布失败**——这是本割接的硬前置，须在窗口前确认；
- **任务网成员挂靠的服务滚动**：首次函数操作触发 `EnsureTaskNetwork`
  （members=dispatcher,server）→ 平台重部署 torchwood 挂 `fleetly-taskgroup-*`
  网（别名 `torchwood-server`）；滚动期间函数回访可能短暂失败（自愈合）；
  **一次性竞态形态（2026-09-29 staging 实证）**：若挂靠滚动发生时
  dispatcher 恰有在途构建请求，滚动会把该 dispatcher 容器换掉——CLI 侧见
  `Post …/v1/dispatch/builds: EOF` 而 fleetlyd 侧 build 已 succeeded（结果不丢，
  只是连接被掐）；挂靠一次完成后重试即绿，无需处置；
- **mlbridge 切项目内网**（messageloop 侧单 env 改动）：确认 messageloop 与
  torchwood 同项目后：
  `fleetly projects network attach messageloop` / `fleetly projects network attach torchwood`
  → `fleetly env set messageloop MLBRIDGE_TORCHWOOD_BASE_URL "http://torchwood-server:9080"`
  → 重新部署 messageloop（messageloop 仓 `docker/fleetly/README.md` §4 的占位即此用途）；
- **percona 托管模板编码**：`bootstrap-runtime.sql` 已兜底 `client_encoding`，
  但 `server_encoding=SQL_ASCII`（字符函数按字节计）。彻底修复 = 平台模板补
  `POSTGRES_INITDB_ARGS=--locale=C --encoding=UTF8` 并重建实例（fleetly 侧改动，
  建议随 T3 小票）；届时 `bootstrap-runtime.sql` 的 ① 段可保留（幂等无害）；
- **平台 env app 级**：密钥注入全部服务（README §7 诚实标注）；
- **DT-10 必填 env preflight 未实现**：本 runbook 的 §0/§2 清单即人工替代。

---

## 附录 A：本地等价实证（本环境无 staging 凭据；以下为可复跑的一手证据）

环境：本机 Docker 29.7.2，percona/percona-distribution-postgresql:18 +
migrate/migrate:v4.18.1 + ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4（真实镜像）。

```text
# ① 迁移源（github:// 钉 commit）对 fresh percona PG18 全量应用（12 个版本）
$ docker run --rm --network tw-t24-probe migrate/migrate:v4.18.1 \
    -source "github://torchwoodcloud/torchwood/db/migrations#78ea1a4" \
    -database "postgres://fleetly:***@tw-t24-pg:5432/torchwood?sslmode=disable" -verbose up
… Finished 12/u events_outbox_changes_index … Finished after 10.415167803s
$ … version  → 12 ; 二次 up → "no change"（幂等）
# ②（可复跑反例）匿名配额：连续探针后 403
#   GET https://api.github.com/repos/torchwoodcloud/torchwood/contents/db/migrations?ref=78ea1a4:
#   403 API rate limit exceeded for 103.230.68.66 (rate reset in 18m17s)
#   → runbook §0.3 preflight + §3.1.2 file:// 兜底（下述 ③ 即兜底形态，全绿）
# ③ 目标库预置（file:// 源）+ bootstrap-runtime.sql（实际仓库文件）
$ docker run --rm --network tw-t24-probe -v <repo>/db/migrations:/migrations:ro \
    migrate/migrate:v4.18.1 -path=/migrations -database "…/cutover_new" up   → 12/u …
$ … percona psql -f /etc/torchwood/bootstrap-runtime.sql（-v dbname/-v auth_password）
  ALTER DATABASE / DO / ALTER ROLE 全绿；新连接实测 ce=UTF8|scs=on|super=false|bypass=false
# ④ 运行态账号 + roles-sig（真实 torchwood 二进制，bun/pgdriver 客户端）
$ docker run --rm --network tw-t24-probe --entrypoint sh \
    -e TORCHWOOD_SECURITY_JWT_SECRET=… -e FLEETLY_DB_…_URL=… \
    ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4 \
    -c 'exec /usr/local/bin/torchwood admin sync-roles-sig --dsn "$URL?sslmode=disable"'
  → roles sig key synced into public.tw_secrets (current slot)
$ docker run … -c 'exec /usr/local/bin/torchwood admin schema repair --dry-run \
    --dsn "postgres://tw_authenticator:***@tw-t24-pg:5432/torchwood?sslmode=disable"'
  → {"scanned":0,…} exit 0（编码兜底前：database ping failed: pgdriver: requires
    standard_conforming_strings=on and client_encoding=UTF8 ——兜底的必要性实证）
# ⑤ postgres 割接全链（旧库含数据+授权 → dump → 恢复进先迁移的库）
$ pg_dump -Fc -d "…/cutover_old" -f /dump/cutover_old.dump
$ pg_restore --clean --if-exists --no-owner -d "…/cutover_new" /dump/cutover_old.dump  → exit 0
$ 校验：marker=pre-cutover-marker / project=demo-app / schema_priv_tw_app=true /
        table_priv_auth=true / conn_priv=true / role_member=true /
        owner=fleetly / migr_version=12 dirty=false
$ 恢复后复验（bootstrap-runtime 的 DATABASE 级设置不随对象恢复丢失）：
  bootstrap-runtime.sql → pg_restore --clean … → 新连接 tw_authenticator：
  after_restore ce=UTF8；marker 仍在场
# ⑥ minio 命令面（silo 镜像直调 + 探针）
$ docker run -d --entrypoint /usr/bin/silo -e MINIO_ROOT_USER/PASSWORD … \
    pgsty/silo:RELEASE.2026-09-03T13-18-01Z server /data --console-address :9001
  → API/WebUI 监听 + /dev/tcp/127.0.0.1/9000 探通（compose command 形态实证）
# ⑦ compose 受控子集：终端 compose 零告警；现役 dokploy compose 首错 depends_on
$ go run ./cmd/fleetly validate <repo>/docker/fleetly/docker-compose.yml
  torchwood: valid (spec_hash 1caeb553645a, 9 services, 2 volumes)
$ go run ./cmd/fleetly validate <repo>/docker/dokploy/docker-compose.yml
  E_COMPOSE_UNSUPPORTED: service "db-grants" uses an unsupported field "depends_on"…
```

补充语义核对（本地 swarm，一手）：

- **compose `command` = 覆盖镜像 ENTRYPOINT**：`docker service create --entrypoint
  /bin/echo postgres:18 PROBE-ENTRYPOINT-OVERRIDE` → 任务日志输出
  `PROBE-ENTRYPOINT-OVERRIDE`（证明 ContainerSpec.Command 替换 entrypoint）；
  fleetly 侧同链（`planner.go` → `substrate/services.go:172` `container.Command =
  spec.Command`）——本 compose 的 worker/dispatcher/packer/silo/作业 `sh -c`
  形态即据此书写；
- **GHCR 应用镜像不含 db/migrations**（`ghcr.io/torchwoodcloud/torchwood:sha-78ea1a4`）：
  `/app` 仅 `configs/`，无 `db/`、无 `*.up.sql` → 迁移源必须外置（README §5）；
- **平台 init job 并行、无 depends_on**：T2-0④ spike 结论（顺序原语仅 init job
  本身）；本 compose 用「有界等待环」补序（db-bootstrap/roles-sig）。
