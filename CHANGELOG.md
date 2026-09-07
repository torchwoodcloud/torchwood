# Changelog

本文件记录独立分发的子模块版本。模块遵循 Go nested-module tagging：
`genproto/vX.Y.Z` tag 承载 `github.com/torchwooddev/torchwood/genproto`，
`sdk/go/vX.Y.Z` tag 承载 `github.com/torchwooddev/torchwood/sdk/go`。
TypeScript SDK 以 npm 包 `@torchwood/sdk` 分发（`sdk/typescript/`，`task sdk:publish` 发布）。
发布流程见 `.github/workflows/release.yml`（workflow_dispatch）。

## @torchwood/sdk

### v0.2.0 — 2026-09-07

**破坏性版本（0.x minor 携带破坏性，A10 决议）**：收拢 npm 0.1.0（2026-09-01）以来服务端发生的全部契约断裂。**请与 0.2.0 世代服务端同批升级**（兼容承诺见迁移说明末尾）。

**变更**：

- 查询栈对齐 C7 单 AST：过滤唯一载体为 `query`（typed 构造器 + `FromDSL` 糖）；`queries` 字符串字段随服务端 reserved 退役（R11 文法 parity golden 锁定）；
- 新增 ListChanges（client + server 双面，阶段④补偿 API，R17 补登）；
- 新增 VectorSearch typed builder（KNN：metric / maxDistance / `kvc:` 多页）与 `efSearch`（B7，取值域 [1,500]，越界显式拒绝）；
- 新增 B4 schema 演进三方法（`:migrate` / `:restore` / `:retire`，`AttributeMigration` 类型）与集合契约 JSON Schema 导出（B10，`GET .../collections/{c}:exportSchema?as=jsonschema`）；
- 错误面对齐域码体系：`DOCUMENT.VERSION_CONFLICT` metadata 携带 `current_version`（升级后可免重读直接重试）；
- `agentTools` 目录不变（18 动词）。

**迁移说明（0.1.0 → 0.2.0，底稿 = `docs/design/poc-to-release-migration.md` §7.1）**：

| 断裂项 | 旧形态 | 新形态 | 迁移动作 |
|---|---|---|---|
| 分页 | offset 族 token、`offset()` 构造器 | keyset-only（`ka:`/`kb:` + docID 完整游标） | 丢弃在途 token 从首页重取；`offset()` 调用点改 orders + cursor |
| total | 每页精确 total | 续页 `total=0=unknown` | 首页取 total，或独立 count 接口 |
| `queries` DSL 字段 | List 请求 `queries` 字符串（部分接口曾被静默忽略） | 字段 reserved；唯一过滤 = `query` AST | **最危险档：旧 SDK 的过滤被 proto 运行时静默忽略（结果集变大、无报错）**——SDK 与服务端同批升级；升级期间禁止依赖 queries 的旧客户端执行写操作 |
| 静态面 `filter/order_by` | 恒拒过渡态 | reserved | 静态面过滤走参数面 |
| 错误判别 | message 文本；`expected_version=0` 错位 | 域码 + `retryable`；三态：缺省拒（VERSION_REQUIRED）/ ≤0 InvalidArgument（VERSION_INVALID）/ 冲突 FailedPrecondition（VERSION_CONFLICT，可重试） | 错误处理改按 code（域码）+ retryable 判别，禁止匹配 message 文本 |
| 不可见文档 | 403 PERMISSION_DENIED | 404 NOT_FOUND（防枚举，有意翻转） | 按 404 处理"不可见"（与不存在同语义）；upsert 分支改用 exists 探测 |
| Upsert 并发冲突 | 命中行自动转 update | 分支两侧分别裁决；并发撞唯一键 → ALREADY_EXISTS（可重试） | 直接重试（`request_id` 幂等已覆盖） |
| 新增面 | — | execute-tx、aggregate、`:changes`+last_seq、vectorSearch、`request_id` 写幂等 | 纯增量，无迁移义务，按需采纳 |

