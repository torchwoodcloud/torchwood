# 环境搭建与快速开始

本章面向新加入的开发者，覆盖从零到本地跑通全系统的完整路径：前置条件、本地基础设施、数据库双账号准备、六步启动、端点速查、常用 Task 命令、常见问题与 CLI 上手。

> 事实源：`Taskfile.yml`、`docker/local/docker-compose.yml`、`configs/config.yaml.template`、`.env.example`、`cmd/server/main.go`。

---

## 1. 前置条件

| 依赖 | 版本 | 用途 |
|------|------|------|
| Go | 1.26.5 | `go.mod` 要求 |
| Node.js + pnpm | 22 + pnpm 11.20 | 构建 Console 前端 |
| Docker + Compose | 近期版本 | 运行 PostgreSQL / Redis / MinIO |
| Task | 最新 | 任务编排，安装：`go install github.com/go-task/task/v3/cmd/task@latest` |

代码生成与质量工具（`protoc-gen-go`、`migrate`、`buf@v1.65.0`、`wire`、`golangci-lint`）不必手动逐个安装，`task tools:install` 一次装齐。

---

## 2. 本地基础设施

本地依赖由 `docker/local/docker-compose.yml` 提供，端口可通过 `.env` 覆盖：

| 服务 | 镜像 | 默认端口 | 容器名 |
|------|------|----------|--------|
| PostgreSQL | `percona/percona-distribution-postgresql:18`（自带 pgvector 0.8.3 及常用扩展） | 5432 | `torchwood-postgres` |
| Redis | `redis:7-alpine` | 6379 | `torchwood-redis` |
| MinIO（SILO 分支） | `pgsty/silo` | 9000 / 9001 | `torchwood-minio` |

可覆盖的环境键：`POSTGRES_USER`、`POSTGRES_PASSWORD`、`POSTGRES_DB`、`POSTGRES_PORT`、`REDIS_PORT`、`MINIO_API_PORT`、`MINIO_CONSOLE_PORT` 等。

应用侧连接统一走 `TORCHWOOD_` 前缀环境变量（映射规则见 `03-configuration.md`）。注意数据库有**双账号契约**：`docker/local` 的 `POSTGRES_USER` 是 initdb 引导账号（superuser），只用于 bootstrap 和迁移；**应用运行态 DSN 必须使用非 superuser 的 authenticator 角色**（完整契约见 `13-operations.md`）。完成 §3 的步骤 2.5 一次性引导后，以下 `.env` 配置即可工作：

```env
# 运行态：非 superuser authenticator（生产换强口令并走密管）
TORCHWOOD_DATA_DATABASE_SOURCE=postgres://tw_authenticator:dev-only-auth-pass@127.0.0.1:5432/torchwood?sslmode=disable
TORCHWOOD_DATA_REDIS_PASSWORD=
# JWT 密钥须 ≥32 字符，含弱子串（change-me/minioadmin/password 等）拒绝启动
TORCHWOOD_SECURITY_JWT_SECRET=dev-only-0123456789abcdef-0123456789abcdef
# 首个管理员引导令牌；未配置时注册被拒
TORCHWOOD_SECURITY_SETUP_TOKEN=dev-setup-0123456789abcdef0123456789abcdef
TORCHWOOD_STORAGE_S3_ENDPOINT=http://127.0.0.1:9000
TORCHWOOD_STORAGE_S3_ACCESS_KEY_ID=minioadmin
TORCHWOOD_STORAGE_S3_SECRET_ACCESS_KEY=minioadmin
# 测试 DSN 保持引导账号：集成测试建隔离库 + 跑全量迁移，属于 owner 引导面
TORCHWOOD_TEST_DATABASE_SOURCE=postgres://torchwood:torchwood@127.0.0.1:5432/TORCHWOOD_test?sslmode=disable
TORCHWOOD_TEST_ADMIN_DATABASE_SOURCE=postgres://torchwood:torchwood@127.0.0.1:5432/postgres?sslmode=disable
```

> `TORCHWOOD_STORAGE_S3_*` 的键名由 `internal/pkg/config/bind.go` 按 proto json tag 推导，与 `configs/config.yaml.template` 及 `AGENTS.md` 保持一致。

