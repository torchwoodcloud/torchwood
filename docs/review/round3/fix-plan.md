# Torchwood Round 3 修复方案

> 依据：`docs/review/round3/audit-report.md`（主代理交叉核实后的结论）  
> 分报告：`docs/review/round3/reports/01`–`12`  
> 基线：`main` @ `031ce90`（行号会漂移，实施时用 Grep / 读文件重新定位）  
> 产品决策（本方案已拍板，实施方不得再改口）：  
> 1. **H3**：API Key 可做 Databases schema DDL（与 G12 Functions 写方法同一口径）。  
> 2. **H1**：viewer 只读（仅 List/Get/Count）；未入表写方法一律补登，不要只补两条。

本方案是实施的**唯一事实来源**。分报告里被主代理降级或标「不修」的项，不得自行升级回来。

---

## 0. 批次总览

| 批次 | 名称 | 级别 | 建议顺序 | 依赖 |
|------|------|------|----------|------|
| **H1** | Console 角色门禁收口 | P1 安全 | 立刻 | 无 |
| **H2** | Functions HTTP 注入 Principal | P1 正确性 | 立刻（可与 H1 并行） | 无 |
| **H3** | Databases DDL 放行 API Key | P1 产品契约 | 与 H1 并行或紧随 | 无（守卫已存在） |
| **H4** | SDK 门面与公开 RPC | P1 Agent-Native | 可与 H1/H2 并行 | 无 |
| **H5** | Console 预览 + 邀请幂等 | P1 + 数据完整性 | 可并行 | 无 |
| **H6** | 分页 / token 消费 / 频控 | P2 | 最后 | 无 |

并行约定：H1–H5 文件集基本无重叠（见 §8），可同工作区串行或按矩阵并行。H6 会碰 `clientgrpc/teams.go`、`servergrpc/*`、`infra/auth`，与 H1/H5 无文件冲突，但建议 H1 合入后再动拦截器测试，避免抢 `admin_roles_test.go`。

**本轮不修（明确排除）**

- Redis List 崩溃丢 queued（已声明的 MVP 权衡）
- upsert 冲突列由客户端指定（提权路径已关）
- ConfirmEmailChange 保持 `ACCESS_PUBLIC`（产品决策，不是缺陷）
- 改 `proto/` 契约、`task generate-proto`（本轮无 proto 变更）
- 全量 P3、文档修辞、无关重构

---

## 1. H1 Console 角色门禁收口（P1-1）

**目标**：viewer 不能调用任何 Server 写 RPC；`DeleteAPIKey` / `UpdateUser` 即使绕过拦截器也不能改数据。

### H1-1 补全 `adminRoleMethodRules`（完整写方法表，不是补两条）

- 位置：`pkg/grpc/interceptor/admin_roles.go`
- 对照源：`pkg/grpc/interceptor/apikey_scope.go` 的 `apiKeyScopeRules`（`op == "write"` 的每一项都必须入表）
- 角色模型（与文件头注释、Console `useAdminRole` 对齐）：
  - **viewer**：任何写方法都不在允许列表里
  - **member + owner + admin**：业务写
  - **仅 owner + admin**：平台敏感写

**必须入表的方法与允许角色**（实施时再与 `apikey_scope.go` 对一次，漏一项即不合格）：

| 方法 | 允许角色 |
|------|----------|
| `APIKeysService/CreateAPIKey` | 已有：owner, admin |
| `APIKeysService/DeleteAPIKey` | **新增**：owner, admin |
| `UsersService/CreateUser` | 已有：member, owner, admin |
| `UsersService/UpdateUser` | **新增**：owner, admin（可改 email/status，属接管面） |
| `UsersService/UpdateUserPassword` | 已有：owner, admin |
| `UsersService/DeleteUser` | 已有：owner, admin |
| `UsersService/DeleteUserSession` | 已有：owner, admin |
| `UsersService/CreateUserToken` | 已有：owner, admin |
| `DatabasesService` 全部 schema DDL（Create/Delete Database/Collection/Attribute/Index、UpdateCollection） | 已有：owner, admin |
| `DatabasesService/CreateDocument` `UpdateDocument` `UpsertDocument` `DeleteDocument` `BulkUpdateDocuments` `BulkDeleteDocuments` | **新增**：member, owner, admin |
| `TeamsService/CreateTeam` `DeleteTeam` `CreateMembership` `UpdateMembership` `UpdateMembershipStatus` `DeleteMembership` `UpdateTeamPrefs` | **新增**：member, owner, admin |
| `StorageService/CreateBucket` | 已有：member, owner, admin |
| `StorageService/UpdateBucket` `DeleteBucket` `CreateFile` `DeleteFile` `UpdateFile` `CreateFileToken` | **新增**：member, owner, admin |
| `ProjectsService/CreateProject` | **新增**：owner, admin（与 use-case `RequirePlatformAdmin` 一致） |
| `ProjectsService/UpdateProject` | 已有：member, owner, admin |
| `OAuthProvidersService/UpsertOAuthProvider` `DeleteOAuthProvider` | 已有：owner, admin |
| `FunctionsService` 全部写方法 | 已有：owner, admin |

