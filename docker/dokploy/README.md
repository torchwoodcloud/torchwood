# Torchwood × Dokploy 部署指南

本目录提供 Dokploy（自托管 PaaS，Traefik + Docker Compose）一键部署所需的全部文件：
单 Compose 栈内含 **Postgres(pgvector 基座) + Redis + MinIO + 一次性作业链（迁移 → 三角色授权 → roles_sig 落库）+ server + worker**。
应用镜像由 GitHub Actions 预构建并推送到 GHCR（见 §5），部署机只 pull 不编译——
小内存 VPS 上跑 `go build` 会 OOM，部署被杀时状态表现为 cancelled。

> 部署时序契约（`docs/developer/13-operations.md` §4.5/§6.1：迁移 → `torchwood admin sync-roles-sig` → server/worker 启动）
> 由 compose `depends_on` 链自动保证，无需手工干预。
>
> ⚠️ **不要在 Dokploy 应用的「Command / 自定义命令」字段填任何内容**（包括
> `torchwood admin sync-roles-sig`——那是 §10 外部镜像路径的手工步骤，且填入的
> 必须是完整合法的 docker 命令；残缺形式如 `docker torchwood …` 会让每次部署
> 直接失败 `unknown command`）。本 compose 路径留空即可，作业链随 `up` 自动执行。

## 0. 前置条件

- 一台装好 Docker 的服务器 + Dokploy（≥ v0.10）；
- 本仓库可被 Dokploy 访问（GitHub/GitLab/直接 Git URL；私有仓库需在 Dokploy 配置凭证）；
- 一个指向服务器的域名（如 `tw.example.com`），用于 HTTPS 反代与 `public_url`；
  HTTP 与 gRPC 建议各一个子域（如 `tw.example.com` / `grpc.tw.example.com`），见 §3。

## 1. 创建 Compose 服务

1. Dokploy 控制台：**Projects → Create Project → Create Service → Docker Compose**；
2. 来源选你的 Git 仓库与分支；
3. **Compose Path** 填：`./docker/dokploy/docker-compose.yml`；
4. 先别急着 Deploy——到 **Environment** 页签按下表添加变量，再回来点 **Deploy**。

