# 复审任务（Round 2）：12 - SDK（Go/TypeScript）与 CLI

## 背景

- Round 1 全模块审查已完成，产出 `docs/review/fix-plan.md`（F1–F11 修复批次，提交 1288705）。
- 修复已陆续合入：`git log --oneline 1288705..HEAD` 可见各 fix 提交；当前工作区可能还有未提交改动，审查以当前工作区代码为准。
- 本任务为**只读复审**：不修改任何代码，只输出复审报告。

## 角色

你是资深 Go 与 TypeScript 代码审查专家（SDK 设计），对 Torchwood 项目（Appwrite 风格的 AI/Agent-Native BaaS 平台）的「SDK 与 CLI」做只读审查。同时你是修复验证者，需对照 `fix-plan.md` 逐条核实。

## 第一步：建立基线

- 读 `docs/review/prompts/12-sdk-cli.md`：其「审查范围」「审查重点」「通用检查项」「输出要求」全部沿用于本轮。
- 读 `docs/review/fix-plan.md` 的 F8 全部与 F11-2 章节：这是本模块 Round 1 结论与修复方案。
- 可用 `git log --oneline 1288705..HEAD -- sdk/ cmd/client/` 与 `git show <commit>` 查看修复的实际改动。

## 必读上下文

- 仓库根目录：`D:\Codes\qiulin\torchwood`
- `sdk/go/`：Go SDK，拆分为 `client/`（用户端）与 `server/`（管理面）；`server` 包提供 `InvokeJSON` 以 API Key 调用 Server API；CLI 通过 sdk/go 调用，CLI 源码不直接 import genproto/grpc（有 import_guard_test 兜底）
- `sdk/typescript/`：TypeScript SDK（`src/client/`、`src/server/`）；`sdk/demo/`：web demo
- `cmd/client/`：Torchwood CLI（cobra，`bin/torchwood`），命令通过 JSON map 请求 / InvokeJSON 实现；新增 RPC 无需在 CLI 登记，覆盖完整性由 sdk/go/server 测试保证
- 已知改动（2026-08）：CLI 重构为 `cmd/client/cmd` 包；`FileTokenStore` 在 POSIX 用原子 rename、Windows 先 pre-remove（commit 539d956）；`json.Number` 保 int64 精度（commit df11068）

## 复审重点 A：修复验证（逐条核实）

对 fix-plan 中本模块的每一个修复项逐条核实：

1. **F8-1 TS SDK labels 编码错误** — `sdk/typescript/src/server/users.ts:19,51`：labels 是否改为 `Record<string, unknown>` 直接透传，删除 `{values:...}` 包装。
2. **F8-1 int64 类型与运行时不符** — `sdk/typescript/src/types.ts:95`：count/affected/size/expires_at 等 int64 字段类型是否改为 `string | number` 或统一 string，运行时精度是否得到保持。
3. **F8-1 MFA 分支崩溃** — `sdk/typescript/src/client/account.ts:25,45`：signIn/signUp 是否增加 `if (res.mfa_required) return res` 分支，类型是否补 mfa_required/challenge_token。
4. **F8-2 TS deleteSessions keepCurrent 无法传递** — `sdk/typescript/src/client/account.ts:92-96`、`proto/client/v1/account.proto:56-57`：proto 是否给 DeleteSessions 加 `body: "*"` 或改 query 绑定并重新生成；TS SDK 能否正确传递 keepCurrent。
5. **F8-3 Web demo 构建被破坏** — `sdk/demo/src/lib/` 的 context 文件名（当时沿用项目旧名）vs 10 处 import `@/lib/torchwood-context`：文件是否重命名为 `torchwood-context.tsx`；demo 是否能通过 `task sdk-demo-build`。
6. **F8-4 Go SDK 补 8 个缺失类型化方法** — `sdk/go/server/*`：UpdateUserPassword、DeleteTeam、GetTeamPrefs/UpdateTeamPrefs、UpdateCollection/DeleteCollection/DeleteAttribute/DeleteIndex 是否已补齐并带 bufconn 测试。
7. **F8-4 gRPC 客户端默认接收上限提升** — `sdk/go/internal/conn/conn.go:17-18`：默认 4MiB 是否改为 8MiB，CLI 与 SDK 是否一致生效。
8. **F8-4 CLI deployments create 上限与 help 文案** — `cmd/client/cmd/functions.go:225,432-434`：上限与 help 是否改为 8MiB/1MiB，并与服务端实际限制一致。
9. **F8-4 InvokeJSON 完整性测试去硬编码** — `sdk/go/server/invoke_test.go:49-59`：硬编码清单是否改为遍历 protoregistry，测试是否真实覆盖全部 RPC。
10. **F8-4 TS projects.create 字段修正** — `sdk/typescript/src/server/projects.ts:19-24`：是否移除不存在的 `id` 字段、补齐 description。
11. **F8-4 TS Storage 补 public/metadata** — `sdk/typescript/src/server/storage.ts:7-12`、`sdk/typescript/src/types.ts:156-172`：public 与 metadata 字段是否补齐，类型定义是否同步。
12. **F8-4 FileTokenStore 展开 `~` + MkdirAll** — `sdk/go/client/token.go:59`：home 目录 `~` 是否展开，目录不存在时是否 `MkdirAll`，权限位是否保持 0600。
13. **F8-4 TS SDK 补最小测试集** — `sdk/typescript/src/*`：labels 编码、错误解析、MFA 分支是否有真实断言，不是假断言。
14. **F11-2 TS SDK 与 proto 脱节** — `sdk/typescript/src/server/functions.ts`、account 缺失方法、CI 契约测试：16 个 functions RPC 与 account 缺失 16 个方法是否补齐；是否建立 proto RPC 集合 vs SDK 方法集合的 CI 契约测试。

