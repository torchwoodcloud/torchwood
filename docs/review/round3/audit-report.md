# Torchwood Round 3 全量审核总报告

> 审核日期：2026-08-13  
> 对象：当前 `main`（`031ce90`），工作区干净  
> 方法：12 个模块子代理并行只读审查 → 主代理对全部 P0/P1 交叉读源码核实  
> 分报告：`docs/review/round3/reports/01`–`12`

---

## 总体结论

**有条件通过（Conditional Pass）。无 P0。不建议把本轮当「可关闭审查」——有 4 项已核实的阻断级 P1 应先修。**

经过 Round 1 / Round 2 两轮修复（含 G12 Functions API Key、B1 邮箱 staging、B2 worker 重试持久化、B3 REST 自定义动词），认证、文档权限、租户隔离、JWT/OTP/TOTP、分片上传与 Docker 执行器的**安全主路径是健康的**。本轮没有发现可未认证利用的注入、跨租户越权或凭据泄露。

本轮新问题的共性不再是「底座未修好」，而是**门禁表 / 守卫 / SDK 门面在产品决策变更后没有同步收口**：

1. Console **viewer 角色 denylist 漏登记**，只读管理员可删 API Key、改用户邮箱并接管账号。
2. G12 给 Functions 写方法加了 `RequireServerWriteActor`，HTTP multipart 部署却**没把 Principal 注入 context**，官方大包上传恒 401。
3. 路线图承诺 Agent 可运行时建库/集合，用例层 Databases DDL 仍 `RequirePlatformAdmin`，**持 `databases.write` 的 API Key 被误伤**。
4. TypeScript SDK 实现了 Functions 16 个 RPC，但**没有挂到 `Torchwood.server`**，常规 Agent 调不到。

---

## 模块健康度

| 模块 | P0 | P1（子代理） | 主代理复核 | 建议 |
|------|----|-------------|------------|------|
| 01 安全与认证 | 0 | 1 | **确认**：viewer 越权属实 | 立刻修 |
| 02 动态文档层 | 0 | 0 | 同意，无回归 | 可关闭安全主线 |
| 03 Server API | 0 | 1 | **确认**：Functions HTTP 401 | 立刻修 |
| 04 Client/Console API | 0 | 1（分页） | 降为 P2：列表截断，非安全 | 可关闭安全主线 |
| 05 Account 用例 | 0 | 0 | 同意；B1 验收通过 | 可关闭 |
| 06 Server/Console 用例 | 0 | 2 | **确认** DDL 过严；邀请幂等保留 P1 | 拍板 + 修 |
| 07 Storage/Functions/Worker | 0 | 1 | 与 03 同一缺陷 | 立刻修 |
| 08 CRUD / 领域 | 0 | 0 | 同意 | 常规迭代 |
| 09 基础设施 | 0 | 0 | 同意 | 常规迭代 |
| 10 Proto / OpenAPI | 0 | 0 | 同意 | 可关闭 |
| 11 Console | 0 | 2 | 预览裂图保留 P1；批量删无确认降 P2 | 修预览 |
| 12 SDK / CLI | 0 | 3 | Functions 门面 + Confirm 刷新保留 P1；DeleteTeam 降 P2 | 修门面 |

子代理共报约 12 条 P1。主代理按「是否可复现、是否构成正确性/安全/产品契约断裂」收口为下面 6 条确认项。

---

## 已核实必须处理的问题

### P1-1（安全）viewer 可调用未入表的写 RPC，DeleteAPIKey / UpdateUser 无用例守卫

- **位置**
  - `pkg/grpc/interceptor/admin_roles.go:14-49`（有 `CreateAPIKey`，无 `DeleteAPIKey` / `UpdateUser` / 多数 Storage·Teams 写方法）
  - `pkg/grpc/interceptor/jwt.go:129-134`（仅当表项非空才拦角色）
  - `internal/app/server/apikeys.go:124-133`（`Delete` 无 `Require*`）
  - `internal/app/server/users.go:138-198`（`UpdateUser` 无 actor 守卫；可改 `email` 并置 `email_verified=false`）
- **核实**：表是不完整 denylist。viewer 的 console 会话是 `ActorKind=admin`，可通过 `ACCESS_API_KEY` 门。未登记方法角色检查被跳过。
- **影响**
  - viewer 删除本项目任意 API Key（Create 被禁、Delete 却开放）。
  - viewer 把用户邮箱改到自己控制的地址，再走公开 `CreateRecovery` **接管终端用户账号**。
