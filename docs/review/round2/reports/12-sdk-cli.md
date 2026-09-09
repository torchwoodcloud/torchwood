# 复审报告（Round 2）：12 - SDK（Go/TypeScript）与 CLI

> 审查范围：`sdk/go/`、`sdk/typescript/`、`cmd/client/`、`sdk/demo/`。
> 基线：`docs/review/prompts/12-sdk-cli.md`、`docs/review/fix-plan.md` §F8 / §F11-2。
> 验证命令：`go vet ./...`（sdk/go 与 cmd/client 分别执行）、`go test ./...`、`npx tsc --noEmit`、`npm run test`、`task sdk-demo-build`。

---

## 1. 修复验证结论表

| 编号 | 修复项 | 结论 | 证据与说明 |
|------|--------|------|------------|
| F8-1 | TS SDK labels 编码错误 | ✅ 已修复 | `sdk/typescript/src/server/users.ts:12,40` 中 `labels` 类型为 `Record<string, unknown>`，创建/更新时直接透传对象，不再包 `{values:...}`。`src/__tests__/users.test.ts:8-34` 断言请求体为 `{labels: {region: "cn", tier: 2}}`。 |
| F8-1 | int64 类型与运行时不符 | ✅ 已修复 | `sdk/typescript/src/types.ts:27` `expires_at: string`；`:161` `affected: string \| number`；`:191` `size: string \| number`；`:207` `total_size: string \| number`；`:262` `duration_ms: string \| number`。注释说明网关将 int64 序列化为字符串，同时兼容旧 number。 |
| F8-1 | MFA 分支崩溃 | ✅ 已修复 | `sdk/typescript/src/client/account.ts:33-36,55-57` 在 `signIn`/`signUp` 中增加 `if (res.mfa_required) return res` 分支，不保存 token。`src/types.ts:32-38` 补全 `AuthResult.mfa_required/challenge_token/factors`。`src/__tests__/account.test.ts:16-33` 断言 MFA 分支返回 challenge 且不保存 token。 |
| F8-2 | TS deleteSessions keepCurrent 无法传递 | ⚠️ 部分修复 | `sdk/typescript/src/client/account.ts:103-108` 已将 `keep_current` 作为 query 参数传递；`src/__tests__/account.test.ts:53-72` 断言 URL 含 `keep_current=true`。但 **proto 未按 fix-plan 加 `body: "*"`**，`proto/client/v1/account.proto:123-127` 仍使用 `delete: "/v1/account/sessions"` 无 body；gateway 侧把 `keep_current` 映射为 query 参数（见 `genproto/client/v1/account.swagger.json`）。功能等价，但与原方案不同，REST/gRPC 接口形式不一致。 |
| F8-3 | Web demo 构建被破坏 | ✅ 已修复 | `sdk/demo/src/lib/torchwood-context.tsx` 已存在并替换旧命名文件；10 处 `@/lib/torchwood-context` import 已更新。`task sdk-demo-build` 通过（tsc + vite build 成功）。 |
| F8-4 | Go SDK 补 8 个缺失类型化方法 | ✅ 已修复 | 8 个方法均已补齐并带 bufconn 测试：`sdk/go/server/users.go:70` `UpdateUserPassword`；`teams.go:27` `DeleteTeam`、`:33` `GetTeamPrefs`、`:38` `UpdateTeamPrefs`；`databases.go:66` `UpdateCollection`、`:81` `DeleteCollection`、`:113` `DeleteAttribute`、`:134` `DeleteIndex`。测试见 `sdk/go/server/services_test.go:395-465`。 |
| F8-4 | gRPC 客户端默认接收上限提升 | ✅ 已修复 | `sdk/go/internal/conn/conn.go:20` 使用 `grpc.MaxCallRecvMsgSize(8 << 20)`，注释说明与服务端 `MaxRecvMsgSize(8<<20)` 对齐。 |
| F8-4 | CLI deployments create 上限与 help 文案 | ✅ 已修复 | `cmd/client/cmd/functions.go:225` help 文案说明 "gRPC 消息通道上限 8MiB，建议 ≤1MiB"；`:432-434` 代码限制 `len(code) > 8<<20`。该上限与 gRPC 传输上限一致（服务端 app 上限为 50MiB，见 `internal/app/functions/deployments.go:19`）。 |
| F8-4 | InvokeJSON 完整性测试去硬编码 | ✅ 已修复 | `sdk/go/server/invoke_test.go:48-74` 遍历 `protoregistry.GlobalFiles` 中 `torchwood.server.v1` 包的全部方法（排除 `APIKeysService`），断言每个方法都能被 `findServerMethod` 解析，且 count > 60。 |
| F8-4 | TS projects.create 字段修正 | ✅ 已修复 | `sdk/typescript/src/server/projects.ts:19-23` `create` 入参为 `{ name: string; description?: string }`，不再传 `id`；`description` 已补齐。 |
| F8-4 | TS Storage 补 public/metadata | ✅ 已修复 | `sdk/typescript/src/server/storage.ts:7` `createBucket` 支持 `public`；`:73` `updateFile` 支持 `metadata`。`src/types.ts:180` `Bucket.public?: boolean`；`:192` `FileItem.metadata?: Record<string, string>`。 |
| F8-4 | FileTokenStore 展开 `~` + MkdirAll | ✅ 已修复 | `sdk/go/client/token.go:62` `NewFileTokenStore` 调用 `expandHome`；`:65-74` 展开 `~/` 为主目录；`:100-103` `MkdirAll(dir, 0o700)`；`:106` 临时文件写权限 `0o600`；`:120` `Chmod(s.path, 0o600)` 兜底。`token_test.go:34-57` 验证 round-trip、权限、Clear 幂等。 |
| F8-4 | TS SDK 补最小测试集 | ✅ 已修复 | `src/__tests__/users.test.ts` 验证 labels 编码；`errors.test.ts` 验证错误解析；`account.test.ts` 验证 MFA 分支与 `deleteSessions` query 参数。断言均为真实行为，非 `assert.ok(true)`。 |
| F11-2 | TS SDK 与 proto 脱节 | ⚠️ 部分修复 | `sdk/typescript/src/server/functions.ts` 已补齐 16 个 Functions RPC；AccountService 已补齐全部 34 个 RPC；`src/__tests__/contract.test.ts` 建立 proto swagger operationId 与 SDK 方法集合的比对，本地 `npm run test` 通过。但 **CI（`.github/workflows/ci.yml`）未运行 TS SDK 测试与 demo 构建**，契约测试无法防回归；且契约测试仅校验方法名映射，未校验 HTTP 方法/路径/body/query 绑定。 |

