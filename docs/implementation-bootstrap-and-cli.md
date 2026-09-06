# 实现方案：首个管理员 Bootstrap + Torchwood CLI（cmd/client）

> 状态：待实施（方案已评审，拆分为 3 个可分派的实施 prompt，见 `docs/prompts/`）
> 创建日期：2026-08-10
> 关联：`docs/roadmap.md` §2.9（Seed 数据增强，本方案以「移除 seed」取代）、§0 Agent-Native（CLI 是 Agent/自动化调用的入口之一）
>
> **2026-09-06 更新**：CLI 框架已由 `spf13/cobra` 迁移至 `github.com/lynx-go/commands`（零依赖子命令框架，§4.2/§5 等涉及 cobra 的描述以现状代码 `cmd/client/cmd/` 为准）。行为差异：全局旗标须在子命令路径之后、位置参数之前给出；`--version` 改为 `version` 动词；shell completion 不再提供；退出码契约不变（0/1/2=40x/3=5xx/4=429，经 `commands.ExitCode` 钩子注入）。

---

## 1. 背景与目标

当前系统第一个 Console 管理员、默认 project、默认 API Key 只能由离线脚本 `cmd/seed` 写入数据库，存在三个问题：

- 固定凭据（`admin@torchwood.local / Admin@123`）容易被遗忘在线上环境；
- seed 不经过用例层，绕过权限与校验逻辑（如未写 `admin_projects` 关联）；
- 首次部署体验割裂：要先跑 CLI 脚本再进 Console。

本方案做两件事：

1. **Bootstrap**：移除 `cmd/seed`；Console 提供公开的首个管理员注册端点，第一个注册的管理员自动成为超管（owner），注册时须指定 project id 与 database id。API Key 不在注册时生成，登录后在 Console 的 API Key 页面创建。
2. **CLI**：新增 `cmd/client`（二进制名 `torchwood`），通过 API Key 调用服务端 gRPC 接口，覆盖 Server API 的主要资源。

## 2. 现状关键事实（调研结论，实施时以代码为准）

- `proto/console/v1/auth.proto` 的 `ConsoleAuthService` 默认 `ACCESS_PUBLIC`，仅有 `SignIn/RefreshToken/SignOut`，**无注册端点**。
- `AdminsService.CreateAdmin` 受 `permissions: ["owner"]` 保护，不能用于创建第一个管理员。
- `internal/app/console/admins.go` 的 `Admins.Create` **无 principal 检查**，校验邮箱唯一、密码强度（`users.ValidatePasswordStrength`）、角色合法后可创建。
- `internal/app/server/apikeys.go` 的 `APIKeys.Create` **无 principal 检查**；secret 为两个 UUID 拼接（64 字符），存 SHA-256 hex，明文仅创建时返回。
- `internal/app/server/projects.go` 的 `Projects.CreateProject` **要求平台 admin principal**（安全评审 M7），内部在 `RunInTx` 中插入 project 并调用 `docDB.EnsureSystemCollections`（创建 `default` schema + 7 个系统集合）。
- 有效 API Key scope：`*` / `all` / `<resource>[.read|.write]`，资源共 8 个（databases/users/groups/storage/projects/oauthproviders/apikeys/functions），见 `pkg/grpc/interceptor/apikey_scope.go`。
- API Key 认证：metadata `x-api-key: <secret>`（或 `Authorization: ApiKey <secret>`）；`X-Torchwood-Project` **仅对 admin console session 有效**，API Key 调用不需要。
- 拦截器**禁止 API Key 调用 `APIKeysService`**（防止自铸新 key），见 `pkg/grpc/interceptor/jwt.go`。
- gRPC 明文监听 `127.0.0.1:9060`（仅回环），grpc-gateway 在 `:9080` 以 insecure 转发；Storage 文件上传/下载是独立 HTTP handler（`internal/api/serverhttp`），不在 gRPC 面。
- 无 cobra 等 CLI 框架依赖；现有入口用 `spf13/pflag` + `viper` + `lynx.NewRunner`。
- `cmd/seed` 被以下文档引用，移除时需同步更新：
  - `README.md`、`README_ZH.md`（快速开始 + 目录树）
  - `docs/developer/01-overview.md`、`02-quickstart.md`、`03-configuration.md`、`05-authentication.md`、`12-sdk.md`、`13-operations.md`
  - `docs/roadmap.md` §2.9（「Seed 数据增强」行）
  - `docs/manual-acceptance-checklist.md` 0.4

