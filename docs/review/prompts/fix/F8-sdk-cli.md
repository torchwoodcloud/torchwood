# 修复任务 F8：SDK（Go / TS）与 CLI 修复

## 角色

你是资深 Go + TypeScript 工程师，负责修复 Torchwood SDK 与 CLI 的契约错误与缺失。
方案详见 `docs/review/fix-plan.md` §8（F8 批次）。**只修本任务列出的问题**。

## 工作目录与必读

- 仓库根目录：`D:\Codes\qiulin\torchwood`（Windows，pwsh）
- 必读：`AGENTS.md`、`docs/review/fix-plan.md` §8、`sdk/README.md`、
  `docs/developer/12-sdk.md`
- 审查报告（背景）：`docs/review/` 下的 12 报告

## 修复清单

1. **TS SDK labels 编码错误 + int64 类型不符 + MFA 分支崩溃**（P1）：
   - 位置：`sdk/typescript/src/server/users.ts:19,51`（labels 发送 `{values:[...]}` 包装）、
     `src/types.ts:95`（`User.labels?: string[]`）、`src/types.ts:26,141,169` 与
     `src/client/databases.ts:112-117`、`src/server/databases.ts:285-291`、
     `src/client/account.ts:155,187`（int64 声明为 number，网关实际序列化为字符串）、
     `src/client/account.ts:25,45`（signUp/signIn 未处理 mfa_required）。
   - 修复：
     a. `labels` 改为 `Record<string, unknown>` 直接透传（对照 Go SDK 的 map 语义）；
     b. int64 字段类型改 `string | number`（或统一 string）并在注释说明
        protojson 序列化行为；
     c. signIn/signUp：`if (res.mfa_required) return res;` 后再处理 tokens，
        返回类型补 `mfa_required`/`challenge_token`（对照 proto
        `SignInResponse/SignUpResponse` 与 Go SDK `sdk/go/client/account.go:25,43`）。
2. **TS deleteSessions keepCurrent 无法传递**（P1）：
   - 位置：`src/client/account.ts:92-96`；根因 `proto/client/v1/account.proto:56-57`
     （DELETE 方法无 `body: "*"`，grpc-gateway 不转发请求体）。
   - 修复：修改 proto 给 DeleteSessions 的 http 注解加 `body: "*"`（或改为 query 参数绑定），
     然后执行 `task generate-proto` 重新生成（**需要 buf 可用；若环境无法生成，
     修改 proto 后标注“待 CI/本地 generate-proto”**）；SDK 侧同步确认请求体发送正确。
3. **Web demo 构建被破坏**（P1）：
   - 位置：`sdk/demo/src/lib/` 的 context 文件（当时文件名仍沿用项目旧名）vs 全部 import
     `@/lib/torchwood-context`（App.tsx、LoginPage.tsx、DatabasesPage.tsx 等约 10 处）。
   - 修复：将文件重命名为 `torchwood-context.tsx`；在 `sdk/demo` 目录运行
     `npm install`（若 node_modules 已存在则跳过）与 `npm run build` 验证。
4. **P2 补强**：
   - Go SDK 补 8 个缺失类型化方法：`sdk/go/server/users.go` 补 `UpdateUserPassword`；
     `sdk/go/server/teams.go` 补 `DeleteTeam`/`GetTeamPrefs`/`UpdateTeamPrefs`；
     `sdk/go/server/databases.go` 补 `UpdateCollection`/`DeleteCollection`/`DeleteAttribute`/
     `DeleteIndex`（签名风格对齐现有封装），并补 bufconn 测试
     （参考 `sdk/go/server` 现有测试模式）。
   - `sdk/go/internal/conn/conn.go:17-18`：grpc.NewClient 增加
     `grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20))`（与服务端对齐）。
   - `cmd/client/cmd/functions.go:225,432-434`：`deployments create` 的大小上限与 help
     文案改为服务端实际值（gRPC 8MiB / 建议 ≤1MiB 走 multipart）。
   - `sdk/go/server/invoke_test.go:49-59`：TestInvokeJSONCompleteness 的硬编码 proto
     文件清单改为遍历 `protoregistry.GlobalFiles` 中 `torchwood.server.v1.` 前缀全部文件。
   - `sdk/typescript/src/server/projects.ts:19-24`：移除 proto 不存在的 `id` 字段，
     签名改为 `{ name: string; description?: string }`。
   - `sdk/typescript/src/server/storage.ts:7-12` 与 `src/types.ts:156-172`：补
     `public`（Bucket）/`metadata`（FileItem）字段。
   - `sdk/go/client/token.go:59`：FileTokenStore Save/Load 展开 `~` 且父目录不存在时
     `MkdirAll(0o700)`。
   - TS SDK 补最小测试集（vitest + stub fetch）：错误解析、labels 编码、MFA 分支、query 展开。

## 约束

- proto 修改仅限 DeleteSessions 的 http 注解（若生成工具不可用，则只改 proto 并说明）
- 保持现有代码风格；不引入新第三方依赖（测试框架 vitest 如 package.json 已有则用）
- 不运行需要真实 Torchwood 服务器的测试

## 验证

- Go：`go test ./sdk/go/... ./cmd/client/...`、`go vet ./sdk/go/... ./cmd/client/...`
- TS：在 `sdk/typescript` 目录 `npx tsc --noEmit`（如有测试脚本则运行）
- Demo：在 `sdk/demo` 目录 `npm run build`
- 若改了 proto：`task generate-proto` 后 `go build ./...`

## 输出

最终汇报：按清单逐项给出「改动文件:位置 + 改动摘要 + 验证结果」；注明 proto 改动是否
已重新生成、demo 构建是否通过。
