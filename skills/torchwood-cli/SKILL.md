---
name: torchwood-cli
description: Operate a Torchwood backend through the `torchwood` CLI — manage projects, users, groups, databases/collections/documents, storage metadata, functions, and OAuth providers over the gRPC Server API. Use when the user asks to inspect/manage Torchwood resources, seed or query application data, deploy functions, or automate a Torchwood server from the terminal.
---

# torchwood CLI

`torchwood` 是 Torchwood 后端（BaaS）的运维/数据 CLI：经 gRPC 直连 Server API，以 **scoped API Key** 认证，输出一律 JSON。典型场景：管理项目/用户/分组、建库建集合写文档、查询业务数据、部署与执行函数、存储元数据管理。

## 0. 先检查，再动手

```bash
torchwood version          # 二进制可用（仓库内构建产物在 bin/torchwood，Windows 为 bin\torchwood.exe）
torchwood health get       # 端点连通性（公开方法，无需 key）
torchwood config list      # 已配置哪些 profile
```

若 `health get` 报连接失败，先解决连通性（见 §5），不要盲目重试业务命令。

## 1. 配置与多项目隔离（~/.torchwood/config.yaml）

一个 **profile = 一个项目上下文**（endpoint + 该项目的 scoped API Key）。多项目各占一个 profile，**API Key 只存在本地配置文件里，不进对话/脚本/日志**：

```bash
torchwood config init                          # 首次：生成模板（local profile 指向 127.0.0.1:9060）
torchwood config set local api-key --stdin     # 密钥经 stdin 注入（不进 shell 历史）；需要使用者提供密钥
torchwood config set prod endpoint grpc.example.com:443
torchwood config set prod tls true             # 反代终结 TLS 的远端环境
torchwood config use prod                      # 设为缺省 profile
torchwood config list                          # 列 profile（key 打码为 ****+末4位，JSON）
```

选择 profile：`--profile <name>` 旗标 > `TORCHWOOD_CLI_PROFILE` 环境变量 > 配置文件 `default` 键。
每个字段的取值优先级：显式 flag > `TORCHWOOD_CLI_*` 环境变量 > profile 值 > 内建默认。

面向 Agent 的约定：如果使用者说「操作 prod 项目」，用 `--profile prod`；若 `config list` 里没有对应 profile，请使用者自行完成 `config set <name> api-key --stdin`（密钥不经过对话）。

## 2. 语法铁律（违反即报错）

1. **全局旗标（`--endpoint/--api-key/--timeout/--output/--tls/--profile` 及各命令自有旗标）放在子命令路径之后、位置参数之前**：`torchwood databases documents get app notes doc1 --version 1`；`torchwood users get --api-key ... u1`（旗标在位置参数后）会解析失败。
2. 输出恒为 JSON（`--output` 仅支持 json）。解析时注意整数字段可能是字符串或数字以外的 protojson 形态，宽松处理。
3. 退出码：`0` 成功；`1` 参数/本地错误；`2` 4xx 类（含 401/403/404/409）；`3` 5xx/网络类；`4` 限流 429。脚本按退出码分支。
4. Windows shell 传 JSON 时用外层双引号 + 内层转义，或改用 `rpc --data` 也可以；POSIX shell 用单引号包裹 JSON。
5. `health`/`uuid`/`version`/`config` 无需 API Key；其余命令必须能解析到 key（flag、env 或 profile 三者之一）。

## 3. 命令地图

| 分组 | 能力 |
|---|---|
| `projects` | 项目 list/get（增删是平台管理员操作，不经 CLI） |
| `users` | 用户 CRUD、`update-password`、`sessions`、`tokens`（模拟签发 token） |
| `groups` | 分组 CRUD、`prefs`、`memberships`（邀请/角色/状态） |
| `databases` | 库 CRUD；`collections`、`attributes`、`indexes`、`documents`（含 `count`/`bulk-update`/`bulk-delete`/`upsert`） |
| `storage` | `buckets`、`files` 元数据管理、`usage`（**上传/下载不经 CLI**，走 HTTP handler） |
| `functions` | 函数 CRUD、`deployments`（zip 上传）、`variables`、`executions` |
| `oauth-providers` | OAuth 提供商 list/upsert/delete |
| `admin` | 平台运维：`outbox list-dead/replay`；export/import/schema 直连 DB（需 DSN，不走 API Key） |
| `rpc` | 逃生舱：按完整 gRPC 方法名调用任意 Server API（覆盖未出专用命令的新 RPC） |
| `config` | 本地配置文件管理（见 §1） |