## 3. Bootstrap 设计

### 3.1 Proto 变更（`proto/console/v1/auth.proto`）

`ConsoleAuthService` 新增两个 RPC（继承服务级 `ACCESS_PUBLIC`）：

```proto
// 查询是否已完成初始化（admins 是否为空）
rpc GetSetupStatus(GetSetupStatusRequest) returns (GetSetupStatusResponse) {
  option (google.api.http) = { get: "/v1/console/auth/setup-status" };
}

// 首个管理员注册：仅当 admins 为空时可用
rpc SignUp(SignUpRequest) returns (SignUpResponse) {
  option (google.api.http) = { post: "/v1/console/auth/sign-up", body: "*" };
}

message GetSetupStatusRequest {}
message GetSetupStatusResponse { bool needs_setup = 1; }

message SignUpRequest {
  string email = 1;
  string password = 2;
  string setup_token = 3;
  string project_id = 4;
  string database_id = 5;
}

message SignUpResponse {
  Admin admin = 1;                    // 复用 admins.proto 的 Admin 消息
  string access_token = 2;            // 与 SignInResponse 一致，便于前端统一处理
  string refresh_token = 3;
  reserved 4;
  reserved "default_api_key_secret";
}
```

注意：`SignUp` 是公开端点，必须在 use-case 层保证「仅首次可用」，不依赖拦截器。

### 3.2 Use-case：新增 `internal/app/console/setup.go`

```go
type Setup struct {
    admins    *Admins            // 复用 Create（无 principal 检查）
    projects  *server.Projects   // 复用 CreateProjectInternal（见 3.3）
    databases *server.Databases  // 复用 CreateDatabase（注入 bootstrap principal）
    auth      *Auth              // 复用 SignIn 签发 TokenPair
    adminRepo projects.AdminRepository
    // admin_projects 关联写入（repo 已有或新增方法）
}

type SignUpResult struct {
    Admin  *projects.Admin
    Tokens *TokenPair
}

func (s *Setup) GetSetupStatus(ctx context.Context) (bool, error)
func (s *Setup) SignUp(ctx context.Context, cmd SignUpCommand) (*SignUpResult, error)
```

`SignUp` 流程（顺序与补偿）：

1. **首次性检查**：`admins` 计数 > 0 → `FailedPrecondition("setup already completed")`。
2. 校验 `project_id` / `database_id`（`ident.ValidateSchemaResourceID`）。
3. 调用 `Admins.Create(ctx, {Email, Password, Role: "owner"})` —— 复用邮箱/密码/角色校验。
4. 调用 `Projects.CreateProjectInternal(ctx, {ID: project_id, Name: project_id})` —— 内含 `RunInTx` + `EnsureSystemCollections`（自动创建系统 `default` 库）。
5. 若 `database_id != "default"`，调用 `Databases.CreateDatabase` 创建业务库。
6. 写入 `admin_projects` 关联（owner 实际会被 `ValidateAdminProjectAccess` 放行，但保持数据完整）。
7. 调用 `Auth.SignIn(ctx, {Email, Password})` 签发 TokenPair（handler 侧复用 `setSessionCookies`）。

不创建 API Key；登录后由 Console 的 API Key 页面生成。

**失败补偿**：步骤 4-7 任一步失败时，best-effort 回删已创建的 admin / project / 业务库（避免「admin 已建但 project 缺失」导致无法重试也无法登录的死锁），然后返回错误。补偿失败只记日志。

**并发**：两个并发 SignUp 可能同时通过步骤 1。以 Postgres advisory lock（`pg_advisory_xact_lock`）串行化首次性检查与首个 admin 创建。

### 3.3 `Projects.CreateProject` 最小重构

`internal/app/server/projects.go`：

- 保留 `CreateProject(ctx, cmd)`：principal 校验（平台 admin）原样不动，校验通过后委托给新方法。
- 新增 `CreateProjectInternal(ctx, cmd)`：包含原方法中 name/description 校验、ID 生成与 `RunInTx` 逻辑，**不做 principal 检查**，注释注明「供 bootstrap 等系统路径调用，调用方负责授权」。

行为对现有调用方零变化。

### 3.4 API 层与 Wire

