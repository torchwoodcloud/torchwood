# torchwood CLI 命令全表

调用形态：`torchwood <group> [sub-group...] <verb> [flags] <positionals>`。
**旗标一律在位置参数之前**（Go flag 语义；全局旗标可在任一分组层给出）。
全局旗标：`--endpoint`、`--api-key`、`--timeout`、`--output`、`--tls`、`--profile`。
输出恒为 JSON；退出码 0=成功 / 1=参数与本地错误 / 2=4xx / 3=5xx 与网络 / 4=限流。

## 公开 / 本地命令（无需 API Key）

| 命令 | 说明 |
|---|---|
| `torchwood health get` | 服务健康状态 |
| `torchwood health version` | 服务构建版本 |
| `torchwood version` | CLI 版本 |
| `torchwood uuid` | 本地生成 UUID |
| `torchwood config path` | 打印配置文件路径 |
| `torchwood config init` | 创建带注释的模板配置（已存在则报错） |
| `torchwood config list` | 列 profile（key 打码，JSON：configPath/default/profiles[]） |
| `torchwood config show <profile>` | 单个 profile 字段（key 打码） |
| `torchwood config set [--stdin] <profile> <key> <value>` | 写字段；key ∈ endpoint/api-key/tls/timeout/output；`--stdin` 放在位置参数前，配合 api-key 避免进 shell 历史 |
| `torchwood config use <profile>` | 设缺省 profile |
| `torchwood config remove <profile>` | 删 profile（删缺省时一并清 default） |

## projects（项目）

- `projects list [--page-size N] [--page-token T]`
- `projects get <project-id>`

## users（用户）

- `users list [--queries '[...]'] [--page-size N] [--page-token T]`
- `users get <user-id>`
- `users create --email <e> --password <p> [--name <n>]`
- `users update <user-id> [--name] [--email] [--status] [--email-verified] [--data '{...}']`（仅显式传入的字段生效；清空字段用 `--data`）
- `users update-password <user-id> --password <p>`
- `users delete <user-id>`
- `users sessions list <user-id>` / `users sessions delete <user-id> <session-id>`
- `users tokens create <user-id>`（模拟登录，签发 access/refresh token）

## groups（分组）

- `groups create --name <n>` / `groups list` / `groups get <id>` / `groups delete <id>`
- `groups prefs get <id>` / `groups prefs update <id> --data '{...}'`
- `groups memberships create <group-id> [--user-id <uid> | --email <e>]`
- `groups memberships list <group-id>` / `groups memberships get <gid> <mid>`
- `groups memberships update <gid> <mid> --roles '["owner"]'`
- `groups memberships update-status <gid> <mid> --status active|blocked`
- `groups memberships delete <gid> <mid>`

## databases（数据库 / 集合 / 属性 / 索引 / 文档）

- `databases create --id <id> --name <n>` / `list` / `get <id>` / `delete <id>`
- `databases collections create <db> --id <id> --name <n> [--permissions '["read(\"users\")"]'] [--document-security true|false] [--disabled]`
- `databases collections list <db> [--page-size] [--page-token]`
- `databases collections get|update|delete`（update 仅显式字段：`--name --permissions --document-security --disabled`）
- `databases attributes create <db> <coll> --key <k> --type string|integer|float|boolean|datetime [--size N] [--required] [--array] [--default-value '<json>']`
- `databases attributes delete <db> <coll> <key>`
- `databases indexes create <db> <coll> --id <id> --type key|unique|fulltext --attributes '["a","b"]' [--orders '["asc","desc"]']`
- `databases indexes delete <db> <coll> <index-id>`
- 文档：
  - `databases documents create <db> <coll> --data '{...}' [--document-id id] [--permissions '[...]']`
  - `databases documents list <db> <coll> [--queries '[...]'] [--page-size N] [--page-token T]`
  - `databases documents get <db> <coll> <doc-id>`
  - `databases documents update <db> <coll> <doc-id> --version <n> [--data '{...}'] [--permissions '[...]'] [--increment '{"views":1}']`（OCC；至少一项变更）
  - `databases documents upsert <db> <coll> <doc-id> --data '{...}' [--conflict-columns '["col"]']`
  - `databases documents delete <db> <coll> <doc-id> --version <n>`
  - `databases documents count <db> <coll> [--queries '[...]']`
  - `databases documents bulk-update <db> <coll> --document-ids '["a","b"]' --data '{...}'`
  - `databases documents bulk-delete <db> <coll> --document-ids '["a","b"]'`

### 查询 DSL（--queries JSON 数组，每元素一个字符串）

- 过滤：`equal("k","v")`、`notEqual`、`greaterThan`、`greaterThanEqual`、`lessThan`、`lessThanEqual`、`contains`、`search`
- 数组属性专用（`--array` 属性 + GIN 索引）：`containsAny("k","a","b")`、`containsAll("k","a","b")`
- 排序分页：`orderAsc("k")`、`orderDesc("k")`、`limit(20)`、`offset(40)`
- 系统字段别名：`$id`/`$createdAt`/`$updatedAt`/`$version`
- 向量近邻是 typed builder 专用（KNN），CLI/DSL 字符串不支持

## storage（存储元数据；上传/下载走 HTTP handler，不经 CLI）

- `storage buckets create --name <n> [--public]` / `list` / `get <id>` / `update <id> [--name] [--public]` / `delete <id>`
- `storage files list <bucket-id> [--queries] [--page-size] [--page-token]` / `get <b> <f>` / `update <b> <f> [--name] [--mime-type] [--metadata '{...}']` / `delete <b> <f>`
- `storage usage`

## functions（函数）

- `functions runtimes` / `functions specifications`
- `functions create --id <id> --name <n> --runtime <rt> [--entrypoint] [--timeout-seconds] [--spec]`
- `functions list` / `get <fid>` / `update <fid> [--name] [--entrypoint] [--timeout-seconds] [--spec] [--enabled]` / `delete <fid>`
- `functions deployments create <fid> --code <zip-file>`（≤8MiB）/ `list <fid>` / `get <fid> <dep-id>` / `delete <fid> <dep-id>`
- `functions variables get <fid>` / `functions variables set <fid> --vars '{"K":"V"}'`
- `functions executions create <fid> [--input '<json>'] [--async]` / `functions executions list <fid>` / `functions executions get <fid> <eid>`

## oauth-providers（OAuth 提供商）

- `oauth-providers list`
- `oauth-providers upsert <provider> --client-id <id> --client-secret <secret> ...`（enabled/scopes 等 flag 见 `-h`）
- `oauth-providers delete <provider>`

## admin（平台运维；outbox 走 API Key，其余直连 DB）

- `admin outbox list-dead [--page-size] [--page-token]` / `admin outbox replay <event-id>`
- `admin export` / `admin import` / `admin schema` / `admin sync-roles-sig`：直连数据库，走 `--dsn`/环境变量，不经 API Key

## rpc（逃生舱）

- `torchwood rpc <full-grpc-method> [--data '<protojson>']`
- 方法名形如 `/torchwood.server.v1.UsersService/ListUsers`；请求体 camelCase、字段可省略
- 覆盖尚未出专用命令的 RPC；APIKeysService 被服务端禁止经 API Key 调用