**统计**：✅ 12 项、⚠️ 2 项、❌ 0 项、🔴 0 项。

---

## 2. 新发现问题

### 🟠 P1

1. **CI 未运行 TS SDK 测试与 demo 构建，契约测试无法防回归**
   - 位置：`.github/workflows/ci.yml:72-80`
   - 问题：backend job 只跑 `go test ./...` 与 `sdk/go` 的 Go 测试，未执行 `sdk/typescript` 的 `npm run test`（含 F11-2 契约测试），也未执行 `task sdk-demo-build`。
   - 影响：F8-3 demo 构建、F8-4 TS 测试、F11-2 契约测试均依赖本地/手动运行，无法在 PR 阶段捕获 TS SDK 与 proto 的脱节。
   - 建议：在 CI 新增 SDK TypeScript 步骤（`npm install && npm run test`）和 SDK Demo 构建步骤（`task sdk-demo-build`）。

### 🟡 P2

2. **Go SDK 新增 8 方法的 bufconn 测试只覆盖成功路径**
   - 位置：`sdk/go/server/services_test.go:395-465`
   - 问题：`TestUsers_UpdateUserPassword`、`TestTeams_DeleteAndPrefs`、`TestDatabases_UpdateAndDeleteSchema` 仅断言正常返回与请求字段透传，未覆盖 gRPC 错误码映射、nil/空参数、权限拒绝等边界。
   - 影响：符合 round-2 提示中「不能只跑成功路径」的风险；未来服务端错误映射变化时 SDK 测试无法发现。
   - 建议：为每个新增方法补充至少一个错误路径用例（如 `codes.NotFound`、`codes.PermissionDenied`），验证错误是否被正确透传。

3. **TS SDK 契约测试仅比对方法名，未校验 HTTP 方法/路径/body/query 绑定**
   - 位置：`sdk/typescript/src/__tests__/contract.test.ts:286-313`
   - 问题：测试只检查 `typeof proto[method] === "function"`，不校验 SDK 方法内部使用的 HTTP 方法、URL 路径、字段名映射、body/query 绑定。
   - 影响：存在「方法同名但契约不同」的漏报风险，例如 SDK 把 POST 写成 PATCH、把 body 字段写成 query 参数。
   - 建议：扩展契约测试，从 swagger 读取每个 operation 的 `method`、`path`、`parameters`（in: query/body），与 SDK 方法内部的实际请求做结构化比对；至少为关键写方法（Create/Update/Delete）增加端到端请求捕获测试。