---

## 3. 六步本地启动

### 步骤 0 — 复制环境模板

```bash
cp .env.example .env
# 生产环境须替换 JWT / SETUP 为强随机值：openssl rand -hex 32
```

### 步骤 1 — 启动基础设施

```bash
task docker:up     # docker compose up -d（docker/local/）
docker ps          # 三个容器均为 healthy
```

### 步骤 2 — 数据库迁移

```bash
task db:migrate    # migrate -path ./db/migrations -database <DSN> up
```

迁移 DSN 优先取 `TORCHWOOD_DATA_DATABASE_SOURCE`，否则由 `POSTGRES_*` 变量拼接。**迁移必须用 owner 引导账号**（`torchwood/torchwood`）。如果 `.env` 里的运行态 DSN 已换成 authenticator（如上方示例），迁移时临时用引导账号覆盖：

```bash
TORCHWOOD_DATA_DATABASE_SOURCE="postgres://torchwood:torchwood@127.0.0.1:5432/torchwood?sslmode=disable" task db:migrate
```

（命令行环境变量优先于 Task 的 dotenv 加载。）

### 步骤 2.5 — 创建 authenticator 角色（迁移之后、启动之前）

迁移完成后，向数据库灌入以下 SQL 创建运行态账号。**顺序不可颠倒**：SQL 中的 DO 块只对「引导时已存在」的 public 表授权，先引导后迁移会漏授之后新增的表。

```bash
docker exec -i torchwood-postgres psql -U torchwood -d torchwood <<'SQL'
CREATE ROLE tw_authenticator LOGIN PASSWORD 'dev-only-auth-pass'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION;
GRANT tw_owner, tw_app, tw_system TO tw_authenticator;
GRANT CONNECT, CREATE ON DATABASE torchwood TO tw_authenticator;
GRANT USAGE ON SCHEMA public TO tw_authenticator;
DO $do$ DECLARE t text; BEGIN
    FOR t IN SELECT tablename FROM pg_tables WHERE schemaname = 'public'
        AND tablename NOT IN ('catalog_databases', 'catalog_collections', 'tw_secrets')
    LOOP
        EXECUTE format('GRANT SELECT, INSERT, UPDATE, DELETE ON %I TO tw_authenticator', t);
    END LOOP;
END $do$;
GRANT REFERENCES ON public.projects TO tw_authenticator;
SQL
```

两点注意：

- **`tw_secrets` 必须保持零授权**。迁移 000004 已 REVOKE authenticator 对该表的全部权限——运行态 DSN 对密钥表零权限是防 `app.roles` GUC 提权的硬约束，切勿显式 GRANT（上方排除清单已将其排除）。
- 后续迁移新增 public 表后需要补授权，生产环境建议配置 default privileges 一劳永逸。验证 `rolsuper=false` 的方法与完整双账号契约见 `13-operations.md`。

### 步骤 3 — 安装工具与依赖

```bash
task tools:install     # protoc-gen-go / migrate / buf / wire / golangci-lint
task console:install   # pnpm install（console/）
```

### 步骤 4 — 生成代码

```bash
task generate:all      # generate:proto → generate:config → wire:all
```

| 任务 | 产物 |
|------|------|
| `generate:proto` | `buf lint` + `buf generate` → `genproto/` |
| `generate:config` | 由 `internal/pkg/config/config.proto` 产出 `config.pb.go` |
| `wire:all` | server / worker / dispatcher 三份 `wire_gen.go` |

生成物零漂移校验（生成后 `git diff --exit-code`）见 `04-codegen.md`。

### 步骤 5 — 构建并启动

```bash
task build             # console:build → go build 四个二进制（server / worker / dispatcher / torchwood）到 ./bin/
./bin/server           # Windows 下为 ./bin/server.exe
# 开发态直跑：
task dev:server        # go run ./cmd/server
task dev:worker        # go run ./cmd/worker（独立进程）
```

修改 `console/src/` 之后必须 `task console:build && task build`，否则 Go embed 打包的仍是旧版前端产物。

### 步骤 6 — 首次引导（bootstrap）