**兼容承诺（A10 决议）**：不承诺"旧 SDK × 新服务端"组合；服务端承诺支持最近两个 SDK minor（N 与 N-1）。**1.0.0 与转出门禁 A 区清零绑定**（`docs/developer/15-exit-poc.md` A10），自此同 major 内契约冻结。

### v0.1.0 — 2026-09-01

首个 npm 发布版本。ESM-only（Node >= 18），零运行时依赖，`tsc` 直出 `dist/`
（发布产物不含 `__tests__`，`prepublishOnly` 自动跑测试与干净构建）。

- Client API 客户端（Bearer JWT，token 自动持久化/刷新）：account（注册/
  登录/会话/偏好）、databases（文档 CRUD + count）、groups / memberships、
  realtime（WebSocket 订阅）、assets、payments、subscriptions；
- Server API 客户端（API Key + `X-Torchwood-Project`）：health / projects /
  users / groups / databases（库/集合/属性/索引/文档/Bulk）/ apiKeys /
  oauthProviders / storage / functions / payments / assets / subscriptions /
  billing / outbox；
- Agent 默认工具目录：18 个动词 → Server RPC 的映射（`agentTools` /
  `lookupAgentTool` / `TOOL_*` 常量），供 LLM Agent 与 MCP Tool Server 使用；
- 门面 `Torchwood.withApiKey()` / `Torchwood.withAccessToken()` /
  `Torchwood.create()` 三种实例化方式。

## sdk/go

### v0.2.0 — 2026-09-07

**破坏性版本**：跟随 genproto v0.2.0（tag `sdk/go/v0.2.0`，genproto @ `genproto/v0.2.0`，`release.yml` 流水线发布）。迁移说明与兼容承诺见 `@torchwood/sdk` v0.2.0 小节（同底稿、同批升级要求）。

- typed 查询构造器 + `FromDSL` sugar——`queries` 字符串字段退役后的唯一过滤载体（`sdk/go/query`，与 `pkg/query` 文法 parity golden 锁定）；
- 新增服务封装：`ExecuteTransactions`（事务批，ATOMIC/PARTIAL）、`AggregateDocuments`、`ListChanges`（`:changes` 补偿）、`ExportCollectionSchema`、`MigrateAttribute` / `RestoreAttribute` / `RetireAttribute`（B4 schema 演进生命周期）；
- `VectorSearch` typed builder：metric / maxDistance / 多页 `kvc:` 距离游标（B2）/ `EfSearch`（B7，[1,500]）；
- 错误面对齐域码体系：`VersionConflictError` 携带 `current_version`、retryable 静态判定、不可见文档 403→404 语义；
- `DocumentsPager` 等列表分页全面切换 keyset（`pageToken`）。

### v0.1.2 — 2026-08-26

流水线端到端验证发布（tag `sdk/go/v0.1.2`，genproto @ `genproto/v0.1.2`）：
release.yml 修复后**首次全程绿灯**（rewrite → tidy → build → tag → CI 内
干净目录下游验收）。模块内容与 v0.1.1 一致（其间无 sdk/go 变更），无功能
差异；随附流水线验收脚本补 `go mod tidy`（`go get pkg@ver` 只写根模块
go.sum 条目，直接 build 缺传递 sum）。下游可按需停留在 v0.1.1。

### v0.1.1 — 2026-08-26

首次经 `.github/workflows/release.yml` 成功发布的版本（tag `sdk/go/v0.1.1`，
genproto @ `genproto/v0.1.1`）：require 改写为真实 genproto 版本并移除本地
相对路径 replace，下游 `go get github.com/torchwooddev/torchwood/sdk/go@v0.1.1`
可正常解析编译（干净目录验收通过）。

- 修复 v0.1.0 的分发断裂：v0.1.0 为手动 tag，go.mod 仍含本地 replace 与
  伪版本，下游无法解析——请直接使用 v0.1.1；
- client SDK `CountDocuments` 改用独立 `CountDocumentsRequest`（R4 P3-9
  proto 变更的漏改，v0.1.1 发布流水线首次完整编译 sdk/go 时暴露并修复）；
