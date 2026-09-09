# Round 3 修复报告

> 实施：第三方实施 agent（独立于原审查方）
> 方案审核：`docs/review/round3/plan-review.md`
> 基线：当前工作区 `main`（未 commit/push/checkout，改动全部留在工作区）

## 方案审核摘要

方案审核结论：**通过，带 5 项论证过的偏差实施**（详见 plan-review.md「将采用的实施口径」）。

| 偏差 | 内容 | 原因 |
|------|------|------|
| A（H6-1） | `Storage.ListBuckets` use-case 签名扩展返回 NextPageToken | use-case 本身丢弃 token，handler 层无第三返回值可写（fix-plan 未写明此前置） |
| B（H6-3） | Lua 用「JSON 内嵌 attempts + 回写保留 PTTL」计数 | record 是整体 JSON 字符串，`HINCRBY` 会 WRONGTYPE |
| C（H4-2） | `noRefreshMethods` 额外纳入 client databases 的 3 个 `ACCESS_PUBLIC` 方法 | 按方案「注释写『公开方法 + SignOut』，必须名副其实」的原则收口 |
| D（H2） | 测试用真实 use-case + mock repo/executor + recording executor 断言 ctx Principal | handler 字段是具体类型，无 fake use-case 接口缝 |
| E（H5-4） | 唯一索引只做应用层查重 | adapter 空串存 `''` 非 SQL NULL，普通 unique 会让 pending 邀请撞车（方案已给此 fallback） |

## 逐项对照

### H1 Console 角色门禁收口

| ID | 项 | 状态 | 证据（文件:行） | 验证命令 |
|----|----|------|-----------------|----------|
| H1-1 | 补全 `adminRoleMethodRules`（全部写方法入表；viewer 只读） | ✅ | `pkg/grpc/interceptor/admin_roles.go:14-99`（39 项写方法；`DeleteAPIKey`/`UpdateUser`/文档 CRUD 6/Teams 7/Storage 6/`CreateProject` 全部补登） | `go test -short ./pkg/grpc/interceptor/...` |
| H1-2 | 启动期 `AssertAdminRoleWriteCoverage` fail-closed | ✅ | `pkg/grpc/interceptor/apikey_scope.go:223-274`（可测纯函数 `adminRoleWriteCoverageDiff`）；调用点 `internal/infra/server/grpc.go:76-79`；单测 `admin_roles_test.go:TestAdminRoleWriteCoverageDiff_DetectsMissingAndExtra` | 同上 |
| H1-3 | use-case 纵深防御：`APIKeys.Delete`→`RequirePlatformAdmin`；`UpdateUser`→`RequireServerWriteActor`；Teams 不套守卫 | ✅ | `internal/app/server/apikeys.go:124-130`；`internal/app/server/users.go:138-147`；teams.go 未动 | `go test -short ./internal/app/server/...` |
| H1-4 | 测试断言真实行为（viewer 403、member 细粒度、读方法不入表） | ✅ | `pkg/grpc/interceptor/admin_roles_test.go:227-338`（覆盖表对称断言、viewer 拒 6 写方法、member 拒接管面/放行业务写）；`authz_test.go:TestUsers_UpdateUser_RequiresServerWriteActor`；`apikeys_test.go:TestAPIKeys_Delete_RequiresPlatformAdmin` | 同上 |

### H2 Functions HTTP 注入 Principal

| ID | 项 | 状态 | 证据（文件:行） | 验证命令 |
|----|----|------|-----------------|----------|
| H2-1 | 鉴权成功后 `contexts.WithPrincipal` 注入再调 `CreateDeployment` | ✅ | `internal/api/serverhttp/functions_handler.go:148-152`（`ctx = contexts.WithPrincipal(r.Context(), principal)`）；`RequireServerWriteActor` 未削弱 | `go test -short ./internal/api/serverhttp/...` |
| H2-2 | 测试：admin JWT 与 `functions.write` API Key 走完 upload，executor 收到含 Principal 的 ctx；不注入时测试必失败 | ✅ | `functions_handler_test.go:TestFunctionsHandler_Upload_InjectsPrincipalIntoCtx`（断言 201 + `contexts.Principal` 的 ActorKind 为 admin/service；修复前在守卫处 401、`builds==0`，测试失败） | 同上 |

### H3 Databases DDL 放行 API Key

| ID | 项 | 状态 | 证据（文件:行） | 验证命令 |
|----|----|------|-----------------|----------|
| H3-1 | 9 处 DDL 守卫改 `RequireServerWriteActor`；文档 CRUD/系统集合拒绝/default 保护不动 | ✅ | `internal/app/server/databases.go:50/83/98/141/154/178/204/226/245`；`internal/app/shared/authz.go:12-20` 注释同步 | `go test -short ./internal/app/server/...` |
| H3-2 | 测试覆盖全部改动 DDL 方法（端用户/匿名拒；service/各角色 admin 过守卫；系统集合与 default 保护保持） | ✅ | `internal/app/server/databases_ddl_authz_test.go`（`ddlCalls` 覆盖 9 方法 + `TestDatabases_DDLMethods_KeepSystemCollectionAndDefaultProtection`） | 同上 |

