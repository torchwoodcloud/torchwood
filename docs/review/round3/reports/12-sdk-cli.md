# 全量审查报告（Round 3）：12 - SDK（Go/TypeScript）与 CLI

> 审查范围：`sdk/go/`、`sdk/typescript/src/`、`cmd/client/`、`sdk/demo/src/`（不含 node_modules）。  
> 基线：`docs/review/prompts/12-sdk-cli.md`、`sdk/README.md`、`docs/developer/12-sdk.md`、`docs/review/round2/reports/12-sdk-cli.md`。  
> 交叉对照：`proto/client/v1/account.proto`、`proto/server/v1/databases.proto`、`proto/server/v1/functions.proto`、`internal/app/client/account.go`、`pkg/grpc/interceptor/jwt.go`。  
> 性质：只读审查，未改源代码。

---

## 1. 摘要

Round 2 的 F8 / F11-2 主体仍在：TS 契约测试已扩展到 HTTP method/path/body/query 绑定，CI 已跑 `sdk/typescript` 测试与 `task sdk-demo-build`，FileTokenStore `~` 展开与 `AuthResult.tokens?` 已补齐。

本轮对照 proto 后，**特别关注项结论**：

| 关注项 | 结论 |
|--------|------|
| ConfirmEmailChange 三端暴露与 public 语义 | Go/TS SDK **已暴露**；CLI **按设计不暴露**（仅 Server API）。**语义未跟到 ACCESS_PUBLIC**：两端注释仍写「需登录态」；Go 拦截器未把该方法列入 `noRefreshMethods`，过期 refresh 会挡住公开确认。 |
| DeleteFactor 带 `code` | Go / TS / proto **一致**（verified 需 code，pending 可空；REST 走 query）。CLI 无 Client MFA，不适用。 |
| B3 自定义动词路径 | TS Client/Server 已切到 `documents:count` / `documents:bulkUpdate` / `documents:bulkDelete` / `functions:runtimes` / `functions:specifications`。Go SDK 与 CLI 走 gRPC 方法名，不受 REST 路径影响。 |
| CLI 不 import genproto/grpc | `import_guard_test.go` 覆盖 `cmd/client` 与 `cmd/client/cmd` 非测试源文件，前缀匹配有效。 |
| API Key 不进日志 / 错误不吞 | CLI/SDK 无 `log`/`fmt.Print` 打 key；`formatRPCError` 只打印 gRPC status。Go client `store.Load()` 错误被 `_` 丢掉。 |
| SDK 方法覆盖 vs proto | Server 面：`InvokeJSON` 完整性测试是权威覆盖源。Client 面：Go 缺 `DeleteTeam`；TS `FunctionsService` 实现了 16 个 RPC，但 **未挂到 `Torchwood.server`，包入口也不导出**。 |

未发现凭据回显或 int64 精度回归（P0=0）。有 3 项 P1。

---

## 2. 已核实健康

### 2.1 Round 2 遗留项复验

| 编号 | 项 | 结论 | 证据 |
|------|----|------|------|
| R2-P1 | CI 未跑 TS SDK / demo | ✅ 已修 | `.github/workflows/ci.yml:94-99` 有 `TS SDK test` 与 `SDK demo build`；`Taskfile.yml:141-150` `test` 依赖 `test-sdk-ts`。 |
| R2-P2 | 契约测试只比方法名 | ✅ 已加强 | `sdk/typescript/src/__tests__/contract.test.ts:341-534` 对 40+ 写方法比对 HTTP method/path/query/body。读路径（`CountDocuments`、`listRuntimes`）仍未列入绑定用例。 |
| R2-P2 | DeleteSessions REST/gRPC 参数位置 | ⚠️ 仍在 | proto 仍无 `body: "*"`；TS 继续走 `keep_current` query。功能可用，接口形态未统一。 |
| R2-P2 | FileTokenStore `~` 无测试 | ✅ 已修 | `sdk/go/client/token_test.go:84-106` `TestFileTokenStoreExpandsTildeHome`。 |
| R2-P2 | `AuthResult.tokens` 非可选 | ✅ 已修 | `sdk/typescript/src/types.ts:32-38` 已为 `tokens?: TokenBundle`。 |
| R2-P3 | `expandHome` 不支持 `~user/` | ✅ 已注明 | `sdk/go/client/token.go:60-66` 注释写明仅 `~` / `~/`。 |
| R2-P3 | deployments create help 1MiB/8MiB 混淆 | ✅ 已修 | `cmd/client/cmd/functions.go:225` 区分通道上限 8MiB、建议 ≤1MiB、multipart 50MiB。 |