- 包含自 v0.1.0 tag（2026-08-12）以来的 SDK 增强：默认 30s 超时兜底与
  `Unavailable` 指数退避重试、`InvokeTool` Agent 工具目录、Client SDK
  Realtime 订阅 API、Outbox dead-letter list/replay、`DocumentsPager`
  分页迭代器、storage 传输 helper、错误 helper 全家桶；
- 发布流水线三项修复随附：go.mod require/验收命令改用纯版本号（nested
  module tag 前缀由 Go 解析）、tidy/验收改走默认 module proxy（vanity
  站点不稳定不再阻塞发布）、验收探针 `Health.Check` 双返回值修正。

### v0.1.0 — 2026-08-24（tag 实际打于 2026-08-12，手动）

首个 tag。**不可用于下游解析**：go.mod 仍含本地 replace 与伪版本，
功能上由 v0.1.1 完整取代（v0.1.0 小节历史描述的增强实际随 v0.1.1 发布）。

- Server API 客户端（`x-api-key` + `x-torchwood-project`）：Health / Users /
  Groups / Databases / Projects / Storage / Functions / OAuthProviders /
  Payments / Assets / Subscriptions / Billing / Outbox 13 个类型化服务封装；
- Client API 客户端（Bearer JWT 自动刷新 + FileTokenStore 原子持久化）；
- `InvokeJSON` 动态分发：覆盖全部 `torchwood.server.v1.*` unary（排除
  APIKeysService），proto 新增方法零登记自动可用。

## genproto

### v0.2.0 — 2026-09-07

跟随 sdk/go v0.2.0 发布（tag `genproto/v0.2.0`）。相对 v0.1.2 的主要断裂与新增：

- **C7 查询单栈**：`queries`（client/server databases 两面）与 `ListRequest.filter/order_by`（静态面）`reserved`——唯一过滤载体 = `shared.v1.Query` typed AST；
- **新增 RPC**：`ExecuteTransactions`、`AggregateDocuments`、`ListChanges`、`ExportCollectionSchema`、`MigrateAttribute` / `RestoreAttribute` / `RetireAttribute`；
- **vector**：`vector` 属性类型（dims 2..2000）、`VectorSearch`（KNN 一等算子：metric / max_distance / ef_search / `kvc:` 多页游标）、hnsw 索引 `distance_metric`；
- **数组**：`ArrayUpdate` 八算子（`array_updates`，仅 update 消费）+ `containsAny` / `containsAll` 查询算子（仅 array=true 属性）；
- **错误契约**：域码 + `retryable`（ErrorInfo metadata）；`DOCUMENT.VERSION_CONFLICT` 携带 `current_version`；不可见文档 403→404（防枚举）；`expected_version` 三态（缺省拒 / ≤0 InvalidArgument / 冲突 VERSION_CONFLICT）；
- **protovalidate**：buf.validate 请求形状注解全量上收（`ValidateInterceptor` 链尾统一求值）；
- **authz**：`method_auth` / `service_auth` 策略注解全量进 proto（策略唯一声明源 → PolicySet 语义断言）。

详见 `git log genproto/v0.1.2..genproto/v0.2.0`。

### v0.1.2 — 2026-08-26

跟随 sdk/go v0.1.2 发布；内容与 v0.1.1 一致（流水线验证）。

### v0.1.1 — 2026-08-26

跟随 sdk/go v0.1.1 发布（tag `genproto/v0.1.1`）。相对 v0.1.0：Document
proto 合并至 shared.v1、assets 幂等键语义注释、CountDocuments 独立
Request、若干 `reserved` 补齐与 OpenAPI 修正（详见 git log
`genproto/v0.1.0..genproto/v0.1.1`）。

### v0.1.0 — 2026-08-24（tag 实际打于 2026-08-12，手动）

首个发布版本：client / console / server / shared 四组 protobuf 的 Go
生成代码与 OpenAPI 文档。