### H4 SDK 门面与公开 RPC

| ID | 项 | 状态 | 证据（文件:行） | 验证命令 |
|----|----|------|-----------------|----------|
| H4-1 | TS `server.functions` 挂到门面 + 契约测试门面可达 | ✅ | `sdk/typescript/src/torchwood.ts:6-9,26-33,47-53`；`__tests__/contract.test.ts:Test "Torchwood.server 门面可达全部 Server swagger 服务（含 functions）"`（全部 server 服务逐一断言） | `cd sdk/typescript && npm test` |
| H4-2 | `noRefreshMethods` 名副其实 + TS `auth:"none"` + 注释对齐 | ✅（含偏差 C） | `sdk/go/client/auth.go:14-38`（19 个 Account 公开方法 + SignOut + 3 个 client databases 公开方法）；`sdk/go/client/account.go:94-96`；`sdk/typescript/src/client/account.ts:96-108` | `go test ./sdk/go/client/...`；`npm test` |
| H4-3 | Go Client 补 `DeleteTeam` + bufconn 测试 | ✅ | `sdk/go/client/teams.go:52-56`；`services_test.go:fakeTeams.DeleteTeam` + `TestClientTeams_DeleteTeam`（断言 Bearer 头） | `go test ./sdk/go/client/...` |

### H5 Console 预览 + 邀请幂等

| ID | 项 | 状态 | 证据（文件:行） | 验证命令 |
|----|----|------|-----------------|----------|
| H5-1 | 预览走 axios blob + objectURL + revoke + 失败占位 | ✅ | `console/src/api/storage.ts:previewFile`；`console/src/routes/storage/pages.tsx`（useEffect 拉 blob→createObjectURL→卸载 revoke；失败显示占位）；`file_handler` cookie-优先语义未动 | `task console-build` |
| H5-2 | `canWrite` 显式白名单 fail-closed；分享链接按钮按 writeable 隐藏 | ✅ | `console/src/hooks/useAdminRole.ts:15-22`；`storage/pages.tsx` 分享按钮包 `{writeable && ...}` | 同上 |
| H5-3 | 函数批量删除改 `BulkDeleteButton` 确认 | ✅ | `console/src/routes/functions/pages.tsx`（selectionActions 换 `BulkDeleteButton`，与 users/storage 对齐） | 同上 |
| H5-4 | 邀请幂等：CreateDocument 前按 team+user/规范化 email 查重；`total` 不再虚增 | ✅（含偏差 E） | `internal/app/server/teams.go:232`（调用点）+ `ensureMembershipUnique:539-567`；测试 `teams_memberships_dedupe_test.go`（同 user 重复/大小写 email 解析同 user/pending 重复→AlreadyExists 且 total 不变；新邀请成功才 +1） | `go test -short ./internal/app/server/...` |
| H5-4 附 | memberships unique 索引 | ⚠️ 未加 | `buildInsertParts`（`postgres.go:1613-1628`）空串存 `''` 非 NULL；普通 unique 会让多个空 user_id 的 pending 邀请撞车。按方案 fallback 只做应用层查重 | — |

### H6 分页 / token 消费 / 频控

| ID | 项 | 状态 | 证据（文件:行） | 验证命令 |
|----|----|------|-----------------|----------|
| H6-1 | List 回传 `Meta.NextPageToken`（6 处 handler） | ✅（含偏差 A） | `clientgrpc/teams.go:34-50`、`servergrpc/users.go:56-77`、`servergrpc/teams.go:47-68/150-171`、`servergrpc/storage.go:53-74/150-173`；use-case `internal/app/storage/storage.go:109-129` 签名扩展并同步 `serverhttp/file_handler.go:554` 与 2 个集成测试调用点；测试 `servergrpc/pagination_test.go` + `clientgrpc/teams_pagination_test.go`（token 断言） | `go test -short ./internal/api/clientgrpc/... ./internal/api/servergrpc/...` |
| H6-2 | `GetBucket` 改 `query.BuildEqual` | ✅ | `internal/api/servergrpc/storage.go:81-99`；测试含引号 id 不再解析报错（NotFound） | 同上 |
| H6-3 | account-token 比对成功才删除；错 secret 计数、超限锁定；改邮箱发送频控 | ✅（含偏差 B） | `internal/infra/auth/account_token_redis.go:36-66`（Lua `accountTokenVerifyScript`，cjson 内嵌 attempts、PTTL 保留、`AccountTokenMaxAttempts=5`、统一 Unauthenticated）；`account_token_redis_test.go`（错 secret 不删记录/正确 secret 仍可消费、超限锁定、TTL 保持）；`internal/app/client/account.go:515-524`（签发前 `CheckSendRateLimit`） | `go test -short ./internal/infra/auth/... ./internal/app/client/...` |
| H6-4 | 限流 INCR+EXPIRE 原子化（login throttle / ratelimit / OTP IP 窗口） | ✅ | `internal/infra/auth/incr_ttl.go`（Lua `incrWithTTLScript`）；`login_throttle_redis.go:69-78`、`ratelimit_redis.go:23-33`、`otp_store_redis.go:85-96`；测试 `incr_ttl_test.go`（三个限流器首次计数带 TTL + OTP cooldown 语义保持） | 同上 |