完整动词与旗标清单见 [references/commands.md](references/commands.md)。

## 4. 常见任务 playbook

**建数据模型 + 写入 + 查询**（顺序不可颠倒：集合 → 属性 → 等索引 ready → 文档）：

```bash
torchwood databases create --id app --name 应用库
torchwood databases collections create app notes --name 笔记 --permissions '["read(\"users\")"]'
torchwood databases attributes create app notes --key title --type string --required --size 256
torchwood databases attributes create app notes --key tags --type string --array
torchwood databases indexes create app notes --id idx_title --type key --attributes '["title"]'
torchwood databases documents create app notes --data '{"title":"hi","tags":["a"]}' --document-id doc1
torchwood databases documents list app notes --queries '["equal(\"tags\",\"a\")","orderDesc(\"$createdAt\")","limit(20)"]'
torchwood databases documents count app notes --queries '["greaterThan(\"$createdAt\",\"2026-01-01T00:00:00Z\")"]'
```

- 查询 DSL：`equal/greaterThan/lessThan/contains/...`、`orderDesc/orderAsc`、`limit/offset`；数组属性专用 `containsAny/containsAll`。JSON 数组里每个元素是一个查询字符串，内部双引号要转义。
- **文档更新/删除走 OCC**：必须传 `--version <n>`（取自上次读取响应里的 version）；409 冲突时重新读取再改。
- 更新支持 `--increment '{"views":1}'` 与 `--permissions`；未显式传入的字段不修改。

**用户管理**：`users create --email --password` → `users tokens create <uid>`（模拟登录拿 token）→ `users sessions list/delete`。

**函数**：`functions create --id --runtime`（runtime 先查 `functions runtimes`）→ 打包 zip → `functions deployments create <fid> --code func.zip`（≤8MiB）→ `functions executions create <fid> --input '{}'` → `functions executions list <fid>` 看结果与日志字段。

**逃生舱**：`torchwood rpc /torchwood.server.v1.UsersService/ListUsers --data '{"pageSize":10}'`——方法名 = proto 全路径；请求体是 camelCase protojson。APIKeysService 被服务端禁止经 API Key 调用。

## 5. 故障排查

| 症状 | 处置 |
|---|---|
| `Unavailable` / 连接拒绝 | 确认 endpoint（本机默认 `127.0.0.1:9060`）；远端走 SSH 隧道或反代；TLS 反代场景加 `--tls`（或 profile `tls: true`），代理需以 h2c 转发后端 |
| `Unauthenticated`（退出码 2） | key 缺失/过期/删了，或不属于该 endpoint 的实例：`torchwood health get` 验证连通，`torchwood config show <profile>` 核对（key 打码）；key 换了就用 `config set <profile> api-key --stdin` 更新 |
| `PermissionDenied`（退出码 2） | API Key scopes 不够（如只有 `users.read` 却执行写操作），去 Console 换 scope 或新 key |
| `profile "x" not found` | `torchwood config list` 看已有 profile 名，或 `config set x endpoint ...` 新建 |
| `invalid --timeout` / 配置解析错误 | 配置文件被手改坏了：按报错行修正；`config init` 不覆盖已存在文件 |
| 限流（退出码 4） | 退避重试，别立即循环 |

## 6. 安全守则（Agent 必读）

- **不要** `cat`/`type` 配置文件、不要在命令行参数里携带明文 api-key（配置缺失时请使用者自己 `config set ... --stdin`）。
- `config list/show` 的输出已打码，可以安全读取；任何命令输出若意外包含密钥，提醒使用者轮换。
- 不要把 API Key 写进代码、提交物或日志；示例里用 `<secret>` 占位。
- 写操作（删库/删集合/删用户/重放死信）执行前先 `get`/`list` 确认目标，并向使用者复述将要做的事。