- `internal/api/consolegrpc/auth.go`：`AuthService` 增加 `GetSetupStatus` / `SignUp` 两个 handler；`SignUp` 成功后调用现有 `setSessionCookies`。
- 新依赖注入后执行 `task wire-all`；proto 变更后执行 `task generate-proto`。

### 3.5 Console 前端

最小改动方案（不新增路由）：

- `console/src/api/auth.ts` 增加 `getSetupStatus()`、`signUp({email, password, project_id, database_id})`。
- `console/src/routes/Login.tsx`：挂载时查询 `setup-status`；`needs_setup=true` 时切换为「初始化设置」表单（email/password/确认密码/project id/database id，文案区分），提交调 `sign-up`，成功后 cookie 已设置，直接进入 Console。
- 不在引导页展示 API Key；登录后到 API Keys 页面创建（secret 仅创建时展示一次）。

### 3.6 移除 cmd/seed

- 删除 `cmd/seed/` 目录。
- 全仓 grep `cmd/seed`、`go run ./cmd/seed`、`seed`，更新 §2 列出的所有文档：
  - 快速开始改为：启动服务 → 打开 `/console/` → 按引导填写 project id / database id 完成首个管理员注册；API Key 在登录后的 API Keys 页面创建。
  - `docs/roadmap.md` §2.9 的「Seed 数据增强」行标记为「已由首个管理员 bootstrap 取代，cmd/seed 移除」。
  - `docs/manual-acceptance-checklist.md` 0.4 改为 bootstrap 验收步骤。
  - `AGENTS.md` 如有涉及同步更新。

### 3.7 测试

- 单元测试 `internal/app/console/setup_test.go`：
  - 首次 SignUp 成功：admin=owner、指定 project/database 已创建、tokens 有效、未创建 API Key。
  - 二次 SignUp 返回 `FailedPrecondition`。
  - project / database 创建失败时 admin 被回删（mock 注入失败）。
- 集成测试：通过 gateway `POST /v1/console/auth/sign-up` 全流程；随后 `GET /v1/console/auth/setup-status` 返回 `needs_setup=false`；项目与数据库存在且 `api_keys` 为空。

## 4. CLI（cmd/client）设计

### 4.1 定位与边界

- 二进制名 `torchwood`，构建产物 `bin/torchwood[.exe]`。
- 通过 gRPC（非 HTTP gateway）调用 Server API，认证一律走 `x-api-key` metadata。
- **明确边界**（来自现有安全设计，CLI 不绕过的）：
  - `APIKeysService` 不接受 API Key 调用 → CLI 不提供 api-keys 命令。
  - `CreateProject` 限平台 admin（console session）→ CLI 仅提供 `projects list/get`，不提供 create/update。
  - Storage 文件上传/下载是独立 HTTP handler → CLI MVP 只做 bucket 级 gRPC 命令，文件传输留待后续。
- gRPC 默认绑 `127.0.0.1:9060`；远程使用需调整 `server.grpc.addr` 或走 SSH 隧道，文档注明。服务端目前明文，CLI 默认 insecure 拨号，`--tls` 留作占位（暂返回未支持错误）。

### 4.2 依赖与结构

新增依赖 `github.com/spf13/cobra`（与现有 pflag/viper 同族，风格一致）。**不使用 Wire**（无复杂依赖图），代码全部放在 `cmd/client/` 下：

```
cmd/client/
  main.go            # 执行入口（godotenv + cmd.NewRootCmd）
  cmd/
    root.go          # root command、全局 flag、api-key 校验、根命令组装
    output.go        # invoke（InvokeJSON）+ JSON 渲染
    helpers.go       # JSON 解析/合并 helper
    health.go        # health get / version
    projects.go      # projects list / get
    users.go         # users 子命令
    databases.go     # databases/collections/documents 子命令
    groups.go         # groups/memberships 子命令
    storage.go       # buckets 子命令
    functions.go     # functions 子命令
    oauth.go         # oauth-providers 子命令
    rpc.go           # 通用调用：torchwood rpc <full-method> [--data JSON]
```

### 4.3 全局参数与配置

| Flag | 环境变量 | 默认值 | 说明 |
|------|----------|--------|------|
| `--endpoint` | `TORCHWOOD_CLI_ENDPOINT` | `127.0.0.1:9060` | gRPC 地址 |
| `--api-key` | `TORCHWOOD_CLI_API_KEY` | 无 | API Key secret；除 `health` 外必填，缺失报清晰错误 |
| `--timeout` | `TORCHWOOD_CLI_TIMEOUT` | `30s` | 单次调用超时 |
| `--output` | `TORCHWOOD_CLI_OUTPUT` | `json` | 输出格式（MVP 仅 json） |