### 2.2 特别关注项细证

**ConfirmEmailChange**

- proto：`proto/client/v1/account.proto:133-145`，`ACCESS_PUBLIC`，「点链接无需登录态」。
- 用例：`internal/app/client/account.go:555-617` 只校验 `project_id`/`user_id`/`secret`，不读 principal。
- 服务端拦截器：`pkg/grpc/interceptor/jwt.go:77-86` 对 public 方法忽略无效凭证。
- Go SDK：`sdk/go/client/account.go:94-102` 已暴露；测试 `account_test.go:163-176` 透传字段。
- TS SDK：`sdk/typescript/src/client/account.ts:97-108` 已暴露；契约映射与 HTTP 绑定用例均已登记。
- CLI：`cmd/client` 只调 `torchwood.server.v1`（`invoke.go:61-63` 拒绝 client 包）。符合「CLI = Server API + API Key」约定，**不是漏暴露**。

**DeleteFactor + code**

- proto：`DeleteFactorRequest.code = 2`（`account.proto:717-722`），注释写明 REST `?code=`。
- Go：`DeleteFactor(ctx, factorID, code string)`（`account.go:319-326`）；测试覆盖 verified 缺 code → `InvalidArgument`（`account_test.go:371-394`）。
- TS：`deleteFactor(factorId, code?)` 经 query 传递（`account.ts:399-405`）；`account.test.ts:75-101` 断言有/无 code。
- 三端（proto / Go / TS）一致。CLI 无 Client MFA。

**B3 自定义动词**

- TS Client：`sdk/typescript/src/client/databases.ts:115` `.../documents:count`。
- TS Server：`sdk/typescript/src/server/databases.ts:288,305,320` `:count` / `:bulkUpdate` / `:bulkDelete`；`functions.ts:19,28` `:runtimes` / `:specifications`。
- 契约测试已捕获 BulkUpdate/BulkDelete 的 path；demo `DatabasesPage.tsx` 走 SDK `countDocuments()`。
- Go/CLI 为 gRPC，路径变更不影响。

**覆盖机制**

- Server：`sdk/go/server/invoke_test.go:48-74` 遍历 `torchwood.server.v1`（排除 `APIKeysService`），`count > 60`。新增 Server RPC 无需改 CLI。
- TS：`contract.test.ts` 要求 swagger operationId ⊆ `RPC_TO_METHOD` ⊆ 类 prototype。
- Go Client **没有**对等的 proto 全集扫描；缺方法只能靠人工。

**安全 / 精度 / 平台**

- API Key：`server.Client.authContext` 只进 gRPC metadata；CLI flag/env 取值后不打印；错误文案不含 secret。
- `json.Number`：`cmd/client/cmd/helpers.go:11-16` + `databases_test.go:428-436` 回归 `1234567890123456789`。
- FileTokenStore：POSIX rename；Windows 先 `Remove` 再 `Rename`；`0o600`/`MkdirAll 0o700`；进程内 mutex。
- `import_guard_test.go:13-44` 扫描 `.` 与 `cmd`，禁止 `genproto` / `grpc` / `protobuf` 前缀。

---

## 3. 问题清单

### 🔴 P0 严重

无。未发现 API Key/token 回显、日志落明文、或 int64 精度丢失导致写坏数据。

### 🟠 P1 高

