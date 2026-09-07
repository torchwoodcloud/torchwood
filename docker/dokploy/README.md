# Torchwood × Dokploy 部署指南

本目录提供 Dokploy（自托管 PaaS，Traefik + Docker Compose）一键部署所需的全部文件：
单 Compose 栈内含 **Postgres(pgvector 基座) + Redis + MinIO + 一次性作业链（迁移 → 三角色授权 → roles_sig 落库）+ server + worker**。
镜像由仓库根 `Dockerfile` 多阶段构建（console SPA 经 go:embed 打进二进制）。

> 部署时序契约（`docs/developer/13-operations.md` §4.5/§6.1：迁移 → `torchwood admin sync-roles-sig` → server/worker 启动）
> 由 compose `depends_on` 链自动保证，无需手工干预。

## 0. 前置条件

- 一台装好 Docker 的服务器 + Dokploy（≥ v0.10）；
- 本仓库可被 Dokploy 访问（GitHub/GitLab/直接 Git URL；私有仓库需在 Dokploy 配置凭证）；
- 一个指向服务器的域名（如 `tw.example.com`），用于 HTTPS 反代与 `public_url`。

## 1. 创建 Compose 服务

1. Dokploy 控制台：**Projects → Create Project → Create Service → Docker Compose**；
2. 来源选你的 Git 仓库与分支；
3. **Compose Path** 填：`./docker/dokploy/docker-compose.yml`；
4. 先别急着 Deploy——到 **Environment** 页签按下表添加变量，再回来点 **Deploy**。

首次构建较慢（console pnpm install/build + Go 三二进制编译），属正常现象。

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
| `TORCHWOOD_SERVER_HTTP_CORS_ALLOW_ORIGINS` | | 外部浏览器端 SDK 来源（逗号分隔）；Console 与网关同源，无需配置 |

> **注意 1（密码字符集）**：口令会被拼进 `postgres://` DSN 与 psql 脚本，含 `@ : / # ? ' "` 等字符会直接破坏连接串。
> 一律使用 hex/base62 长随机。
>
> **注意 2**：`POSTGRES_USER` 是基础设施引导账号（superuser），**只**用于迁移/引导作业；
> 运行态 DSN 固定走 `tw_authenticator`（compose 已配好，superuser 会绕过文档面 RLS，见 ops 文档 §4.5）。

## 3. 绑定域名

Compose 服务页 → **Domains** → Add Domain：

- **Service**: `server`，**Port**: `9080`，填你的域名并启用 HTTPS（Let's Encrypt）。

应用对 9080 端口暴露 gRPC-gateway HTTP + `/console/` + Storage 上传下载 + 健康端点；
gRPC（回环 9060）与 metrics（回环 9040）不对外。

## 4. 首次引导（bootstrap）

部署完成后：

1. 打开 `https://<你的域名>/console/`，登录页会自动切为初始化表单；
2. 粘贴 Environment 里的 `TORCHWOOD_SECURITY_SETUP_TOKEN`，创建首个管理员（用户名固定 `owner`）、
   `project_id` 与 `database_id`（将创建项目 + 系统 `default` 库 + 业务库）；
3. 登录后在 Console 创建 API Key（secret 仅展示一次），供 CLI/Agent 调用 Server API。

## 5. 验证

```bash
curl https://<域名>/healthz/readiness      # 200（依赖 PG/Redis/MinIO 全绿）
curl https://<域名>/v1/server/health/version
curl https://<域名>/v1/health              # 依赖明细
```

## 6. 栈内行为说明

- **一次性作业链**：`migrate → db-grants → roles-sig → server/worker`（`depends_on: service_completed_successfully`）。
  作业全部幂等：重部署（镜像变更触发重建）时自动重跑，同钥 `sync-roles-sig` 整体 no-op。
  Dokploy 面板上这几个服务显示 Exited(0) 属预期。