**禁止**把 List/Get/Count/`GetVariables` 写入该表（viewer 必须能读）。

### H1-2 启动期 fail-closed 断言

- 位置：仿 `AssertAPIKeyScopeCoverage`（`apikey_scope.go:191-221`），新增 `AssertAdminRoleWriteCoverage`。
- 规则：`apiKeyScopeRules` 中所有 `op=="write"` 的 full method ⊆ `adminRoleMethodRules` 的 key 集合；表中多出来的、或指向读方法的 key，同样 panic。
- 调用点：`internal/infra/server/grpc.go` 的 `NewGRPCServer`，紧挨现有 scope 断言。
- 单测：构造缺失/多余各一条，断言函数会列出方法名（可用 recover 或抽可测的 compare 函数，不要真的在测试里搞崩进程以外的方式——与现有 coverage 测试风格对齐）。

### H1-3 use-case 纵深防御

| 函数 | 文件 | 守卫 |
|------|------|------|
| `APIKeys.Delete` | `internal/app/server/apikeys.go` | `RequirePlatformAdmin`（与 Create 一致） |
| `Users.UpdateUser` | `internal/app/server/users.go` | `RequireServerWriteActor`（API Key + admin 会话可过；端用户/匿名拒）。角色细粒度交给 H1-1。 |

不要给 `Teams` 的写方法套 `RequireServerWriteActor`：Client Teams API 复用同一 use-case，端用户必须能邀请/退出。

### H1-4 测试（必须断言真实行为）

在 `pkg/grpc/interceptor/admin_roles_test.go` / `jwt` 拦截器测试中新增：

1. 覆盖表：每个 `op=="write"` 方法都有规则；读方法不在表内。
2. viewer 调 `DeleteAPIKey`、`UpdateUser`、`CreateDocument`、`CreateTeam`、`DeleteBucket`、`CreateFileToken` → `PermissionDenied`。
3. member 调 `DeleteAPIKey` / `UpdateUser` → `PermissionDenied`；调 `CreateDocument` / `CreateTeam` → 过拦截器（可以在业务校验处失败，但不得是 `admin_role_denied`）。
4. use-case：`APIKeys.Delete` 对 service actor / 端用户拒绝；`UpdateUser` 对端用户/匿名拒绝，对 admin/service 进入业务校验。

**验收**：`go vet ./pkg/grpc/interceptor/... ./internal/app/server/... ./internal/infra/server/...`；`go test -short` 上述包全绿。启动路径不得因漏登 panic（断言与表一致）。

---

## 2. H2 Functions HTTP 注入 Principal（P1-2）

**目标**：`POST /v1/server/functions/{functionId}/deployments/code` 对已鉴权的 admin / `functions.write` API Key 不再恒 401。

### H2-1 注入 Principal

- 位置：`internal/api/serverhttp/functions_handler.go` 的 `upload`
- 现状：`authorize` 成功后仍用裸 `r.Context()` 调 `CreateDeployment`。
- 方案：鉴权成功后

```go
ctx = contexts.WithPrincipal(r.Context(), principal)
```

再把这个 `ctx` 传给 `CreateDeployment`。`contexts` 包：`internal/pkg/contexts`。

不要改 `RequireServerWriteActor` 去「context 没有 Principal 就放行」——那是拆纵深防御。

### H2-2 测试

现有 `functions_handler_test.go` 只覆盖 `authorize` 失败。新增一条（不依赖 Docker）：

- handler 注入可记录 ctx 的 fake Functions use-case（或现有 mock 模式）。
- 用 admin JWT 或带 `functions.write` 的 API Key 走完 `upload`（最小合法 zip 魔数 `PK\x03\x04` + 足够短的 body）。
- 断言 `CreateDeployment` 被调用，且 `contexts.Principal(ctx)` 成功、`ActorKind` 为 admin 或 service。
- 对照：不注入时这条测试必须失败（用来证明修的是根因）。

**验收**：`go test -short ./internal/api/serverhttp/...` 全绿；人工确认 `upload` 再无裸 `r.Context()` 调写 use-case。

---