部署只做镜像拉取 + 容器编排，一两分钟内完成。前提是镜像已存在：推送到 main 后
GitHub Actions 会自动构建并推 GHCR（首次部署前确认 [image workflow](https://github.com/torchwoodcloud/torchwood/actions/workflows/image.yml) 至少成功过一次）。

## 2. 环境变量（Environment 页签）

| 变量 | 必填 | 说明 / 生成方式 |
|------|------|-----------------|
| `TORCHWOOD_SECURITY_JWT_SECRET` | ✅ | 主密钥（≥32 字符），`openssl rand -hex 32`；弱子串（`change-me`/`secret` 等）拒绝启动 |
| `TORCHWOOD_SECURITY_SETUP_TOKEN` | ✅ | Console 首个管理员引导令牌，`openssl rand -hex 32`；填到 Console 初始化表单 |
| `TORCHWOOD_SERVER_HTTP_PUBLIC_URL` | ✅ | 对外绝对地址，如 `https://tw.example.com`（OAuth/支付回调基准） |
| `POSTGRES_PASSWORD` | ✅ | owner 引导账号（superuser，仅迁移/引导用）口令；**仅用 `[A-Za-z0-9]`**，见下方「注意 1」 |
| `TORCHWOOD_AUTH_PASSWORD` | ✅ | 运行态 `tw_authenticator`（非 superuser）口令；**仅用 `[A-Za-z0-9]`**，如 `openssl rand -hex 24` |
| `MINIO_ROOT_USER` / `MINIO_ROOT_PASSWORD` | ✅ | 栈内 MinIO 凭据（同时作为应用 S3 凭据注入） |
| `POSTGRES_USER` / `POSTGRES_DB` | | 默认 `torchwood` / `torchwood` |
| `TORCHWOOD_SERVER_HTTP_CORS_ALLOW_ORIGINS` | | 浏览器端跨域来源，**默认 `*`**（非凭据放开：SDK/前端以 Bearer token 调 API）。仅跨站携带会话 cookie 的特殊形态需改为显式列出 origin 并开 `allow_credentials`（config.yaml） |
| `TORCHWOOD_HTTP_DOMAIN` | ✅ | HTTP 域名（gateway/Console/Storage），如 `tw.example.com`；Traefik label 路由，见 §3 |
| `TORCHWOOD_GRPC_DOMAIN` | ✅ | gRPC 域名（Traefik 终结 TLS → h2c），如 `grpc.tw.example.com`，见 §4 |
| `TORCHWOOD_GRPC_PORT` | | 宿主侧 gRPC 端口，默认 `9060`；**仅绑回环**，供 SSH 隧道兜底（见 §4） |

> **注意 1（密码字符集）**：口令会被拼进 `postgres://` DSN 与 psql 脚本，含 `@ : / # ? ' "` 等字符会直接破坏连接串。
> 一律使用 hex/base62 长随机。
>
> **注意 2**：`POSTGRES_USER` 是基础设施引导账号（superuser），**只**用于迁移/引导作业；
> 运行态 DSN 固定走 `tw_authenticator`（compose 已配好，superuser 会绕过文档面 RLS，见 ops 文档 §4.5）。

## 3. 绑定域名（compose Traefik label，不用 Domains UI）

域名路由**不走 Dokploy「Domains」UI**——两条 Traefik router/service 已直接以 label
声明在本目录 compose 的 `server` 服务上，改域名/换环境只动 Environment 变量：

| 变量 | 承载 | 入口 |
|------|------|------|
| `TORCHWOOD_HTTP_DOMAIN` | gRPC-gateway REST + `/console/` + Storage 上传下载 + 健康端点（9080） | `https://<域名>/…`；80 自动 301 → https |
| `TORCHWOOD_GRPC_DOMAIN` | gRPC（9060，Traefik 终结 TLS 后以 **h2c** 转发——gRPC 要求端到端 HTTP/2） | `--endpoint <域名>:443 --tls`（见 §4） |

两条 Host 规则互斥、互不影响；HTTPS 同用 Dokploy 内置的 `letsencrypt` 证书解析器。

> ⚠️ **Dokploy「Domains」UI 里不要再保留本服务的域名条目**（包括历史添加的）：
> UI 生成的 router 与 label 声明的 router 规则相同，并存时 Traefik 二选一不可控
> （可能间歇落到无 h2c 的后端，表现为 gRPC 间歇 500）。UI 只留空即可；
> DNS 记录照常指向服务器，与本配置无关。
>
> 环境变量的插值与 compose 内其它 `${VAR}`（如 `TORCHWOOD_SECURITY_JWT_SECRET`）同源，
> 无需额外开关；插值落空（规则渲染成空 Host）时先检查变量拼写。

## 3.1 OAuth 第三方登录（浏览器流）

为你的客户前端（任意域名，含独立站点）接入 GitHub 等 OAuth 登录：

1. **Provider 侧**（以 GitHub App 为例）：
   - 回调 URL 填 `https://<你的域名>/v1/account/oauth2/github/callback`——**全部署共享**，
     多项目由 OAuth `state` 区分，无需按项目注册；
   - Permissions → Account permissions → **Email addresses 设为 Read-only**
     （GitHub App 忽略 authorize 的 scope 参数，取邮箱走 App 权限；经典 OAuth App 不需要）；
   - 配置用 Client ID（`Iv1.` 开头），不是纯数字的 App ID。
2. **Torchwood 侧**：Console → Settings → OAuth 启用 github 并填 Client ID/Secret（每项目独立配置）；
   Console → 项目详情 → **Redirect Allowlist** 把客户前端域名加入白名单（success/failure 落点校验）。
3. **前端发起**（一行跳转，无需任何 CORS/凭据配置）：

```js
window.location.href =
  `https://<你的域名>/v1/account/oauth2/github/authorize?project_id=<项目id>` +
  `&success=${encodeURIComponent("https://<前端域名>/auth/callback")}` +
  `&failure=${encodeURIComponent("https://<前端域名>/login?oauth=failed")}`;
```

authorize 端点校验白名单后 `Set-Cookie` nonce 并 302 到授权页；授权完成回调自动落到
success 地址（token 在 URL fragment）。失败时回 failure 地址带 `error=oauth_failed`。
CORS 基线 `*` 只服务普通 API 调用（Bearer token），OAuth 流不经 CORS。

## 4. gRPC 对外暴露（SDK / CLI 直连）

server 的 gRPC 监听 `:9060`（明文 h2c）。对外标准路径走 **Traefik TLS 终结域名**
（§3 的 `TORCHWOOD_GRPC_DOMAIN`）：label 以 `loadbalancer.server.scheme=h2c` 满足
gRPC 端到端 HTTP/2 要求，客户端用系统根证书连接：

SDK（Go）——`WithTLS` 启用 TLS（自定义 CA/mTLS 用 `WithDialOptions` 传凭据）：

```go
client, err := sdkserver.New("grpc.tw.example.com:443",
    sdkserver.WithAPIKey("sk-..."),
    sdkserver.WithTLS(),
    sdkserver.WithDatabaseID("main"))
```

CLI（`--tls` 用系统根证书；全局旗标在子命令路径之后、位置参数之前）：

```bash
torchwood health get --endpoint grpc.tw.example.com:443 --tls
torchwood users list --endpoint grpc.tw.example.com:443 --tls --api-key sk-...
# 环境变量等价：TORCHWOOD_CLI_ENDPOINT / TORCHWOOD_CLI_API_KEY
```

**回环兜底通道**：compose 仍以 `127.0.0.1:${TORCHWOOD_GRPC_PORT:-9060}:9060` 发布宿主端口
（只绑回环，不进公网），供 SSH 隧道明文直连：

```bash
ssh -L 9060:127.0.0.1:9060 <服务器>
torchwood health get --endpoint 127.0.0.1:9060   # 隧道内明文，等价 443+--tls
```

如确需远程明文直连（不推荐），去掉 ports 的 `127.0.0.1:` 前缀恢复 0.0.0.0 发布，
**并务必用防火墙/安全组限制来源 IP**——`x-api-key` 在明文链路上可被嗅探。

gRPC 直连不受 `server.http.public_url` 与 CORS 影响（那是 HTTP/浏览器侧概念）；
gRPC 侧认证即 API Key（`x-api-key` metadata），限流维度同理。

## 5. 镜像与版本（GitHub Actions → GHCR）

应用镜像由 [image workflow](https://github.com/torchwoodcloud/torchwood/actions/workflows/image.yml)
在 GitHub Actions 上构建（console SPA + server/worker/torchwood 三二进制），推送到
`ghcr.io/torchwoodcloud/torchwood`。compose 三个应用服务（server/worker/roles-sig）
均声明 `pull_policy: always`，每次 Deploy/Redeploy 都会拉取最新并按需重建容器。

| tag | 何时更新 | 用途 |
|-----|----------|------|
| `latest` | 每次 push 到 main | 默认；Redeploy 即升级到最新 main |
| `sha-<短commit>` | 每次 push 到 main | 回滚锚点：把 `TORCHWOOD_IMAGE` 钉到它再 Redeploy |
| `vX.Y.Z` / `vX.Y` | push `v*` tag | 语义版本发布 |

- **钉版本/回滚**：Environment 加 `TORCHWOOD_IMAGE=ghcr.io/torchwoodcloud/torchwood:sha-abc1234` → Redeploy。
- **版本元数据**：CI 注入 `version/commit/date`，`curl https://<域名>/v1/server/health/version` 可验证部署到的确切版本。
- **GHCR 可见性**：首次发布后包默认**私有**，二选一：
  - 改 Public：GitHub org → Packages → `torchwood` → Package settings → Danger Zone → Change visibility（推荐）；
  - 保持私有：部署机 `docker login ghcr.io -u <用户名> -p <PAT(read:packages)>` 后再 Deploy。
- **本地出镜像**：`task docker:build` 仍可用（同 Dockerfile，版本元数据为 dev/unknown）。

## 6. 首次引导（bootstrap）

部署完成后：

1. 打开 `https://<你的域名>/console/`，登录页会自动切为初始化表单；
2. 粘贴 Environment 里的 `TORCHWOOD_SECURITY_SETUP_TOKEN`，创建首个管理员（用户名固定 `owner`）、
   `project_id` 与 `database_id`（将创建项目 + 系统 `default` 库 + 业务库）；
3. 登录后在 Console 创建 API Key（secret 仅展示一次），供 CLI/Agent 调用 Server API。

## 7. 验证

```bash
curl https://<HTTP域名>/healthz/readiness      # 200（依赖 PG/Redis/MinIO 全绿）
curl https://<HTTP域名>/v1/server/health/version
curl https://<HTTP域名>/v1/health              # 依赖明细
curl -I http://<HTTP域名>/                     # 301 → https（80 重定向路由）
torchwood health get --endpoint <gRPC域名>:443 --tls   # gRPC 经 Traefik TLS（无需 API key）
```

## 8. 栈内行为说明

- **一次性作业链**：`migrate → db-grants → roles-sig → server/worker`（`depends_on: service_completed_successfully`）。
  作业全部幂等：重部署（镜像变更触发重建）时自动重跑，同钥 `sync-roles-sig` 整体 no-op。
  Dokploy 面板上这几个服务显示 Exited(0) 属预期。
- **首次 initdb 引导**：`initdb/01-authenticator.sh` 只在 `postgres_data` 卷为空时执行。
  之后改 `TORCHWOOD_AUTH_PASSWORD` 不会同步数据库角色口令，需在 Dokploy 打开 postgres 终端手工执行：
  `ALTER ROLE tw_authenticator PASSWORD '<新口令>';`（同步改 Environment 后重启 server/worker）。
- **MinIO bucket**：应用启动时自动创建（`torchwood-storage`，小写），无需 mc 初始化。
- **Redis AOF**：compose 已带 `--appendonly yes`——refresh 轮换记录存 Redis，
  无持久化时容器重启 = 全部已登录会话下次刷新即失效（重新登录即恢复，非故障）。
  换 Redis 实例同理（`docs/developer/13-operations.md` §6.1）。
- **Functions（可选）**：worker 常驻消费函数执行队列；启用 docker executor 需放开 compose 中
  `docker.sock` 挂载（⚠ 等同宿主 root 权限）。不用 Functions 可删除 worker 服务。

## 9. 日常运维

| 操作 | 做法 |
|------|------|
| 升级 | Dokploy **Redeploy**：拉取 latest 新镜像（`pull_policy: always`）→ 重跑迁移（增量）→ 授权/roles-sig 幂等 no-op → 滚动替换 server/worker。生产排水窗口 30s（`TORCHWOOD_ENV=production` 已设）。`/v1/server/health/version` 可验证版本 |
| 回滚 | Environment 加 `TORCHWOOD_IMAGE=<sha- tag 或 vX.Y.Z>` → Redeploy（迁移只进不退，回滚不回退 schema） |
| 备份 | 项目级：`docker exec <server容器> torchwood admin export --project <id> --out /backup/p1`（需持久化 `/backup` 卷或导出到 S3）；全实例：`pg_dump -Fc` 全库（覆盖 `public` 与全部 `tw_*` schema）+ MinIO `mc mirror` |
| 换 JWT 密钥 | 改 Environment `TORCHWOOD_SECURITY_JWT_SECRET` → Redeploy（roles-sig 自动重落库，双钥窗口内旧 sig 不降级） |
| 看 pgvector | `SELECT extname, extversion FROM pg_extension WHERE extname='vector';`（Percona PG18 镜像自带 0.8.3，迁移 000005 自动启用） |
| psql 终端 | Dokploy → postgres 服务 → Execute Shell：`psql -U torchwood -d torchwood` |

## 10. 方案 B：复用外部依赖（不用栈内 PG/Redis/MinIO）

若已有 Dokploy 模板部署的 Postgres/Redis/MinIO 或外部 S3，可将本栈拆为 **Application** 类型部署
（源选 **Docker Image**：`ghcr.io/torchwoodcloud/torchwood:latest`，监听端口 9080），环境变量同 §2，另外：

- gRPC 对外：`TORCHWOOD_SERVER_GRPC_ADDR=:9060`，并在 Application 的「Ports」里追加 `9060`（宿主:容器）；
- `TORCHWOOD_DATA_DATABASE_SOURCE` 指向外部 PG 的 `tw_authenticator` DSN；
- `TORCHWOOD_STORAGE_S3_ENDPOINT`/凭据指向外部 S3/MinIO；
- 迁移与引导作业需手工执行一次（保持 owner/authenticator 双账号契约；**仅适用本节
  外部路径**——单 Compose 栈路径的作业已内置，见文首警告）：

```bash
# ① 迁移（owner 引导 DSN；可用一次性容器挂本仓库 db/migrations）
docker run --rm -v "$PWD/db/migrations:/migrations" migrate/migrate:v4.18.1 \
  -path=/migrations -database "<owner DSN>" up
# ② 授权补齐（psql 以 owner 连接）
psql "<owner DSN>" -v ON_ERROR_STOP=1 -v dbname=<库名> -f docker/dokploy/bootstrap-roles.sql
# ③ roles_sig 落库（用 GHCR 镜像内置的 CLI）
docker run --rm ghcr.io/torchwoodcloud/torchwood:latest \
  torchwood admin sync-roles-sig --dsn "<owner DSN>"
```

首次 initdb 引导（创建 `tw_authenticator`）对应 `docker/dokploy/initdb/01-authenticator.sh` 中的 SQL，需以 superuser 手工执行一次。

## 11. 文件清单

| 文件 | 用途 |
|------|------|
| `docker-compose.yml` | 全栈编排 + 一次性作业链（相对路径均相对本目录） |
| `config.yaml` | 运行时配置基线，bind-mount 到 `/app/configs/config.yaml`（env 覆盖优先） |
| `initdb/01-authenticator.sh` | 首次 initdb 创建 `tw_authenticator`（仅空卷执行一次） |
| `bootstrap-roles.sql` | 迁移后补齐 authenticator 授权面（ops 文档 §4.5 ②③④④'） |