1. **ConfirmEmailChange 已是 ACCESS_PUBLIC，Go SDK 拦截器仍按登录态走刷新，过期 refresh 会阻断公开确认**
   - 位置：`sdk/go/client/auth.go:14-20`、`sdk/go/client/account.go:94-96`、`sdk/typescript/src/client/account.ts:96-107`。
   - 问题：
     - proto / use-case 明确「邮件链接点开即完成、无需登录」。
     - Go 注释仍写「需登录态，user_id 必须等于当前用户」。
     - `noRefreshMethods` 注释自称「公开方法 + SignOut」，实际只列 SignIn/SignUp/RefreshToken/SignOut。`ConfirmEmailChange`（以及 UpdateVerification / UpdateRecovery / MagicURL / OTP / MFA challenge 等公开 RPC）不在表内。
     - 拦截器对未列入的方法先 `refreshIfExpiring`：本地有过期 token 且 refresh 失败（`Unauthenticated`）时 **RPC 根本不会发出**。服务端 public 分支本会忽略坏凭证。
     - TS 默认 `auth: "user"`，未像 `updateVerification` 那样设 `auth: "none"`。服务端会忽略无效 Bearer，功能通常可用，但注释同样错误。
   - 影响：持有过期 FileTokenStore 的 Go 客户端点邮件确认链会失败；集成方按注释先要求登录，与产品语义相反。
   - 建议：把全部 ACCESS_PUBLIC 方法（至少 ConfirmEmailChange / UpdateVerification / UpdateRecovery / Create*OTP* / CreateMFASession / MagicURL / Anonymous / OAuth 换票）加入 `noRefreshMethods`；Go/TS 注释改为「公开：凭 user_id+secret，无需登录」；TS `confirmEmailChange` 设 `auth: "none"`。

2. **TS SDK `FunctionsService` 未挂到门面，发布入口也导不出，16 个 Functions RPC 对常规调用方不可达**
   - 位置：`sdk/typescript/src/torchwood.ts:24-51`、`sdk/typescript/src/index.ts:1-3`、`sdk/typescript/package.json:8-12`。
   - 问题：`src/server/functions.ts` 实现了 ListRuntimes…GetExecution；`contract.test.ts` 直接 `import { FunctionsService } from "../server/index.js"` 所以测试通过。`Torchwood.server` 只有 health/projects/users/teams/databases/apiKeys/oauthProviders/storage。`index.ts` 只导出 `Torchwood` / `TorchwoodError` / types；`exports` 仅 `"."`。
   - 影响：按 `docs/developer/12-sdk.md` §3.3 用 `Torchwood.withApiKey()` 的 Agent **调不到 Functions**。契约测试漏报「类在、门面无」。
   - 建议：`torchwood.ts` 增加 `server.functions`；根入口再导出 `FunctionsService`（或至少保证门面可达）；契约测试断言 `Torchwood` 实例属性覆盖 Server swagger 服务。

3. **Go Client SDK 未封装 `TeamsService.DeleteTeam`**
   - 位置：`sdk/go/client/teams.go:14-59`（7 个方法）；对照 `proto/client/v1/teams.proto:72-74`。
   - 问题：TS Client 有 `deleteTeam`（`sdk/typescript/src/client/teams.ts:22-24`）；Go Server 有 `DeleteTeam`（`sdk/go/server/teams.go:27`）。Go Client 无该方法，fake 测试也未实现。Client 包没有 InvokeJSON 逃生舱。
   - 影响：Go 终端用户 SDK 无法删自己的团队，与 proto / TS 不一致。
   - 建议：补 `DeleteTeam` + bufconn 测试；为 `sdk/go/client` 增加与 `invoke_test.go` 同类的 proto 方法全集扫描。

### 🟡 P2 中

4. **Go Server 类型化 `UpdateUser` 丢掉 proto 的 `email_verified`**
   - 位置：`sdk/go/server/users.go:48-67`；对照 `proto/server/v1/users.proto:113-123`。
   - 问题：签名只有 `name/email/status/labels/prefs`。TS `users.update` 与 CLI `--email-verified` 都支持。
   - 影响：类型化 Go API 无法改邮箱验证状态；须走 `InvokeJSON`。
   - 建议：增加 `emailVerified *bool` 并透传到 `UpdateUserRequest.EmailVerified`。

5. **Go client `store.Load()` 错误被丢弃**
   - 位置：`sdk/go/client/auth.go:26,32`（`tok, _ := c.store.Load()`）。
   - 问题：FileTokenStore 损坏时 `Load` 返回 parse error，拦截器当「无 token」继续调，表现为 `Unauthenticated`，调用方看不到文件损坏。
   - 影响：排障困难；与「错误不吞」不符。
   - 建议：`Load` 非 `(nil, nil)` 的 error 直接返回给调用方。

6. **FileTokenStore 在 Windows 上非原子替换**
   - 位置：`sdk/go/client/token.go:118-127`。
   - 问题：先 `Remove` 再 `Rename`。两步之间 `Load` 会当成无 token；跨进程无锁。POSIX rename 才是原子的。
   - 影响：Windows 并发刷新/多进程共享 token 文件可能丢会话。
   - 建议：文档写明单进程假设；或评估替代方案（限制多进程共享同一文件）。