每条检查：1）修复是否已落地；2）修复是否正确完整（有无绕过路径、边界遗漏）；3）修复是否引入新问题（接口/行为变化是否同步到全部调用方与前端/SDK）；4）承诺的测试是否真实存在且断言真实行为。

## 复审重点 B：回归与新问题排查

- 修复触动的文件及其上下游：行为变化是否破坏既有功能（功能完整性回归）。
- Round 1 报告中的 P2/P3 未修项：确认仍存在则原级保留，被修复波及的标注变化。
- 按 round-1「通用检查项」重扫本模块：安全（token/API Key 处理、redirect/URL 注入）、正确性（错误处理/JSON 精度/并发）、一致性（与 AGENTS.md 约定、proto 注解、domain 端口签名）、测试质量。
- **本模块修复后特有风险点**：
  1. F8-2 修改 `proto/client/v1/account.proto` 给 DeleteSessions 加 body 后，需确认 gateway 路由、genproto 生成、Console 前端调用均同步，避免 REST 行为漂移。
  2. F8-4 新增 8 个 Go SDK 类型化方法，需确认方法签名、错误映射、URL path/query 构造与 server proto 完全一致，bufconn 测试不能只跑成功路径。
  3. F8-4 将 gRPC 默认接收上限从 4MiB 提至 8MiB，需确认 CLI、Go SDK、TS SDK（fetch 无此限制）三端一致，且不会因大响应导致客户端 OOM。
  4. F11-2 新增 CI 契约测试仅比对 RPC 方法集合时，需确认是否同时校验字段名映射、请求方法（GET/POST/DELETE/PATCH）与 body/query 绑定，避免「方法同名但契约不同」的漏报。

## 输出要求

简体中文复审报告，三节结构：

1. **修复验证结论表**：每个修复项一行——✅已修复 / ⚠️部分修复 / ❌未修复 / 🔴引入回归，附证据（`文件路径:行号`）与一句话说明；
2. **新发现问题**：按 🔴P0 / 🟠P1 / 🟡P2 / 🟢P3 分级，每条给 `文件路径:行号` + 问题描述 + 影响 + 修复建议；
3. **模块总体结论**：修复完成度百分比估计、剩余风险 Top 3、是否建议关闭本模块审查。

## 约束

- 只读，不修改任何文件；不运行需要 Postgres/Redis/MinIO/Docker 的集成测试；
- 可运行 `go vet ./sdk/go/... ./cmd/client/...`、`go test ./sdk/go/... ./cmd/client/...`、`npx tsc --noEmit`（`sdk/typescript/` 目录）与 `task sdk-demo-build` 辅助验证。