## 3. H3 Databases DDL 放行 API Key（P1-3）

**已拍板**：与 G12 相同，API Key 持 `databases.write` 可做 schema；console viewer/member 仍由 H1 拦截器拦 DDL（DDL 已是 owner/admin）。

### H3-1 替换守卫

- 位置：`internal/app/server/databases.go` 全部 schema 入口（Create/Delete Database/Collection/Attribute/Index、UpdateCollection），当前 `shared.RequirePlatformAdmin`。
- 改为 `shared.RequireServerWriteActor`（注意本文件已 `import` 的是 `internal/app/shared` 还是别名——保持与文件现有 import 一致）。
- **不要**改文档 CRUD 路径（本来就没有 PlatformAdmin 守卫）。
- **不要**改系统集合写拒绝、default 库保护、标识符校验。

### H3-2 测试

仿 `internal/app/functions/authz_test.go` 的 `TestFunctionsWriteMethods_RequireServerWriteActor`：

- 端用户 / 匿名 → 拒
- API Key（service）/ 各角色 admin 会话 → 过守卫、进入业务校验（非法 id 等）
- 覆盖全部改动过的 DDL 方法，不要只测 CreateDatabase

**验收**：`go test -short ./internal/app/server/...` 全绿。

---

## 4. H4 SDK 门面与公开 RPC（P1-5、P1-6 + 降级后的 DeleteTeam）

### H4-1 TS `FunctionsService` 挂到门面

- `sdk/typescript/src/torchwood.ts`：`server` 增加 `functions: FunctionsService`，构造时 `new FunctionsService(this.transport)`。
- 从 `./server/index.js` 补 import（该文件已 `export { FunctionsService }`）。
- `sdk/typescript/src/index.ts`：至少保证经 `Torchwood.server.functions` 可达。若现有导出策略是「只导出门面」，不要为了导出类而打乱；门面可达即可。
- 契约测试：断言 `new Torchwood({endpoint:"http://x", projectId:"p"}).server.functions` 存在，且 Server swagger 里 `FunctionsService_*` 的方法都能在实例上找到。现有 `contract.test.ts` 直接 import 类会漏这个洞，必须补门面断言。

### H4-2 公开 ConfirmEmailChange 不走登录刷新

**Go**（`sdk/go/client/auth.go`）：

- `noRefreshMethods` 注释写「公开方法 + SignOut」，必须名副其实。
- 把 `proto/client/v1/account.proto` 里所有 `ACCESS_PUBLIC` 的 Account RPC 的 `*_FullMethodName` 加进去（至少：SignIn/SignUp/RefreshToken/SignOut 已有；**必须加** `ConfirmEmailChange`，并 grep 补齐 Email OTP / Phone OTP / Magic URL / Recovery / Verification / Anonymous / OAuth / MFA challenge 等公开方法）。
- `sdk/go/client/account.go` 的 `ConfirmEmailChange` 注释改为：公开，凭 `user_id`+`secret`，无需登录。

**TS**（`sdk/typescript/src/client/account.ts`）：

- `confirmEmailChange` 的 request 加 `auth: "none"`（同文件其它公开方法已这样写）。
- 注释与 Go 对齐。

### H4-3 Go Client 补 `DeleteTeam`（原 12 报告 P1，主代理降为 P2，本批次顺手做）

- `sdk/go/client/teams.go` 增加 `DeleteTeam`，对齐 TS / proto。
- 补 bufconn / fake 测试。

**验收**：`cd sdk/typescript && npm test`；`go test ./sdk/go/client/... ./sdk/go/server/...`。不要改 `proto/`。

---

## 5. H5 Console 预览 + 邀请幂等（P1-7、P1-4）+ 同文件 P2

### H5-1 文件预览改走 axios blob（P1-7）

- 现状：`console/src/api/storage.ts` 的 `filePreviewUrl` 只拼路径；`storage/pages.tsx` 用 `<img src={filePreviewUrl(...)}>`。浏览器 GET 带不上 `X-Torchwood-Project`，admin JWT 又没有 ProjectID，`file_handler.resolveReadContext` 在 cookie 鉴权成功后直接 `missing project context`。
- 方案（选 A，不要做 URL 加 `?project=` 除非 A 不可行）：
  1. 新增 `previewFile(bucketId, fileId)`：`api.get` 预览路径，`responseType: "blob"`，与 `downloadFile` 同路自动带头。
  2. 详情页用 `useEffect`/`useQuery` 拉 blob → `URL.createObjectURL` → `<img src={objectUrl}>`，卸载时 `revokeObjectURL`。
  3. 失败时显示占位，不要裂图死静默。