7. **DeleteSessions REST 仍走 query，与 gRPC 消息字段不一致（Round 2 遗留）**
   - 位置：`proto/client/v1/account.proto` DeleteSessions HTTP 注解；`sdk/typescript/src/client/account.ts:119-123`。
   - 问题：功能可用，REST/gRPC 参数位置不同。
   - 影响：其他 REST 调用方需单独记差异。
   - 建议：proto 加 `body: "*"` 并重生，或在 proto 注释固定「REST 用 query」。

8. **契约测试未覆盖自定义动词读路径**
   - 位置：`sdk/typescript/src/__tests__/contract.test.ts:407-463`。
   - 问题：BulkUpdate/BulkDelete 已比对 path；`CountDocuments`、`ListRuntimes`、`ListSpecifications` 未进 HTTP 绑定用例。
   - 影响：`:count` / `:runtimes` 写回旧路径时测试仍绿。
   - 建议：把这三个 GET 自定义动词加入 `cases`。

### 🟢 P3 低

9. **文档与实现不同步：Account 表漏 `confirmEmailChange`；Functions 未进门面说明**
   - 位置：`docs/developer/12-sdk.md:164`（§4.1 无 confirmEmailChange）、`:364`（映射表无 ConfirmEmailChange）、`:110-121`（§3.3 无 `tw.server.functions`）；`sdk/README.md:120-124` Server 列表无 Functions。
   - 影响：Agent 以文档为 schema 时漏方法。
   - 建议：文档与 `RPC_TO_METHOD` / 门面属性对齐。

10. **TS `increment` 类型为 `Record<string, number>`**
    - 位置：`sdk/typescript/src/types.ts:168`。
    - 问题：proto 为 `map<string, int64>`；JS number 对 >2^53 丢精度。CLI 已用 `json.Number`。
    - 建议：类型改为 `Record<string, string | number>`，并注明大整数用字符串。

11. **Go Client `ListTeams` 不传分页/queries**
    - 位置：`sdk/go/client/teams.go:24-26`。
    - 问题：固定空 `ListRequest{}`，团队多时截断。
    - 建议：签名增加 `queries/pageSize/pageToken`。

12. **TS `signOut` 仅在请求成功后清 token**
    - 位置：`sdk/typescript/src/client/account.ts:60-65`。
    - 对照：Go `SignOut` 在成功或 `Unauthenticated` 时都清（`account.go:74-78`）。
    - 建议：401 时同样 `setAccessToken(undefined)`。

13. **Server `authContext` 用 `NewOutgoingContext` 覆盖已有 outgoing metadata**
    - 位置：`sdk/go/server/client.go:107-118`。
    - 问题：调用方事先挂的 metadata 会被丢掉。Client 包用的是 `AppendToOutgoingContext`。
    - 建议：改为 append。

14. **demo 把 API Key 与 access token 放 localStorage**
    - 位置：`sdk/demo/src/lib/storage.ts:19-60`。
    - 问题：文档已说明 demo 如此；XSS 可读 secret。可接受，但与 Console HttpOnly cookie 不一致。
    - 建议：设置页提示「仅本地演示，勿填生产 Key」。

---

## 4. 模块结论

**Agent 集成友好度：中上。**  
Server 面（Go `InvokeJSON` + CLI `rpc` + TS 契约测试）对新增 RPC 是机器可发现的；API Key 走 metadata、错误类型化、int64 精度、B3 路径、DeleteFactor code 都已对齐。缺口集中在 Client 公开流语义、TS Functions 门面、Go Client 方法全集。

**最需优先修复的 3 项**

1. 修正 ConfirmEmailChange（及同类公开 RPC）的 Go `noRefreshMethods` + 两端注释/`auth: "none"`，与 ACCESS_PUBLIC 一致。  
2. 把 `FunctionsService` 挂到 `Torchwood.server.functions` 并纳入契约对门面的断言。  
3. 补 Go Client `DeleteTeam`，并给 `sdk/go/client` 加 proto 方法覆盖扫描。

**是否建议关闭本模块审查：不建议。**  
Round 2 CI/契约粒度/token 类型等已收敛；本轮仍有 3 个 P1。补完上述三项、并把 CountDocuments/listRuntimes 纳入 HTTP 绑定用例后，可视为本模块审查收敛。其余 P2/P3 可作技术债。

**verdict：CONDITIONAL（3×P1 未关，P0=0）**