## 验证

| 命令 | exit code |
|------|-----------|
| `go vet ./...` | 0 |
| `go build ./...` | 0 |
| `go test -short ./...` | 0 |
| `task console-build` | 0 |
| `cd sdk/typescript && npm test` | 0（17/17 pass，含新增门面契约测试） |
| `cd sdk/go && go test ./client/... ./server/...` | 0 |
| 批次 H1：`go test -short ./pkg/grpc/interceptor/... ./internal/app/server/... ./internal/infra/server/...` | 0 |
| 批次 H2：`go test -short ./internal/api/serverhttp/...` | 0 |
| 批次 H3：`go test -short ./internal/app/server/...` | 0 |
| 批次 H4：sdk/go + TS npm test | 0 / 0 |
| 批次 H5：`go test -short ./internal/app/server/...` + `task console-build` | 0 / 0 |
| 批次 H6：`go test -short ./internal/infra/auth/... ./internal/api/clientgrpc/... ./internal/api/servergrpc/... ./internal/app/client/...` | 0 |

未跑需要 Postgres/Redis/MinIO/Docker 的集成测试（按纪律只跑 `-short` 与纯单元测试；auth 的 Redis 相关测试走 miniredis）。

## 未做与原因

1. **memberships unique 索引（H5-4）**：适配器把空串存为 `''`（非 SQL NULL），`unique(team_id,user_id)` / `unique(team_id,email)` 会使「多个 user_id 为空的 pending 邀请」或「仅 user_id 邀请（email 为空）」撞车，破坏合法流程。按 fix-plan §5 的 fallback「若做不到，只做应用层并在 fix-report 写明」执行——应用层查重已实现并有测试。reconcile 对存量项目不补索引（已知限制）。
2. **未给 ListAPIKeys/ListFunctions/ListOAuthProviders/ListDeployments/ListExecutions 新做分页**：按 fix-plan §6 明示排除（从未把 ListRequest 传入仓储）。
3. **未跑集成测试**：本地无 Postgres/Redis/MinIO/Docker，纪律要求只跑 `-short`。
4. **未改 proto/、未 generate-proto、未 commit/push/checkout**。
5. **H2/H3/H6 之外的无关重构一律未做**（如 file_handler 仅改 ListBuckets 调用签名）。

## 需原审查方深度审查时关注的点

1. **`Storage.ListBuckets` 签名变更（偏差 A）**：`([]storage.Bucket, int64, string, error)`，波及 `servergrpc/storage.go`（ListBuckets/GetBucket）、`serverhttp/file_handler.go:554` 与 2 个集成测试——纯签名变化，无行为变化。
2. **account-token Lua（偏差 B）**：`cjson.decode` 依赖 Redis 内置 cjson（Redis ≥2.8，默认 7.x 可用）；旧格式记录无 `attempts` 字段按 0 计，向后兼容；锁定语义为「删除记录」，后续正确 secret 亦 Unauthenticated。
3. **`noRefreshMethods` 范围（偏差 C）**：除 account 全部 ACCESS_PUBLIC 外，纳入 client databases 的 `ListDocuments/GetDocument/CountDocuments`（同为 ACCESS_PUBLIC，避免同类缺陷残留）。
4. **`UpdateUser` 新增 `RequireServerWriteActor` 的调用面**：仅 `servergrpc/users.go:118` 调用（经拦截器注入 Principal），无内部裸调路径（已 grep 核实）；`cascade_guards_test.go` 的既有用例已按新契约注入 service actor。
5. **`CreateMembership` 现在对 email 做 normalize 后落库**：新数据大小写统一；存量记录 email 大小写可能不一致，查重只对新建生效。
6. **`adminRoleMethodRules` 与 `apiKeyScopeRules` 对称断言**：若未来新增 ACCESS_API_KEY 方法但漏登角色表，启动期会 panic（fail-closed）；若误把读方法登记进角色表同样 panic。