- **不要**改 `file_handler.resolveReadContext` 的「cookie 优先于 token」语义（跨模块，超出本轮；用 blob 即可规避）。

### H5-2 `canWrite` fail-closed（原 11 P2-1，同目录顺手）

- `console/src/hooks/useAdminRole.ts`：`canWrite` 改为显式白名单 `owner | admin | member`；`undefined` / `viewer` 为 false。
- 分享链接按钮按 `writeable` 隐藏（`storage/pages.tsx`）。

### H5-3 函数批量删除加确认（原 11 P1，降为 P2，同目录顺手）

- `console/src/routes/functions/pages.tsx`：批量删除改用已有 `BulkDeleteButton`，与 users/storage/databases 对齐。

### H5-4 团队邀请幂等（P1-4）

- 位置：`internal/app/server/teams.go` `CreateMembership`
- 在 `CreateDocument` 之前：
  1. 邮箱走与 Users 相同的 normalize（`ToLower` + `TrimSpace`）。
  2. 若已解析出 `userID`：按 `team_id` + `user_id` 查是否已有 membership。
  3. 若有 email：按 `team_id` + 规范化 email 再查一次。
  4. 已存在 → `AlreadyExists`（不要再 `+1 total`）。pending 重复邀请同样 `AlreadyExists`，调用方重试即可。
- 唯一索引：
  - 在 `system_collection_specs.go` 的 memberships 上增加 unique 索引。注意：pending 邀请可能 `user_id` 为空、仅 user_id 邀请可能 `email` 为空。**不要**对可空列建会把空串撞车的 unique。
  - 推荐：只做应用层查重（必须）+ 若 adapter 能把空值存成 SQL NULL，再加 `unique(team_id, email)` / `unique(team_id, user_id)` 部分唯一；若做不到，**只做应用层**并在 fix-report 写明。
  - reconcile 对存量项目可能不补索引（已知限制）。应用层查重是存量项目的真实修复。
- 测试：同一 user / 同一 email 第二次 `CreateMembership` → `AlreadyExists`，`total` 不增加；accepted 重复同样。

**验收**：`go test -short ./internal/app/server/...`；Console 改动后 `task console-build`（否则 Go embed 打旧包）。不要为了预览去启动浏览器。

---

## 6. H6 分页 / token 消费 / 频控（P2）

只做下列条目，不要把分报告里所有 P2/P3 塞进来。

### H6-1 列表回传 `NextPageToken`

handler 用 `_` 丢掉 use-case 第三返回值的，全部改成写入 `Meta.NextPageToken`（对照 `ListDocuments`）：

| 文件 | RPC |
|------|-----|
| `internal/api/clientgrpc/teams.go` | `ListTeams` |
| `internal/api/servergrpc/users.go` | `ListUsers` |
| `internal/api/servergrpc/teams.go` | `ListTeams`、`ListMemberships` |
| `internal/api/servergrpc/storage.go` | `ListBuckets`、`ListFiles` |

本轮**不**给 `ListAPIKeys` / `ListFunctions` / `ListOAuthProviders` / `ListDeployments` / `ListExecutions` 新做分页（它们根本没把 `ListRequest` 传进仓储）。不要顺手重构那些列表。

### H6-2 `GetBucket` 改 `query.BuildEqual`

- 位置：`internal/api/servergrpc/storage.go` 手拼 `equal("$id","` + id + `")`
- 改为 `query.BuildEqual("$id", req.GetId())`，与 `ListFiles` / 公开 bucket HTTP 路径一致。

### H6-3 account-token：比对成功才删除

- 位置：`internal/infra/auth/account_token_redis.go` `verifyTokenWithEmail`
- 现状：先 `GETDEL` 再比 hash → 错 secret 也烧掉 token（`ConfirmEmailChange` / recovery / magic / verification 全中招）。
- 方案：与 `otp_store_redis.go` 一样用 Lua：hash 匹配才 `DEL`，否则只 `HINCRBY`（或等价计数），超过 N 次（建议 5，与 OTP 对齐）再删并锁定。错 secret 仍返回统一 `Unauthenticated`（防枚举）。
- 更新 `account_token_redis_test.go`：错 secret **不得**删除记录；对 secret 第二次使用才 Unauthenticated。
- 改邮箱发送：`internal/app/client/account.go` 签发 email_change token 前调用已有 `CheckSendRateLimit`（与 verification/recovery/magic 一致）。

### H6-4 限流 INCR + EXPIRE 原子化

