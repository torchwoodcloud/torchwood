# 实施 Prompt B：Torchwood CLI（cmd/client）框架 + 核心命令

> 将本文件整体作为任务说明分派给实施 agent。仓库路径：`D:/Codes/qiulin/torchwood`
> 完整设计文档：`docs/implementation-bootstrap-and-cli.md` §4、§6（**先通读再动手**，以代码现状为准）
> 注意：本任务与「首个管理员 Bootstrap」任务并行，**不要**触碰 `internal/app/console`、console 前端或 console proto（Bootstrap 已移除 `cmd/seed`，CLI 冒烟测试改用 bootstrap 引导产出的默认 API Key）。

---

## 任务目标

新建 `cmd/client`（二进制名 `torchwood`），通过 API Key 走 gRPC（非 HTTP gateway）调用 Server API。本 prompt 覆盖：CLI 框架（拨号/认证/输出/全局参数）、`health`、`projects`、`users` 具名命令、通用 `rpc` 逃生舱命令，以及构建接入与测试。其余资源命令由后续 prompt 补齐。

## 关键现状（已调研核实，实施时复核）

- 服务端 gRPC 明文监听 `127.0.0.1:9060`（仅回环），无 TLS。
- 生成代码：`genproto/server/v1`（包名 `serverv1`），含全部 `NewXxxServiceClient`；模块名 `github.com/torchwoodcloud/torchwood`。
- 认证：outgoing metadata `x-api-key: <secret>`；**不要传** `X-Torchwood-Project`（仅对 admin console session 有效）。
- API Key 无法调用的面（CLI 不提供对应命令）：`APIKeysService`（拦截器禁止）、`CreateProject`（use-case 限平台 admin）。
- `pkg/grpc/` 下只有 interceptor，无现成 client 封装，需自行拨号。
- `proto/server/v1` 共 9 个服务 83 个 RPC；本 prompt 涉及：`HealthService`（2，公开）、`ProjectsService`（4，CLI 只做 list/get）、`UsersService`（9）。
- 无 CLI 框架依赖；新增 `github.com/spf13/cobra`（与现有 pflag/viper 同族）。**不使用 Wire**。
- `cmd/worker/main.go` 是 cmd 入口风格参考（godotenv、pflag、配置绑定），但 CLI 不加载服务端 configs/。

## 实施步骤

1. **框架**：按设计文档 §4.2 建 `cmd/client/`（`main.go`、`conn.go`、`output.go`、`cmd_rpc.go` 等）：
   - 全局 flag/环境变量：`--endpoint`/`TORCHWOOD_CLI_ENDPOINT`（默认 `127.0.0.1:9060`）、`--api-key`/`TORCHWOOD_CLI_API_KEY`、`--timeout`（30s）、`--output json`。见设计文档 §4.3。
   - `conn.go`：insecure 拨号 + unary client interceptor 注入 `x-api-key`；`health` 命令豁免 api-key 必填校验；`--tls` 占位（使用时返回未支持错误）。
   - `output.go`：`protojson.MarshalOptions{Multiline: true, Indent: "  "}` 渲染响应到 stdout；gRPC status 错误打印 `code + message` 到 stderr、非 0 退出码，`PermissionDenied` 时附加 scope 提示。
2. **通用 `rpc` 命令**：`torchwood rpc <full-method> [--data '<json>']`；用一张 `method -> func() proto.Message` 注册表把 `--data` protojson 反序列化到对应请求类型后发起 unary 调用。注册表至少覆盖 `proto/server/v1` 全部方法（除 `APIKeysService`），供本命令与后续资源命令复用；写代码生成式的手工注册均可，但必须有测试保证完整性（遍历 `serverv1` 全部服务方法名与注册表比对）。
3. **具名命令**：
   - `torchwood health get`、`torchwood health version`（无需 key）。
   - `torchwood projects list [--page-size] [--page-token]`、`torchwood projects get <id>`。
   - `torchwood users` 覆盖 `UsersService` 全部 9 个方法（list/get/create/update/delete/sessions/tokens 等，按 proto 实际方法命名子命令）；标量参数用具名 flag，复杂结构（labels/prefs 等 Struct）接受 `--data` JSON 合并。
4. **构建接入**：`Taskfile.yml` 的 `build` 任务增加 `cmd/client` 产物 `bin/torchwood[.exe]`（参照 server/worker 写法）；`go mod tidy`。
5. **文档**：`docs/developer/01-overview.md` 目录树与组件表加 cmd/client；`AGENTS.md` 项目结构补充一行；如开发者文档有 CLI 使用位置（02-quickstart/09-api-guide）各加一小节用法示例。
6. **测试**：table-driven 单测覆盖「flag → request message」构造函数；`rpc` 注册表完整性测试；错误路径（缺 api-key、PermissionDenied）测试。

## 约束

- 遵守 `AGENTS.md`：最小改动、代码风格与现有 cmd 入口一致、中文注释。
- 除 cobra 外不引入新依赖；不改服务端任何代码与 proto。
- 不实现 Storage 文件上传/下载（独立 HTTP handler，不在 gRPC 面，后续单排）。
- 不实现 api-keys 与 projects create/update 命令（安全设计边界，见上）。

## 完成验收

- `task build` 产出 `bin/torchwood`；`torchwood --help` 输出完整命令树。
- 本地栈（`task up && task migrate && task dev-server`）下：
  - `torchwood health get` 无需 key 成功输出 JSON；
  - `torchwood users list --api-key <有效key>` 成功；无效 key 返回清晰错误；
  - 无 `users.write` scope 的 key 调 `users create` 返回 `PermissionDenied` 及 scope 提示；
  - `torchwood rpc /torchwood.server.v1.UsersService/ListUsers --data '{}'` 与 `users list` 结果一致。
- `task test` 通过。
- 完成后输出：变更文件清单、命令树快照、每个验收项的验证方式与结果、遗留问题（特别是留给后续 prompt 的注册表/模式说明）。