全新数据库上打开 `http://127.0.0.1:9080/console/`，登录页会自动切换为「初始化设置」表单（实现见 `internal/app/console/setup.go`）。前提是已配置 `TORCHWOOD_SECURITY_SETUP_TOKEN`，否则注册直接返回 `FailedPrecondition`。

引导行为：

- 创建首个管理员，角色固定为 `owner`；仅当 `admins` 表为空时可用，并发的重复注册由 advisory lock 串行化拦截。
- 同时创建指定的 `project_id` 及其首个业务库 `database_id`（两者都是必填项，命名规则 `^[a-z][a-z0-9]{0,27}$`）。常规 CreateProject 缺省首库名为 `app`。
- 注册成功即写入 `TORCHWOOD_session_console` HttpOnly cookie（SameSite=Lax，refresh cookie 限定 `/v1/console/auth` 路径）。
- API Key 不在注册时生成：登录后到 **API Keys** 页面创建，之后以 `x-api-key` 请求头调用 Server API。

---

## 4. 端点一览

以下为 `configs/config.yaml.template` 的默认值（HTTP / metrics 端口由 `server.http.addr` / `server.metrics.addr` 决定，非硬编码）：

| 表面 | 地址 |
|------|------|
| Admin Console | `http://127.0.0.1:9080/console/` |
| HTTP API（grpc-gateway） | `http://127.0.0.1:9080/v1/...`（如 `/v1/server/users`） |
| gRPC（仅回环） | `127.0.0.1:9060` |
| Metrics | `http://127.0.0.1:9040/metrics` |
| 健康检查 | `http://127.0.0.1:9080/healthz/liveness`、`/healthz/readiness` |

`task console:dev` 的 Vite 开发代理指向同源 `/v1`。

---

## 5. 常用任务速查

| 任务 | 用途 |
|------|------|
| `task list` | 列出全部任务 |
| `tools:install` | 安装 buf / wire / migrate 等工具 |
| `docker:up` / `docker:down` / `docker:purge` | 启动 / 停止 / 删卷重置（`docker compose down -v`） |
| `db:migrate` | 执行 `db/migrations` 迁移 |
| `generate:proto` / `generate:config` / `wire:all` / `generate:all` | proto / 配置 / Wire 代码生成 |
| `gen:authz-matrix` | 从策略注册表重新生成 `docs/developer/authz-matrix.md` |
| `lint:proto` | `buf lint` + `buf breaking --against '.git#branch=origin/main'` |
| `console:install` / `console:build` / `console:dev` | 前端依赖 / 构建 / 开发服务器 |
| `dev:server` / `dev:worker` | 直跑 server / worker |
| `build` | console:build + 四个二进制 |
| `test` | lint:go + lint:golangci + test:sdk-go + test:sdk-ts + `go test -race -v ./... -cover` |
| `lint` | lint:go + lint:golangci + lint:sdk-go + lint:console |
| `docker:build` | 构建发布镜像 |

`task test` 自动从 `.env` 加载 `TORCHWOOD_TEST_*`；`lint:golangci` 是全量门禁（无棘轮豁免）。

---

## 6. 常见问题

| 现象 | 处理 |
|------|------|
| 端口占用 | 基础设施端口改 `.env`（`POSTGRES_PORT` 等）后重跑 `task docker:up`；应用端口改 `configs/config.yaml` |
| `task db:migrate` 失败 | 确认 `docker ps` 三容器 healthy；确认 DSN 与 `POSTGRES_*` 一致；需重置时 `task docker:purge` |
| Console 改动不生效 | embed 的是 `console/dist`，须先 `console:build` 再 `build`；开发调试用 `task console:dev` |
| 鉴权失败 | 检查 JWT secret 长度与弱子串、`SETUP_TOKEN` 是否已配；API Key 放 `x-api-key` 头；跨项目调用带 `X-Torchwood-Project` |
| 反向代理后 IP 不准 | 默认不信任 `X-Forwarded-For`，需配置 `security.trusted_proxies`（如 `TORCHWOOD_SECURITY_TRUSTED_PROXIES=127.0.0.1/32`） |
| 直接 `go test` 报错 | 集成测试需要 `TORCHWOOD_TEST_*` 环境变量，改用 `task test` 或手动导出 |

---