- **首次 initdb 引导**：`initdb/01-authenticator.sh` 只在 `postgres_data` 卷为空时执行。
  之后改 `TORCHWOOD_AUTH_PASSWORD` 不会同步数据库角色口令，需在 Dokploy 打开 postgres 终端手工执行：
  `ALTER ROLE tw_authenticator PASSWORD '<新口令>';`（同步改 Environment 后重启 server/worker）。
- **MinIO bucket**：应用启动时自动创建（`torchwood-storage`，小写），无需 mc 初始化。
- **Functions（可选）**：worker 常驻消费函数执行队列；启用 docker executor 需放开 compose 中
  `docker.sock` 挂载（⚠ 等同宿主 root 权限）。不用 Functions 可删除 worker 服务。

## 7. 日常运维

| 操作 | 做法 |
|------|------|
| 升级 | Dokploy **Redeploy**：自动重建镜像 → 重跑迁移（增量）→ 授权/roles-sig 幂等 no-op → 滚动替换 server/worker。生产环境排水窗口 30s（`TORCHWOOD_ENV=production` 已设） |
| 备份 | 项目级：`docker exec <server容器> torchwood admin export --project <id> --out /backup/p1`（需持久化 `/backup` 卷或导出到 S3）；全实例：`pg_dump -Fc` 全库（覆盖 `public` 与全部 `tw_*` schema）+ MinIO `mc mirror` |
| 换 JWT 密钥 | 改 Environment `TORCHWOOD_SECURITY_JWT_SECRET` → Redeploy（roles-sig 自动重落库，双钥窗口内旧 sig 不降级） |
| 看 pgvector | `SELECT extname, extversion FROM pg_extension WHERE extname='vector';`（Percona PG18 镜像自带 0.8.3，迁移 000005 自动启用） |
| psql 终端 | Dokploy → postgres 服务 → Execute Shell：`psql -U torchwood -d torchwood` |

## 8. 方案 B：复用外部依赖（不用栈内 PG/Redis/MinIO）

若已有 Dokploy 模板部署的 Postgres/Redis/MinIO 或外部 S3，可将本栈拆为 **Application** 类型部署
（build 方式选 Dockerfile，监听端口 9080），环境变量同 §2，另外：

- `TORCHWOOD_DATA_DATABASE_SOURCE` 指向外部 PG 的 `tw_authenticator` DSN；
- `TORCHWOOD_STORAGE_S3_ENDPOINT`/凭据指向外部 S3/MinIO；
- 迁移与引导作业需手工执行一次（保持 owner/authenticator 双账号契约）：

```bash
# ① 迁移（owner 引导 DSN；可用一次性容器挂本仓库 db/migrations）
docker run --rm -v "$PWD/db/migrations:/migrations" migrate/migrate:v4.18.1 \
  -path=/migrations -database "<owner DSN>" up
# ② 授权补齐（psql 以 owner 连接）
psql "<owner DSN>" -v ON_ERROR_STOP=1 -v dbname=<库名> -f docker/dokploy/bootstrap-roles.sql
# ③ roles_sig 落库
docker run --rm torchwood-app:local torchwood admin sync-roles-sig --dsn "<owner DSN>"
```

首次 initdb 引导（创建 `tw_authenticator`）对应 `docker/dokploy/initdb/01-authenticator.sh` 中的 SQL，需以 superuser 手工执行一次。

## 9. 文件清单

| 文件 | 用途 |
|------|------|
| `docker-compose.yml` | 全栈编排 + 一次性作业链（相对路径均相对本目录） |
| `config.yaml` | 运行时配置基线，bind-mount 到 `/app/configs/config.yaml`（env 覆盖优先） |
| `initdb/01-authenticator.sh` | 首次 initdb 创建 `tw_authenticator`（仅空卷执行一次） |
| `bootstrap-roles.sql` | 迁移后补齐 authenticator 授权面（ops 文档 §4.5 ②③④④'） |
