# Round 3 方案审核

> 审核人：第三方实施 agent（独立于原审查方）
> 审核对象：`docs/review/round3/fix-plan.md`（H1–H6）
> 对照基线：当前工作区 `main`（`git log -1` 未变动，行号以审核时读到的代码为准）

## 缺陷是否仍在

| 批次 | 结论 | 证据（当前代码） |
|------|------|------------------|
| H1 | **仍在** | `pkg/grpc/interceptor/admin_roles.go:14-49` 表缺失：`DeleteAPIKey`、`UpdateUser`、Databases 文档 CRUD 6 个写、Teams 全部 7 个写、Storage 6 个写、`CreateProject`；`internal/app/server/apikeys.go:124` `Delete` 无守卫；`internal/app/server/users.go:138` `UpdateUser` 无 actor 守卫；`pkg/grpc/interceptor/jwt.go:131` 仅当表项非空才拦角色 |
| H2 | **仍在** | `internal/api/serverhttp/functions_handler.go:148` 鉴权成功后仍用裸 `r.Context()` 调 `CreateDeployment` |
| H3 | **仍在** | `internal/app/server/databases.go:50/83/98/141/154/178/204/226/245` 共 9 处 DDL 入口 `shared.RequirePlatformAdmin` |
| H4 | **仍在** | `sdk/typescript/src/torchwood.ts:24-33` `server` 无 `functions`；`sdk/go/client/auth.go:14-20` `noRefreshMethods` 仅 4 项；`sdk/typescript/src/client/account.ts:97-108` `confirmEmailChange` 无 `auth:"none"`；`sdk/go/client/teams.go` 无 `DeleteTeam` |
| H5 | **仍在** | `console/src/api/storage.ts:278-280` `filePreviewUrl` 只拼路径、`console/src/routes/storage/pages.tsx:595-599` `<img>` 直连；`console/src/hooks/useAdminRole.ts:15-17` `canWrite` 是 denylist（`role !== "viewer"`）；`console/src/routes/functions/pages.tsx:195-209` 批量删除无确认；`internal/app/server/teams.go:184-255` `CreateMembership` 在 `CreateDocument` 前无查重 |
| H6 | **仍在** | `clientgrpc/teams.go:34`、`servergrpc/users.go:61`、`servergrpc/teams.go:52/155`、`servergrpc/storage.go:58/161` 全用 `_` 丢 token；`servergrpc/storage.go:82` 手拼 `equal("$id","`+id+`")`；`internal/infra/auth/account_token_redis.go:120-140` 先 `GETDEL` 再比 hash；`login_throttle_redis.go:69-83`、`ratelimit_redis.go:23-41`、`otp_store_redis.go:85-101` 均 `INCR` 后 `count==1` 才 `EXPIRE`；`internal/app/client/account.go:515-529` email_change 签发前无 `CheckSendRateLimit` |

结论：fix-plan 描述的全部缺陷在当前代码中仍然存在，方案可行。

## 误伤风险评估

1. **H1 member 业务写**：方案把 Databases 文档 CRUD、Teams 写、Storage 写（除 API Key 管理/DDL）放开给 `member/owner/admin`，与文件头注释的角色模型、`apikey_scope.go` 的 write 口径一致，未误伤。`CreateProject` 登记为 owner/admin 与 use-case `projects.go:44-55` 的平台 admin 守卫一致（use-case 是 `ActorKind==admin && IsPlatformAdmin`，拦截器角色 owner/admin 正好是其前置子集，两层一致）。`UpdateUser` 拦截器 owner/admin + use-case `RequireServerWriteActor`：API Key（service）经 use-case 放行、角色门禁不适用于 API Key——与方案「角色细粒度交给 H1-1、use-case 只挡凭证类型」一致，API Key 改用户本来就是 Server API 的合法 `users.write` 能力。
2. **H3 API Key DDL**：与已落地 G12 Functions 口径一致（`RequireServerWriteActor` 放行 service + admin 会话）；console viewer/member 仍被 H1 拦截器拦 DDL；文档 CRUD（无 PlatformAdmin 守卫）不动；系统集合拒绝、default 库保护、标识符校验保持。
3. **Client Teams**：不给 Teams 写方法套 `RequireServerWriteActor`（方案明示），端用户邀请/退出路径不受影响。
4. **公开 ConfirmEmailChange**：`noRefreshMethods` 只影响 Go 客户端「过期 refresh 提前刷新」逻辑，不触碰服务端 `ACCESS_PUBLIC` 语义，无误伤。
5. **H5-4 唯一索引**：`buildInsertParts`（`postgres.go:1613-1628`）把值原样传参，空串存为 `''` 而非 SQL NULL；普通 `unique(team_id, email)`/`unique(team_id, user_id)` 会让「多个 pending 邀请 user_id 为空」撞车，会破坏合法邀请流程。方案本身已给 fallback（「若做不到，只做应用层并在 fix-report 写明」），采纳应用层查重方案。
6. **H5-2 canWrite fail-closed**：viewer 将看不到写按钮/分享链接按钮（分享链接 = CreateFileToken 写操作），与后端拦截器收紧方向一致，属正确收敛。