## 7. CLI 上手

CLI 二进制为 `bin/torchwood`（入口 `cmd/torchwood`，实现随仓库根 `cli/` 包）。它经 gRPC 直连 Server API（`sdk/go/server.InvokeJSON` 动态分发），新增 RPC 无需在 CLI 登记；`cli/import_guard_test.go` 兜底禁止 CLI 直接 import 生成代码。

```bash
./bin/torchwood health get
./bin/torchwood uuid
./bin/torchwood users list --api-key <secret>
./bin/torchwood databases documents create app notes --data '{"title":"hi"}' --document-id doc1
./bin/torchwood leaderboards boards create daily_wins --period-kind daily --policy best    # 幂等建榜（leaderboards.admin）
./bin/torchwood leaderboards submit daily_wins user_42 --value 100                         # 代任意 subject 提交（leaderboards.write）
./bin/torchwood leaderboards top daily_wins
./bin/torchwood analytics overview --from 2026-09-01 --to 2026-09-14                       # 日期按 UTC 零点归一
./bin/torchwood analytics ingest --file events.json                                        # 或 --file - 走 stdin（analytics.write）
./bin/torchwood assets grant user_42 gems --quantity 100 --idempotency-key comp-2026-0914  # 运营补偿（assets.write，进审计）
./bin/torchwood payments refund --help                                                     # 订单查询 / 退款 / 人工履约
TORCHWOOD_GIT_TOKEN=ghp_xxx ./bin/torchwood functions deployments create-from-git greet --url https://github.com/acme/functions.git --ref main --dir functions/greet   # git 源部署（token 走环境变量，见 08-functions.md §3.4）
./bin/torchwood runbook up --dir runbooks                                                  # 版本化资源迁移，见 19-runbook.md
./bin/torchwood rpc /torchwood.server.v1.UsersService/ListUsers --data '{"pageSize":10}' --api-key <secret>
```

全局旗标在子命令路径之后、位置参数之前给出：`--endpoint`、`--api-key`、`--timeout`、`--output`、`--tls`（系统根证书校验，用于反向代理终结 TLS 的场景，代理需以 h2c 转发后端）、`--profile`。对应环境变量 `TORCHWOOD_CLI_*`。

### 7.1 配置文件与多项目 profile

连接配置可落在 `~/.torchwood/config.yaml`（路径可用 `TORCHWOOD_CLI_CONFIG` 覆盖）。一个 profile 对应一个项目上下文（endpoint + 该项目 scoped API Key），多项目各占一个 profile 实现隔离；Agent 和脚本只需 `--profile <name>`，密钥本体不进对话与代码：

```bash
./bin/torchwood config init                          # 生成带注释的模板（已存在则报错不覆盖）
./bin/torchwood config set local api-key --stdin     # 密钥经 stdin 注入，不进 shell 历史
./bin/torchwood config set prod endpoint grpc.example.com:443
./bin/torchwood config set prod tls true
./bin/torchwood config use prod                      # 设为缺省 profile
./bin/torchwood config list                          # 列出 profile（密钥打码，JSON 输出）
./bin/torchwood config show prod                     # 查看单个 profile
./bin/torchwood config remove prod                   # 删除 profile
./bin/torchwood config path                          # 打印配置文件路径
./bin/torchwood users list --profile local           # 显式选择 profile；缺省用配置的 default 键
```

取值优先级（每个字段独立判定）：**显式 flag > `TORCHWOOD_CLI_*` 环境变量 > 配置 profile 值 > 内建默认**。profile 选择优先级：**`--profile` flag > `TORCHWOOD_CLI_PROFILE` > 配置的 `default` 键**。

配置文件做严格解析：未知键、悬空 default 指向、非法字段值都会报错而不是静默忽略，避免拼错键名后连错环境。文件以 0600 权限落盘；所有 config 命令的输出不回显密钥本体（打码为 `****` + 末 4 位）。

---

## 相关文档

- `03-configuration.md` — 配置体系与 `TORCHWOOD_` 环境变量映射
- `13-operations.md` — 生产部署与双账号契约
- `19-runbook.md` — runbook 资源迁移命令
- `11-testing.md` — 测试分层与 `task test` 细节