- **修复**
  1. 所有非 List/Get/Count 的 Server 写方法补进 `adminRoleMethodRules`。
  2. `APIKeys.Delete` 加 `RequirePlatformAdmin`；`UpdateUser` 至少 `RequireServerWriteActor`。
  3. 启动期断言：ACCESS_API_KEY 写方法 ⊆ 角色表 ∪ 显式豁免（对齐 `AssertAPIKeyScopeCoverage`）。
  4. 补拦截器测试：viewer 调 `DeleteAPIKey` / `UpdateUser` 必须 `PermissionDenied`。

### P1-2（正确性）Functions HTTP 部署上传未注入 Principal，合法请求恒 401

- **位置**：`internal/api/serverhttp/functions_handler.go:148-152`；对照 `internal/app/functions/deployments.go:32-35`、`internal/app/shared/authz.go:43-47`
- **核实**：`authorize` 成功后仍用裸 `r.Context()` 调 `CreateDeployment`。HTTP 不经 gRPC 拦截器，context 无 Principal。`RequireServerWriteActor` 缺 Principal → `Unauthenticated`。现有测试只测 `authorize()`，从未走到 use-case。模块 03 与 07 **独立报出同一缺陷**。
- **影响**：`POST /v1/server/functions/{id}/deployments/code` 对 admin 与带 `functions.write` 的 API Key 均失败。Console / CLI 大包 multipart 部署不可用。gRPC ≤1MiB 路径正常。
- **修复**：`ctx = contexts.WithPrincipal(r.Context(), principal)`；补单测断言 mock `CreateDeployment` 收到的 ctx 含 Principal。

### P1-3（产品契约）Databases schema DDL 拒绝 API Key

- **位置**：`internal/app/server/databases.go` 全部 DDL 入口 `RequirePlatformAdmin`；对照 `apikey_scope.go` 将 DDL 映射为 `databases.write`；`docs/roadmap.md`「Agent 可运行时建库/集合」。
- **核实**：API Key 的 `ActorKind=service`，必然被 `RequirePlatformAdmin` 拒绝。拦截器会放行 `databases.write`。文档 CRUD 无此守卫。这是 G12 同类问题在 Databases 上的残留。
- **影响**：不是越权，是 **Agent-Native 验收被 fail-closed 误伤**。CLI/SDK/Agent 无法用合法 Key 建库建集合。
- **修复**：与 G12 对齐，DDL 改为 `RequireServerWriteActor`，角色仍由 `adminRoleMethodRules` 限 owner/admin。若产品决定 schema 仅限平台 admin，则应在拦截器拒绝 API Key 的 DDL，并改路线图/OpenAPI。

### P1-4（数据完整性）团队邀请不幂等，memberships 无唯一索引

- **位置**：`internal/app/server/teams.go:184-255`；`system_collection_specs.go` memberships 无 `(team_id, user_id)` unique。
- **核实**：重复邀请或重复 `accepted` 会再写一行并 `adjustTeamTotal +1`。`total` 会大于真实成员数。
- **修复**：创建前按 team+user / 规范化 email 查重；系统集合补 unique 索引。

### P1-5（SDK / Agent-Native）TS `FunctionsService` 未挂到 `Torchwood.server`

- **位置**：`sdk/typescript/src/torchwood.ts:24-51`；`src/index.ts` 不导出 `FunctionsService`。实现本身在 `src/server/functions.ts`，契约测试直接 import 类所以仍绿。
- **核实**：`Torchwood.server` 只有 health/projects/users/teams/databases/apiKeys/oauthProviders/storage。按文档 `Torchwood.withApiKey()` 的 Agent **调不到 Functions**。
- **修复**：门面增加 `server.functions`；契约测试断言实例属性覆盖 Server swagger 服务。

### P1-6（SDK）公开 ConfirmEmailChange 仍走登录刷新

- **位置**：`sdk/go/client/auth.go:14-20`；TS `confirmEmailChange` 默认 `auth: "user"`。
- **核实**：`noRefreshMethods` 自称含公开方法，实际只有 SignIn/SignUp/RefreshToken/SignOut。本地有过期 refresh 时，Go 客户端在 RPC 发出前就会失败。
- **修复**：全部 `ACCESS_PUBLIC` 方法加入 `noRefreshMethods`；TS 设 `auth: "none"`；注释改为「凭 user_id+secret，无需登录」。

### P1-7（Console）文件预览 `<img>` 带不上项目头

- **位置**：`console/src/api/storage.ts:272-279`；`console/src/routes/storage/pages.tsx:595-599`；`file_handler.go:529-534`
- **核实**：admin JWT 不含 `ProjectID`；浏览器 `<img src>` 不能带 `X-Torchwood-Project`；cookie 鉴权成功后项目为空直接 `missing project context`，不会回落到 `?token=`。下载走 axios 不受影响。
- **修复**：预览改 `api.get(..., { responseType: "blob" })` + `createObjectURL`；或 URL 加 `project` 查询参数。