- 位置：`login_throttle_redis.go`、`ratelimit_redis.go`、`otp_store_redis.go` 的 IP 窗口
- 现状：`INCR` 后若 `count==1` 再 `EXPIRE`；进程在中间崩溃会留下无 TTL 键，永久锁死。
- 方案：Lua，或 `SET key 1 EX ttl NX` + 已存在则 `INCR`。失败路径不得留下无 TTL 计数器。
- 补测试：至少一条证明首次计数带 TTL。

**验收**：`go test -short ./internal/infra/auth/... ./internal/api/clientgrpc/... ./internal/api/servergrpc/... ./internal/app/client/...`

---

## 7. 各批次验收命令（汇总）

| 批次 | 命令 |
|------|------|
| H1 | `go test -short ./pkg/grpc/interceptor/... ./internal/app/server/... ./internal/infra/server/...` |
| H2 | `go test -short ./internal/api/serverhttp/...` |
| H3 | `go test -short ./internal/app/server/...` |
| H4 | `go test ./sdk/go/client/... ./sdk/go/server/...`；`cd sdk/typescript && npm test` |
| H5 | `go test -short ./internal/app/server/...`；`task console-build` |
| H6 | `go test -short ./internal/infra/auth/... ./internal/api/clientgrpc/... ./internal/api/servergrpc/... ./internal/app/client/...` |
| 全部完成后 | `go vet ./...`；`go build ./...`；`go test -short ./...`；`task console-build` |

不跑需要 Postgres/Redis/MinIO/Docker 的集成测试（`go test -short` 已跳过）。不要为了验证去改 CI 配置。

---

## 8. 文件冲突矩阵

| 批次 | 主要写入文件 | 不要碰 |
|------|----------------|--------|
| H1 | `pkg/grpc/interceptor/admin_roles.go`、`admin_roles_test.go`、（可）`jwt` 测试、`apikey_scope.go` 旁新增 assert、`internal/infra/server/grpc.go`、`internal/app/server/apikeys.go`、`users.go`、对应 `*_test.go` | functions_handler、console/src、sdk、databases.go DDL 守卫（H3） |
| H2 | `internal/api/serverhttp/functions_handler.go`、`functions_handler_test.go` | `RequireServerWriteActor` 实现、deployments 业务逻辑 |
| H3 | `internal/app/server/databases.go`、`authz_test.go` 或新建 DDL 守卫测试 | documentdb adapter、系统集合拒绝逻辑 |
| H4 | `sdk/typescript/src/torchwood.ts`、`index.ts`、`client/account.ts`、`__tests__/contract.test.ts`、`sdk/go/client/auth.go`、`account.go`、`teams.go`、对应测试 | proto/、cmd/client、console |
| H5 | `console/src/api/storage.ts`、`routes/storage/pages.tsx`、`hooks/useAdminRole.ts`、`routes/functions/pages.tsx`、`internal/app/server/teams.go`、`system_collection_specs.go`（仅当能安全加 unique）、对应测试 | file_handler 鉴权语义、Client teams 传输层 |
| H6 | `clientgrpc/teams.go`、`servergrpc/{users,teams,storage}.go`、`account_token_redis.go`、`login_throttle_redis.go`、`ratelimit_redis.go`、`otp_store_redis.go`、`internal/app/client/account.go`、对应测试 | admin_roles.go（H1）、functions_handler（H2） |

同工作区并行时：H1∥H2∥H4∥H5 安全；H3 与 H1 都动 `internal/app/server` 测试，建议串行或 H3 只改 `databases.go` + 独立测试文件。H6 最后做。

---

## 9. 实施方对方案的审核义务

第三方 agent **必须先审方案再写代码**。审核要回答：

1. 当前代码是否仍存在方案描述的缺陷（行号可能已变，以读到的代码为准）？
2. 方案是否会误伤合法路径（尤其 H1 member 业务写、H3 API Key DDL、Client Teams 复用）？
3. 测试是否能抓住根因，而不是恒真断言？
4. 有无方案写错、遗漏的写 RPC、或与 G12/B1 已落地行为冲突？

把审核结论写到 `docs/review/round3/plan-review.md`。若发现方案有误：**记录偏差与替代做法，按替代做法实现**，不要既不改方案也不说明就自行发挥。

---

## 10. 完成后必须交付

1. `docs/review/round3/plan-review.md` — 方案审核
2. 工作区代码改动（**不要 git commit / push / 改 remote**）
3. `docs/review/round3/fix-report.md` — 逐项对照本方案：✅ 已修 / ❌ 未修 / ⚠️ 偏差（原因）
4. 每批次验证命令的出口状态

上级（原审查方）会在实现完成后再做一次深度审查。不要在 fix-report 里写「请原审查方继续改代码」。