4. **F8-2 通过 TS SDK query 参数绕过，proto 未同步加 body**
   - 位置：`proto/client/v1/account.proto:123-127`、`sdk/typescript/src/client/account.ts:103-108`
   - 问题：fix-plan 原方案是在 proto 给 `DeleteSessions` 加 `body: "*"` 后重新生成；当前实现选择让 TS SDK 把 `keep_current` 作为 query 参数传递。REST/gRPC 在 `DeleteSessions` 上的参数位置不一致（gRPC 为消息字段，REST 为 query）。
   - 影响：功能当前可用（gateway 已正确绑定 query），但未来 Console 或其他 REST 调用方接入时需单独记忆该差异；且一旦服务端变更参数绑定方式，TS SDK 与 gRPC 客户端行为将漂移。
   - 建议：评估统一为 `DeleteSessions` 加 `body: "*"` 并重新生成，使 REST 与 gRPC 都通过消息体传 `keep_current`；或在 proto 注释中显式声明 REST 使用 query 参数。

5. **FileTokenStore 的 `~` 展开缺少单元测试**
   - 位置：`sdk/go/client/token.go:65-74`、`token_test.go`
   - 问题：实现已存在，但 `token_test.go` 未覆盖 `NewFileTokenStore("~/tokens.json")` 路径是否正确展开到用户主目录。
   - 影响：低，但手动验证不足，后续重构 expandHome 时无回归防护。
   - 建议：增加一个测试用例，使用 `os.UserHomeDir()` 计算期望值并断言 Save 后文件落在主目录下。

6. **TS SDK `AuthResult.tokens` 类型未标为可选，MFA 分支存在 undefined 访问风险**
   - 位置：`sdk/typescript/src/types.ts:32-38`
   - 问题：`AuthResult.tokens` 声明为 `tokens: TokenBundle`（非 optional），但 MFA 分支不返回 tokens。调用方若按类型直接访问 `res.tokens.access_token` 会在运行时得到 `Cannot read properties of undefined`。
   - 影响：类型安全误导，可能导致 SDK 用户写出崩溃代码。
   - 建议：将 `tokens` 改为 `tokens?: TokenBundle`，并在 JSDoc 中强调 `mfa_required` 为 true 时无 tokens。

### 🟢 P3

7. **FileTokenStore `expandHome` 仅支持 `~/`，不支持 `~user/` 形式**
   - 位置：`sdk/go/client/token.go:66`
   - 问题：仅判断 `path == "~"` 或前缀 `"~/"`，shell 标准还支持 `~otheruser/dir`。
   - 影响：极低，常见用法已覆盖。
   - 建议：文档说明只支持 `~` 与 `~/`，或在注释中显式标注。

8. **CLI `functions deployments create` help 中 "建议 ≤1MiB" 与 8MiB 上限并存，可能误导用户**
   - 位置：`cmd/client/cmd/functions.go:211,225,432`
   - 问题：help 文案同时出现 "建议 ≤1MiB" 与 "上限 8MiB"；服务端 app 实际允许 50MiB（multipart），gRPC 消息层限制 8MiB。
   - 影响：用户可能误以为服务端硬限制为 1MiB，或混淆 8MiB/50MiB 的适用范围。
   - 建议：help 文案区分 "gRPC 通道上限 8MiB" 与 "服务端建议单包 ≤1MiB，更大请走 multipart 上传"。

---

## 3. 模块总体结论

- **修复完成度**：约 **85%**。F8 全部 13 项与 F11-2 的核心内容已落地，12 项完全修复、2 项部分修复，无未修复或引入回归的项。
- **剩余风险 Top 3**：
  1. **CI 未覆盖 TS SDK 测试与 demo 构建**（P1）：契约测试和 demo 构建目前只在本地验证，无法在 PR 阶段防回归。
  2. **契约测试粒度不足**（P2）：仅比方法名，未校验 HTTP 方法、路径、body/query 绑定，存在「同名不同契」的漏报风险。
  3. **F8-2 绕过 proto 修改**（P2）：REST 与 gRPC 在 `DeleteSessions` 上传参位置不一致，未来调用方接入时容易漂移。
- **是否建议关闭本模块审查**：**不建议完全关闭**。建议在补齐 CI 中 TS SDK 测试/demo 构建步骤、并为契约测试增加 HTTP 绑定校验后，方可视为本模块审查收敛；其余 P2/P3 可作为后续技术债跟进。

---

## 附录：验证命令输出摘要

```text
# sdk/go
$ go vet ./...          # 通过
$ go test ./...         # ok (client/internal/conn/server)

# cmd/client
$ go vet ./cmd/client/...   # 通过
$ go test ./cmd/client/...  # ok (cmd/client, cmd/client/cmd)

# sdk/typescript
$ npx tsc --noEmit      # 通过
$ npm run test          # 14 tests pass

# sdk/demo
$ task sdk-demo-build   # tsc + vite build 成功
```