## 方案问题与偏差

1. **H6-1 ListBuckets 需要改 use-case 签名（方案未写明）**：`internal/app/storage/storage.go:107-125` `ListBuckets` 本身丢弃 `list.NextPageToken`，返回签名 `([]storage.Bucket, int64, error)`，handler 层没有第三返回值可写。必须把 use-case 签名扩为 `([]storage.Bucket, int64, string, error)`，并同步 3 个调用点（`servergrpc/storage.go:58/81`、`serverhttp/file_handler.go:554`）与 2 个集成测试。这是 H6-1 表格包含 ListBuckets 的隐含前置，实施时一并做。
2. **H6-3 计数不能直接 HINCRBY**：account-token record 是整体 JSON 字符串（`account_token_redis.go:23-29`），对同一 key `HINCRBY` 会 WRONGTYPE。改用 Lua 内 `cjson.decode` + 记录内嵌 `attempts` 字段 + 回写 `SET`（保留 `PTTL`）的等价计数；超 5 次（与 OTP 对齐）`DEL` 锁定。所有失败路径统一返回 `Unauthenticated`（防枚举，方案明示）。
3. **H2 测试无法用「可记录 ctx 的 fake use-case」**：handler 字段类型是具体 `*appfunctions.Functions`（`functions_handler.go:32`），没有接口缝。改用「现有 mock 模式」：真实 `Functions` use-case + 本地 mock `FunctionRepo`/`Executor`，由 recording executor 在 `Build` 处记录 ctx 并断言 `contexts.Principal` 存在、ActorKind 为 admin/service，响应 201。修复前该测试在 `RequireServerWriteActor` 处 401（executor 不会被调），测试失败——满足「不注入时测试必须失败」的对照要求。
4. **H4-2 范围微扩**：`noRefreshMethods` 注释自称「公开方法 + SignOut」（`auth.go:14`），方案列举只到 account.proto。client databases 的 `ListDocuments/GetDocument/CountDocuments` 同为 `ACCESS_PUBLIC`（`proto/client/v1/databases.proto`），同样会被过期 refresh 拦截。按方案「注释必须名副其实」的原则把这三个也加入，属 H4-2 的合理收口，非新增范围。
5. **H1-2 断言对称性成立**：`apiKeyScopeRules` 全部条目都是 serverv1 方法（client 服务均为 `ACCESS_AUTHENTICATED`，启动断言 `AssertAPIKeyScopeCoverage` 已证明集合一致），「write 方法 ⊆ 角色表 ∧ 角色表 key ⊆ write 方法」的对称断言可安全成立。
6. **H1 表无「漏登」之外的多余项**：现有表 key 全部是 write 方法且都在 `apiKeyScopeRules` 中，断言不会在启动期误伤。

## 将采用的实施口径

**完全按方案实施**，附带下列已在上面论证的偏差（fix-report 将再记一次）：

- **偏差 A（H6-1）**：`Storage.ListBuckets` 签名扩展返回 NextPageToken，同步全部调用点。
- **偏差 B（H6-3）**：Lua 用「JSON 内嵌 attempts + 回写保留 PTTL」实现计数与锁定，替代 HINCRBY。
- **偏差 C（H4-2）**：`noRefreshMethods` 额外纳入 client databases 的 3 个 `ACCESS_PUBLIC` 方法。
- **偏差 D（H2）**：测试用真实 use-case + mock repo/executor + recording executor 断言 ctx Principal。
- **偏差 E（H5-4）**：唯一索引只做应用层查重（空串非 NULL 的适配器事实），`system_collection_specs.go` 不加 unique，fix-report 写明原因。

其余（H1 表内容、`AssertAdminRoleWriteCoverage` fail-closed、H3 守卫替换、H5-1 blob 预览、H5-2/H5-3、H6-2/H6-4）完全按方案正文执行。