认证注入：unary client interceptor 向 outgoing metadata 写 `x-api-key`。**不传** `X-Torchwood-Project`（对 API Key 无意义）。

### 4.4 命令树

```
torchwood health get                     # HealthService（公开，无需 key）
torchwood health version

torchwood projects list [--page-size N] [--page-token T]
torchwood projects get <id>

torchwood users list|get|create|update|delete ...
torchwood users sessions list <user-id> / delete <user-id> <session-id>

torchwood databases list|get|create ...
torchwood databases collections list|get|create ...
torchwood databases documents list|get|create|update|delete ...

torchwood groups ... / memberships ...
torchwood storage buckets list|get|create|update|delete ...
torchwood functions list|get|create|execute ...
torchwood oauth-providers list|get|update

torchwood rpc <full-method> [--data '<json>']   # 逃生舱：覆盖全部 83 个方法
```

- 请求参数优先用具名 flag（如 `users create --email --password [--name]`）；复杂结构（如 document 的 data JSON、queries 数组）接受 `--data` / `--queries` JSON 字符串，用 `protojson.Unmarshal` 合并进请求。
- `rpc` 子命令用动态消息（`protocmp`/`dynamicpb` 不可行时允许退化为：请求直接 `protojson.Unmarshal` 到对应方法生成的请求类型——通过一张 `method -> func() proto.Message` 注册表实现，该注册表同时服务具名命令的测试）。
- 错误处理：gRPC `status` 错误打印 `code + message` 到 stderr，退出码非 0（`PermissionDenied` 时提示检查 scope）。
- 成功输出：响应 message 的 protojson（缩进 2 空格）到 stdout。

### 4.5 构建与工程化

- `Taskfile.yml`：`build` 任务增加 `cmd/client` 产物（参照 server/worker 现有写法）；如适用加 `dev-client`。
- `go mod tidy` 纳入 cobra。
- `AGENTS.md` 补充一行 cmd/client 说明；`docs/developer/01-overview.md` 目录树与组件表同步。

### 4.6 测试

- 单元测试：各命令的「flag → request message」构造函数（table-driven），`rpc` 的 method 注册表完整性（遍历注册表，确认每个方法都能构造请求类型）。
- 集成测试（可选，`internal/testutil` 或起真实 server）：用 bootstrap 得到的 key 跑 `health get` 与 `users list` 冒烟。

## 5. 实施拆分（分派 prompt）

| Prompt | 文件 | 内容 | 依赖 |
|--------|------|------|------|
| A | `docs/prompts/bootstrap-first-admin.md` | §3 全部：proto、use-case、前端、移除 seed、文档与测试 | 无 |
| B | `docs/prompts/cli-framework.md` | §4.2/4.3/4.5 + health/projects/users/rpc 命令（§4.4 部分） | 无（与 A 并行） |
| C | `docs/prompts/cli-resources.md` | §4.4 其余资源命令：databases/groups/storage/functions/oauth-providers | B 完成后 |

A 与 B 无代码交集（A 不碰 cmd/client，B 不碰 console/bootstrap），可并行分派；C 依赖 B 的框架模式。

## 6. 验收标准

**Bootstrap**

- 全新数据库上启动 server，打开 `/console/` 出现初始化引导；填写 project id / database id 注册后即为 owner 并直接进入 Console。
- 指定 project 与 database 已创建；未生成 API Key。登录后在 API Keys 页面创建密钥，再用 `x-api-key` 调用 `ListUsers` 等接口。
- 再次调用 `sign-up` 返回 `FailedPrecondition`；`setup-status` 返回 `needs_setup=false`。
- `cmd/seed` 目录与文档引用全部清除；`task test`、`task build`、`task console-build && task build` 通过。

**CLI**

- `torchwood health get` 无需 key 成功；`torchwood users list --api-key <bootstrap key>` 返回 JSON。
- 无 scope 的 key 调用写命令返回清晰的 `PermissionDenied` 提示。
- `torchwood rpc /torchwood.server.v1.UsersService/ListUsers --data '{}'` 与具名命令结果一致。
- `task build` 产出 `bin/torchwood`；`task test` 通过。