---

## 降级 / 不升 P1 的项（主代理裁定）

| 子代理原级 | 项 | 裁定 | 理由 |
|------------|----|------|------|
| 04 P1 | `ListTeams` 丢 `NextPageToken` | **P2** | 真实功能缺陷，第一页可用，非安全。同类：Server ListUsers/Teams/Files。 |
| 11 P1 | 函数批量删除无确认 | **P2** | UX / 误触，后端权限正确。 |
| 12 P1 | Go Client 缺 `DeleteTeam` | **P2** | 单方法缺口，TS/Server 已有。 |
| 01 P2 | account-token 先 GETDEL 再比 secret | **保持 P2** | 不能接管账号，但是公开 Confirm 后可定点作废链接。建议下一轮修。 |
| 02 无 | upsert 冲突列客户端指定 | **不升级** | 提权路径已关；剩余是契约纵深。 |
| 07 无 | Redis List 崩溃丢 queued | **不升级** | 设计文档已声明的 MVP 权衡。 |

---

## 已核实为健康（不再复报）

下列在 Round 1/2 / B1–B3 修过的主路径，本轮对照当前代码确认未回归：

- JWT 强制 HS256、域分离密钥、弱密钥启动拒绝
- refresh Lua 原子轮换；OTP Lua 一次性；TOTP 锁定 + 防重放
- API Key `functions.write` 拦截器 + 启动期 coverage 断言（gRPC 路径）
- API Key 以 `keys` 角色参与 `_perms`，不默认 bypass
- upsert ON CONFLICT 提权修复（advisory lock + 命中行走 update 权限）
- `_perms` 过滤带 `_tenant`；标识符 `quoteIdent`；值参数化
- B1 邮箱 staging：只写 `pending_email`，Confirm 前旧邮箱可登录；`ACCESS_PUBLIC` 为产品决策
- B2 worker `attempt` 写入 payload，旧消息兼容，超限 `MarkExecutionFailed`
- B3 `:count` / `:bulkUpdate` / `:bulkDelete` / `:runtimes` / `:specifications` 与 proto/gateway/TS 一致
- zip slip/bomb/symlink、超限 `RemoveAll`、分片 complete/abort 互斥、file token HMAC
- Console 不再把 token 放 localStorage；cookie HttpOnly + SameSite=Lax
- 144 个 RPC 均有 HTTP 与 authz 注解；缺注解启动失败
- CLI `import_guard_test` 有效；DeleteFactor `code` 三端一致

---

## 建议的下一轮修复批次（H）

按依赖与文件冲突：

| 批次 | 内容 | 模块 | 优先级 |
|------|------|------|--------|
| **H1** | 补全 `adminRoleMethodRules` + `DeleteAPIKey`/`UpdateUser` 用例守卫 + 启动断言 | 01, 06 | 立刻 |
| **H2** | Functions HTTP `WithPrincipal` + 单测 | 03, 07 | 立刻 |
| **H3** | Databases DDL 改为 `RequireServerWriteActor`（或明确拒绝 API Key 并改文档） | 06 | 需产品拍板，建议与 G12 一致放行 |
| **H4** | TS `server.functions` 门面 + ConfirmEmailChange `noRefreshMethods` / `auth: "none"` | 12 | 高 |
| **H5** | Console 预览改 blob；邀请幂等 + unique 索引 | 11, 06 | 高 |
| **H6** | 分页 token 回传、account-token 比对成功才 DEL、改邮箱频控、限流 INCR+EXPIRE 原子化 | 03/04/01/05 | 常规 |

建议顺序：H1 ∥ H2 ∥ H4 → H3（拍板后）→ H5 → H6。

---

## 产品决策待确认

1. **API Key 能否做 Databases schema DDL**（P1-3）。推荐与 G12 相同：放行 `databases.write`，console 角色仍限 owner/admin。
2. **viewer 的「只读」范围**：除 DeleteAPIKey / UpdateUser 外，未入表的文档写、Teams 写、Storage 写目前也被放行。修 H1 时应一次性做成 allowlist/完整 denylist，不要只补两条。

---

## 子代理与主代理分工

- 子代理：12 个 `general-purpose` 审查员，按 `docs/review/prompts/01`–`12` 通读当前源码，各自写入 `docs/review/round3/reports/`。
- 主代理：独立阅读 ConfirmEmailChange、`admin_roles.go`、`apikey_scope.go`、`functions_handler.go`、`users.go` `UpdateUser`、`authz.go`、`torchwood.ts`、`file_handler.go` `resolveReadContext`，对全部 P1 做采纳 / 降级 / 合并。
- 本文件中的 P1-1、P1-2、P1-3、P1-5、P1-7 均经主代理亲自读代码确认，不是转述。
