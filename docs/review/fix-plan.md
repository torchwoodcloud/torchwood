# Torchwood 修复方案（基于 2026-08-12 全模块代码审查）

> 依据 `docs/review/` 12 份审查报告（01 安全认证 / 02 文档层 / 03 Server API / 04 Client API /
> 05 Account 用例 / 06 Server 用例 / 07 Storage-Functions / 08 CRUD / 09 基础设施 /
> 10 Proto / 11 Console / 12 SDK）汇总。行号以审查时代码为准，修复时以实际代码为准。

## 0. 修复优先级总览

| 批次 | 名称 | P0 | P1 | 建议顺序 | 依赖 |
|------|------|----|----|----------|------|
| F1 | 认证域修复（account/infra-auth） | 1 | 7 | 1 | 无 |
| F2 | 鉴权拦截器与 Server API 提权收口 | 2 | 2 | 1（与 F1 并行） | 无 |
| F3 | 动态文档层修复 | 1 | 3 | 2 | 无 |
| F4 | 级联删除与 Server 用例修复 | 1 | 2 | 2（与 F3 并行） | 无 |
| F5 | Functions 安全修复 | 1 | 5 | 3 | F2（handler 鉴权） |
| F6 | Storage 修复 | 0 | 2 | 3（与 F5 并行） | 无 |
| F7 | 基础设施与引导安全 | 1 | 4 | 4 | 需 generate-config/wire |
| F8 | SDK 与 CLI 修复 | 0 | 5 | 4（与 F7 并行） | 无 |
| F9 | Console 前端修复 | 0 | 3 | 4（与 F7 并行） | 无 |
| F10 | CI 修复（解锁测试） | 0 | 1 | **先于 F5** 执行 | 无 |
| F11 | Proto/OpenAPI 契约修复 | 0 | 3 | 5 | 需 generate-proto |

> ⚠️ **并行执行约定**：各批次文件范围无重叠（见 §7），可在独立 git 分支并行；
> 同一工作区串行执行时按批次顺序逐一提交。F5 依赖 F10 先修复 CI 才能验证。

---

## 1. F1 认证域修复（文件：internal/app/client/*、internal/infra/auth/*、internal/pkg/contexts/*）

### F1-1 🔴 Magic URL 登录 secret 回传响应体（P0，审查 01/04/05）
- 位置：`internal/app/client/magic_url.go:77-87`、`internal/api/clientgrpc/account.go:403-416`
- 方案：`CreateMagicURLSession` 响应只返回**不透明 challengeID**（与 email OTP 一致），
  secret 仅存在于邮件链接 `buildAccountActionURL` 中；clientgrpc handler 原样透传即可
  （需确认 Challenge 结构支持区分 secret 与不透明 ID，或新增字段）。
- 验证：`POST /v1/account/sessions/magic-url` 响应不得包含可登录的 secret；补测试断言。

### F1-2 🟠 account token 校验非原子（P1，审查 01/05）
- 位置：`internal/infra/auth/account_token_redis.go:93-116`（verifyToken 的 GET→DEL）
- 方案：改用 `GETDEL`（参考 `oauth_state_redis.go` / `mfa_challenge_redis.go` 模式），
  校验+删除原子化；补并发双消费测试。

### F1-3 🟠 MFA 登录校验无防重放/锁定（P1，审查 01/05）
- 位置：`internal/infra/auth/totp.go:98-110`（ValidateTOTP）、`internal/app/client/mfa.go:274`
- 方案：`ValidateTOTP` 复用注册路径的 `claimUsedCode`（60s 防重放）与
  `checkFactorLock`/`recordFactorFailure`（15min/5 次锁定）；`CreateMFASession` 加频控。

### F1-4 🟠 TOTP secret 与 JWT 共用主密钥（P1，审查 01/05）
- 位置：`internal/infra/auth/totp.go:42-45,60`、`pkg/secretbox/secretbox.go:16-19`
- 方案：派生独立 purpose key（参考 `pkg/jwtparser.DeriveKey` 已有先例），
  或将派生从裸 SHA-256 改为 HKDF（extract+salt+expand），为 TOTP/OTP/cookie 分离域。
  **注意**：密钥域变更会使存量 TOTP secret 失效——需评估是否双密钥解密窗口（写旧读新）。

### F1-5 🟠 删除 MFA 因子无需二次验证（P1，审查 05）
- 位置：`internal/app/client/mfa.go:200-232`（DeleteFactor）
- 方案：删除 verified 因子前要求有效 TOTP code（或密码）；删除时作废该用户未消费 challenge。

### F1-6 🟠 PATCH /v1/account 改邮箱无需再认证且不撤销会话（P1，审查 05）
- 位置：`internal/app/client/account.go:391-404,436-440`
- 方案：改邮箱要求旧密码（或已过二次验证）；变更后撤销全部会话；新邮箱验证后才生效。

### F1-7 🟠 密码修改/重置后会话残留（>50 条分页截断）（P1，审查 05）
- 位置：`internal/infra/auth/session_service.go:157-170`（DeleteSessionsByUser）、
  `internal/app/client/account.go:449,478-496`
- 方案：删除会话改为循环分页（PageSize=1000 直至 NextPageToken 空）或按 user_id 批量删除；
  补 >50 会话集成测试。

### F1-8 🟠 CreateJWT「一次性」名不副实（P1，审查 05）
- 位置：`internal/app/client/jwt.go:13-54`
- 方案：加随机 jti 并在 Redis 记录一次性消费（Lua GETDEL/SETNX），或在 claims 绑定会话纳入校验。

### F1-9 🟡 补强项（P2 批次，可一并处理）
- SignUp 无频控（`account.go:140-213`）→ 复用 `RateLimiter` 按 IP 限流
- 邮箱无格式/长度校验（`account.go:144`、`email_otp.go:46`、`magic_url.go:39`）→ `net/mail.ParseAddress` + ≤254
- SignIn 时序枚举（`account.go:263-270`）→ 不存在用户时对固定哑哈希执行一次 Verify
- prefs 无大小限制（`account.go:516-535`）→ 限制 64KB/嵌套深度
- 匿名用户无法升级（`anonymous.go:27-62`）→ UpdateAccount 在 password_hash 为空时允许直接设置密码
- 登录节流按邮箱可被定向锁号（`login_throttle_redis.go:30-42`）→ 未注册邮箱失败不计数
- 会话数量无上限（`session_service.go:45-70`）→ 配置化上限，超限淘汰最旧

**验收**：`go test ./internal/app/client/... ./internal/infra/auth/...`（单元测试）
相关集成测试需本地 Redis/Postgres，由 CI 兜底。

---

## 2. F2 鉴权拦截器与 Server API 提权收口（文件：pkg/grpc/interceptor/*、internal/api/serverhttp/functions_handler.go、internal/api/consolegrpc/admins.go、internal/app/console/admins.go）

### F2-1 🔴 API Key 全量 scope 越权 console AdminsService（P0，审查 01/06）
- 位置：`pkg/grpc/interceptor/jwt.go:110-144`、`internal/api/consolegrpc/admins.go:38-80`
- 方案：
  1. `jwt.go` permission 分支（`permissionMethods`）对 `CredentialTypeAPIKey` 凭证直接拒绝
     ——API Key 仅允许经 `apiKeyMethods` 的 scope 门禁调用（console/owner 类权限是 admin 专属）；
  2. 纵深防御：`AdminsService` handler/use-case 增加 `ActorKind == Admin` 守卫；
  3. 补测试：`*`/`all` scope 调 `CreateAdmin/ListAdmins/UpdateAdmin/DeleteAdmin` 必须 PermissionDenied。

### F2-2 🔴 Console 受限 admin（viewer/member）经 Server API 全面提权（P0，审查 06）
- 位置：`pkg/grpc/interceptor/jwt.go:110-137` + `internal/app/server/*` 各用例
- 方案（最小收口，按文件分散，优先两处）：
  1. 拦截器对 admin 会话按角色约束：viewer/member 仅放行 List/Get/Count 类方法
     （复用 `APIKeyScopeAllowed` 的 read 判定或新增 admin 角色规则表）；
  2. 至少禁止非平台 admin 调用：`CreateAPIKey`、`CreateUserToken`、`UpdateUserPassword`、
     `DeleteUser`、schema DDL（databases 全部写方法）、`SetVariables`——
     在 use-case 内校验 `principal.IsPlatformAdmin`（对齐 `Projects.CreateProject` 模式）；
  3. 补集成测试：viewer 调写方法必须 PermissionDenied。

### F2-3 🔴 端用户可上传 Functions 部署代码包（P0，审查 03）
- 位置：`internal/api/serverhttp/functions_handler.go:173-193`（authorize）
- 方案：`authorize()` 增加分支——`CredentialTypeToken/Session` 且 `ActorKind != Admin`
  一律拒绝（PermissionDenied）；补端用户 JWT 上传 deployment 必须 403 的测试。

### F2-4 🟡 纵深防御补强（P2）
- `jwt.go` extractCredential 多凭证并存时拒绝（防凭证混淆，`jwt.go:150-165`）
- HTTP 鉴权三处重复（`file_handler.go:700-767`、`functions_handler.go:173-232`）→ 抽公共 httpAuth 辅助
  （**注意**：抽取公共辅助会改动 file_handler.go，若与 F6 并行需协调，可后置）

**验收**：`go vet ./pkg/grpc/interceptor/... ./internal/api/serverhttp/... ./internal/api/consolegrpc/... ./internal/app/console/...`；
`go test ./pkg/grpc/interceptor/...`（有 miniredis 单测）。

---

## 3. F3 动态文档层修复（文件：internal/infra/documentdb/*、pkg/query/*、internal/app/server/databases.go、internal/app/client/databases.go）

### F3-1 🔴 UpsertDocument TOCTOU 竞态提权（P0，审查 02）
- 位置：`internal/infra/documentdb/postgres.go:481-507,533-540`
- 方案：把「预查 + INSERT ON CONFLICT」包进 `p.db.RunInTx`；预查改为
  `SELECT _id ... FOR UPDATE`（锁住冲突目标行后再查权限，INSERT 同事务执行）；
  或对 `(_tenant, 冲突列值)` 加 `pg_advisory_xact_lock` 串行化。
- 验证：补并发集成测试——victim 与 attacker 并发插入同一冲突值，断言 attacker 无法改写。

### F3-2 🟠 ListDocuments page_size 参数失效（P1，审查 02/08）
- 位置：`pkg/query/query.go:205-207`（ParseMany 恒注入默认 50）、`postgres.go:729-735`
- 方案：`ParseMany` 不注入默认 limit（默认值交由 adapter 决定），
  ListDocuments 改为「DSL 未显式指定 limit 时用 q.PageSize」并保留上限 clamp；
  重写被掩盖的 `TestListDocuments_PaginationGuards`（原断言因恒 50 恰好通过）。

### F3-3 🟠 CreateDocument 尾随读回半完成状态（P1，审查 02）
- 位置：`internal/app/server/databases.go:333-336`、`internal/app/client/databases.go:140-143`
- 方案：删除 app 层冗余 principal 重读，直接返回 adapter 的 `created`
  （adapter 已用 SystemPrincipal 读回）；或容忍尾随读回 PermissionDenied。

### F3-4 🟠 文档写入与 _perms 非原子（P1，审查 02）
- 位置：`postgres.go:424-432`（Create）、`:618-635`（Update）、`:664-667`（Delete）
- 方案：数据语句与 setPermissions/clearPermissions 包进 `p.db.RunInTx`
  （参考 Bulk 已有先例，`clients.InTx` 嵌套防护可复用）。

### F3-5 🟡 DDL 与元数据非事务（P2，审查 02/06）
- 位置：`postgres.go:334-352`（CreateAttribute）、`:361-375`（CreateIndex）、
  `:244-252`（DeleteAttribute）、`:278-286`（DeleteCollection）、`:166-175`（CreateCollection）
- 方案：DDL + 元数据写入包进同一 `RunInTx`（PG 支持事务内 DDL）。

### F3-6 🟡 错误分类与校验补强（P2）
- `docdbErrorSQLStates` 补 42P10（无唯一索引的 ON CONFLICT）、23505（元数据重复键）
  （`internal/app/shared/docdb_errors.go:19-31`）
- UpdateDocument 目标不存在 → ErrDocumentNotFound（映射 NotFound）（`postgres.go:639-645`）
- CreateDocument 补 `validateDocID`（`postgres.go:378-385`）
- SumDocumentField 字段白名单 + 类型校验（`postgres.go:914-953`）
- contains/startsWith/endsWith 的 `%`/`_` 转义 + `ESCAPE '\'`（`postgres.go:1722-1730`）
- DecodePageToken 失败显式报 InvalidArgument（`postgres.go:740-744`）
- 权限检查路径 N+1（`postgres_permissions.go:66-68`）→ 复用调用方已取的 coll

**验收**：`go vet ./internal/infra/documentdb/... ./pkg/query/...`；`go test ./pkg/query/...`；
documentdb 集成测试由 CI（需 F10）兜底。

---

## 4. F4 级联删除与 Server 用例修复（文件：internal/app/server/users.go、teams.go、projects.go、internal/infra/documentdb/postgres.go 的 DeleteDatabase/DeleteCollection）

### F4-1 🔴 用户/团队级联删除被 50 条截断（P0，审查 06/02）
- 位置：`internal/app/server/users.go:287-326`（deleteUserCascade）、`internal/app/server/teams.go:455-463`
- 方案：级联 ListDocuments 设 `PageSize: 1000` 并循环直至 `NextPageToken` 为空；
  `DeleteUser` 的 sessions/identities/memberships 三集合、`DeleteTeam` 的 memberships 均同处理。
- 验证：补 >50 会话/成员集成测试。

### F4-2 🟠 DeleteDatabase/DeleteCollection 不清理元数据（P1，审查 06/02）
- 位置：`postgres.go:143-154`（DeleteDatabase）、`:272-287`（DeleteCollection）
- 方案：删除物理对象时同步删除 `document_collections`/`document_attributes`/
  `document_indexes` 对应行（按 project_id/database_id/collection_id 过滤，同一事务/顺序）。
- 验证：删库/删集合后重建同名资源必须成功。

### F4-3 🟠 团队 last-owner 保护缺失（P1，审查 05/06）
- 位置：`internal/app/server/teams.go:280-300,357-368`、`internal/app/client/teams.go:140-161`
- 方案：删除/降级 owner membership 前统计 accepted 且含 owner 角色的 membership 数，
  ≤1 时拒绝（FailedPrecondition），client 自退路径同样校验。

### F4-4 🟠 UpdateUser 改邮箱不查重（P1，审查 03/06）
- 位置：`internal/app/server/users.go:141-153`
- 方案：email 分支先按新邮箱查重（排除自身 userID），重复返回 AlreadyExists；
  并发兜底走 `MapDocumentDBError` 的 23505 → AlreadyExists。

### F4-5 🟠 GetProject 返回 nil,nil（P1，审查 03）
- 位置：`internal/api/servergrpc/projects.go:67-71`
- 方案：`p == nil` 时返回 `codes.NotFound`（对齐 users.go:87-89）。

### F4-6 🟡 补强项（P2）
- DeleteUser/DeleteTeam 级联整体包入事务（`users.go:272-282`）
- CreateUserToken 增加审计标记 + 生命周期限制（`users.go:249-270`）
- admins use-case 角色下沉（`app/console/admins.go`——与 F2-2 协调，F2 改过该文件时此处跳过）
- DeleteBucket 孤儿文件元数据（见 F6，避免重复）

**验收**：`go vet ./internal/app/server/...`；集成测试由 CI 兜底。

---

## 5. F5 Functions 安全修复（文件：internal/app/functions/*、internal/infra/functions/docker.go、internal/infra/bun/bunrepo/function_repo.go、pkg/idgen/id.go）

### F5-1 🔴 Function ID 路径穿越 → 任意文件写入（P0，审查 07）
- 位置：`internal/app/functions/management.go:47-49`、`pkg/idgen/id.go:20-22`（IsValid 仅判非空）、
  `internal/app/functions/deployments.go:144-157`（zipPath/writeZip/removeZip）
- 方案：
  1. `CreateFunction` 对 ID 做字符集+长度校验（如 `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`）；
  2. 纵深防御：zipPath 中 functionID 用 `filepath.Base` 或哈希后再拼接；
  3. writeZip/removeZip 落盘前断言 `filepath.Dir(path)` 仍在 `os.TempDir()/torchwood-functions` 前缀内。
- 验证：补 `../../..` 等恶意 ID 的拒绝测试。

### F5-2 🟠 GetDeployment/DeleteDeployment 跨项目 IDOR（P1，审查 07）
- 位置：`management.go:114-141`、`function_repo.go:80-93,118-124`、`internal/domain/functions/repo.go:17,20`
- 方案：前置 `GetFunction(projectID, functionID)` 校验；repo 端口签名加 projectID，
  SQL 加 `fd.project_id = ?`（改动端口需同步 wire 无需，但更新接口与调用方）。

### F5-3 🟠 Docker 解压 0600 + USER 非 root → EACCES（P1，审查 07）
- 位置：`docker.go:328`（0o600 写入）、`:365-375`（USER node）
- 方案：解压写入改 0o644（或 `os.Chmod(target, 0o644)`）；并先修 F10 让 CI 真跑
  `TestDockerExecutor_BuildAndRunNode` 验证。

### F5-4 🟠 GetVariables 明文返回全部 secret（P1，审查 06/07）
- 位置：`internal/app/functions/variables.go:30-35`、`servergrpc/functions.go:237-247`
- 方案：Get 返回脱敏（空串或掩码）；值仅在 SetVariables 请求/响应中可见一次；
  建议变量列加密存储（可后置）。

### F5-5 🟠 docker build 失败被吞 + 构建日志丢弃（P1，审查 07）
- 位置：`docker.go:149-157`（`io.Copy(io.Discard, resp.Body)`）
- 方案：LimitReader(64KB+1) 读入并保存，扫描流内 `"error"` JSON；失败返回带日志的错误；
  由 buildDeployment 截断写入 dep.Error。

### F5-6 🟠 TW_DATA 64KB 超 execve 32KiB 硬限制（P1，审查 07）
- 位置：`internal/app/functions/executions.go:20`、`docker.go:185`
- 方案：`maxExecutionDataBytes` 收紧至 32KB 且与 env 合并预算；或校验 `len(data)+envSize ≤ 32KB`。

### F5-7 🟡 补强项（P2）
- SetVariables 校验 function 存在（`variables.go:12-28`）→ NotFound
- worker 补构建无超时（`executions.go:254`）→ `context.WithTimeout(5min)`
- buildDeployment 信号量满泄漏 pending（`deployments.go:61-80`）→ 删除行与 zip / 对账
- ensureNetwork sync.Once 粘住瞬时错误（`docker.go:96-112`）→ 失败不缓存
- CreateFunction ID 字符集同时解决镜像名非法（P3-11）
- worker 消费失败丢任务（`worker.go:120-125`）→ 重抛回队或标 failed
- 孤儿对账状态倒挂（`function_repo.go:217-234`）→ 对账仅处理 building/running

**验收**：`go vet ./internal/app/functions/... ./internal/infra/functions/... ./cmd/worker/... ./pkg/idgen/...`；
docker 集成测试需 F10 修复后的 CI。

---

## 6. F6 Storage 修复（文件：internal/app/storage/*、internal/infra/storage/*、internal/api/serverhttp/file_handler.go）

### F6-1 🟠 complete 互斥锁 TTL 短于长 Compose（P1，审查 07）
- 位置：`internal/infra/storage/redis_upload_session.go:24`、`internal/app/storage/uploads.go:166-199`
- 方案：锁 TTL 提至 1h 或引入续期；回滚删对象前确认「自己仍是锁持有者 + 会话存在」。

### F6-2 🟠 Preview 解码无像素级防线（P1，审查 03/07）
- 位置：`file_handler.go:571,624-635`
- 方案：解码前用 `image.DecodeConfig` 读取宽高，超过上限（如 8192 边长）直接拒绝；
  输出改流式编码。

### F6-3 🟡 补强项（P2）
- DeleteBucket 不删 files 元数据（`app/storage/storage.go:150-184`）→ 按 bucket_id 级联删文档
- UploadChunk 缺 EnsureBucket（`uploads.go:138-141`）→ CreateUploadSession/UploadChunk 补
- 默认 bucket 名大小写不一致（`storage.go:484-490` vs `minio.go:45-49`）→ 统一小写 + 单常量
- upload session 无 owner 绑定（`uploads.go:111-146`）→ 增加 OwnerUserID 校验
- file token 与 JWT 共用密钥（`storage.go:414,427`）→ 独立 purpose key（与 F1-4 同模式）
- 私有文件下载无 Cache-Control（`file_handler.go:497-507`）→ `private, no-store`
- 公开 bucket 匿名路径 bucketID 拼 DSL 未转义（`file_handler.go:538-541`）→ 参数化/预校验 UUID

**验收**：`go vet ./internal/app/storage/... ./internal/infra/storage/... ./internal/api/serverhttp/...`；
集成测试需本地 MinIO（CI 兜底）。

---

## 7. F7 基础设施与引导安全（文件：internal/pkg/config/config.proto + bind.go、cmd/server/*、internal/infra/server/*、internal/infra/clients/*、internal/infra/health/*、internal/app/console/setup.go）

> ⚠️ 本批次改 config.proto，需执行 `task generate-config` 与 `task wire-all`（依赖变更时）。

### F7-1 🔴 Console 首个管理员引导可被抢占（P0，审查 06/09）
- 位置：`internal/app/console/setup.go:98-107`、`proto/console/v1/auth.proto`（SignUp PUBLIC）
- 方案：
  1. 新增 `security.setup_token` 配置（env `TORCHWOOD_SECURITY_SETUP_TOKEN`），
     未设置时 SignUp 拒绝（403/404）；SignUp 时校验请求携带的 setup token；
  2. 并发兜底：`pg_advisory_xact_lock` 串行化首次性检查（或 admins 表哨兵行唯一约束）。
- 注意：Console 前端登录页需要能输入 setup token（`console/src/routes/Login.tsx` 相关改动可选后置，
  可先用 API 手动调用验证）。

### F7-2 🟠 graceful shutdown 顺序错误（P1，审查 09）
- 位置：`cmd/server/main.go:26-29`（OnStop(cleanup)）
- 方案：cleanup 移出 OnStop，在 `runner.Run()` 返回后调用；建议启用 WithDrainTimeout。

### F7-3 🟠 慢查询/调试 SQL 记录内联参数（P1，审查 09）
- 位置：`internal/infra/clients/dbhook.go:54-74`、`configs/config.yaml.template:33`（debug: true）
- 方案：日志只输出操作名+表名+占位符（或 Args 单独字段默认不落盘）；
  password_hash/secret/token 列强制掩码；模板 debug 默认 false。

### F7-4 🟠 Prometheus metrics 无鉴权且默认监听全部接口（P1，审查 09）
- 位置：`internal/infra/server/metrics.go:16-24`、`configs/config.yaml.template:17`（`:9040`）
- 方案：默认改 `127.0.0.1:9040`；或加 scoped token 中间件。

### F7-5 🟠 JWT 弱默认被启动校验接受（P1，审查 09）
- 位置：`cmd/server/provides.go:48-50`、`.env.example:8`
- 方案：拒绝已知弱值（`change-me-in-production` 等）与 <32 字符；启动告警。

### F7-6 🟡 补强项（P2）
- HTTP 侧补 panic recovery（`internal/infra/server/grpc_gateway.go:100-108`）
- 启动 ping 加超时（`clients/database.go:37,89`）
- health 检查结果缓存（`infra/health/checks.go:78-90`）
- idgen：每次生成打项目查询 + DB 抖动静默回退（`infra/idgen/service.go:111-136`）
- random 策略 Redis 集合无界（`idgen/random_redis.go:16-31`）
- 审计落库无超时 + 错误静默（`pkg/grpc/interceptor/audit.go:60-62`）——与 F2 协调避免同文件
- 连接池零值陷阱（`clients/database.go:73-81`）

**验收**：`task generate-config`、`task wire-all` 通过；`go vet ./cmd/... ./internal/...`；
`go test ./internal/pkg/config/... ./internal/pkg/database/...`。

---

## 8. F8 SDK 与 CLI 修复（文件：sdk/typescript/src/*、sdk/go/**、cmd/client/cmd/*、sdk/demo/src/*）

### F8-1 🟠 TS SDK labels 编码错误 + int64 类型与运行时不符 + MFA 分支崩溃（P1，审查 12/10）
- 位置：`sdk/typescript/src/server/users.ts:19,51`、`src/types.ts:95`、`src/client/account.ts:25,45`
- 方案：
  1. `labels` 改为 `Record<string, unknown>` 直接透传（删除 `{values:...}` 包装）；
  2. int64 字段（count/affected/size/expires_at）类型改为 `string | number` 或统一 string；
  3. signIn/signUp 增加 `if (res.mfa_required) return res` 分支，类型补 mfa_required/challenge_token。

### F8-2 🟠 TS deleteSessions keepCurrent 无法传递（P1，审查 12）
- 位置：`src/client/account.ts:92-96`；根因 `proto/client/v1/account.proto:56-57`（DELETE 无 body）
- 方案：proto 给 DeleteSessions 加 `body: "*"`（或改 query 绑定）并重新生成；
  **需与 F11 协调 generate-proto**（若 F11 后置，可先由本批次直接修改 proto + 重新生成）。

### F8-3 🟠 Web demo 构建被破坏（P1，审查 12）
- 位置：`sdk/demo/src/lib/` 的 context 文件（当时文件名仍沿用项目旧名）vs 10 处 import `@/lib/torchwood-context`
- 方案：重命名文件为 `torchwood-context.tsx`；跑 `task sdk-demo-build` 验证。

### F8-4 🟡 补强项（P2）
- Go SDK 补 8 个缺失类型化方法（UpdateUserPassword、DeleteTeam、GetTeamPrefs/UpdateTeamPrefs、
  UpdateCollection/DeleteCollection/DeleteAttribute/DeleteIndex）+ bufconn 测试
- gRPC 客户端默认 4MiB 接收上限 → 8MiB（`sdk/go/internal/conn/conn.go:17-18`）
- CLI deployments create 上限与 help 文案改 8MiB/1MiB（`cmd/client/cmd/functions.go:225,432-434`）
- InvokeJSON 完整性测试硬编码清单 → 遍历 protoregistry（`sdk/go/server/invoke_test.go:49-59`）
- TS projects.create 移除不存在的 `id` 字段 + 补 description（`src/server/projects.ts:19-24`）
- TS Storage 补 public/metadata（`src/server/storage.ts:7-12`、`src/types.ts:156-172`）
- FileTokenStore 展开 `~` + MkdirAll（`sdk/go/client/token.go:59`）
- TS SDK 补最小测试集（labels 编码、错误解析、MFA 分支）

**验收**：`go test ./sdk/go/... ./cmd/client/...`；`npx tsc --noEmit`（sdk/typescript）；
`task sdk-demo-build`。

---

## 9. F9 Console 前端修复（文件：console/src/**）

### F9-1 🟠 分片上传续传 key 碰撞 → 文件损坏（P1，审查 11）
- 位置：`console/src/routes/storage/chunked-uploader.tsx:27-28`
- 方案：key 加入 `file.lastModified`；复用 session 前校验 size 一致，不一致重建并清旧 key。

### F9-2 🟠 登出失败无兜底（P1，审查 11）
- 位置：`console/src/hooks/useAuth.tsx:63-70`
- 方案：await 登出请求；失败 toast 警告并引导重试；成功时 `queryClient.clear()`。

### F9-3 🟡 补强项（P2）
- 跨项目 Query Cache 污染：`functions/pages.tsx:106-109`、`storage/pages.tsx:224-227` key 加 projectId
- 保存后表单与服务端失同步（`databases/pages.tsx:1567-1580`）→ 成功后用响应重建 values
- 批量更新缺 MAX_BULK_OPERATIONS 预检（`databases/pages.tsx:1226-1244`）
- 无效路由参数白屏（`databases/pages.tsx:997-998,1403-1404`）→ NotFound
- 分片上传无重试/取消（`chunked-uploader.tsx:75-108`）→ 单分片重试 + abort 清理
- 前端硬编码分片常量（`api/storage.ts:98-99`）→ 使用 session.chunk_size
- 双 toast（页面 onError 与拦截器重复）→ 统一由拦截器处理
- Promise.all 批量删除无失败处理（`storage/pages.tsx:124-134`）→ allSettled
- 角色权限 UI gating（viewer 隐藏写按钮）
- 路由级 React.lazy + ErrorBoundary

**验收**：`npx tsc --noEmit`（console/）；`task console-build`。

---

## 10. F10 CI 修复（文件：.github/workflows/ci.yml）

### F10-1 🟠 CI backend job 必失败于 minio 健康检查（P1，审查 07）
- 位置：`.github/workflows/ci.yml:38`（`curl -f http://localhost:9000/minio/health/live`）
- 方案：minio 镜像无 curl → 改用 TCP 探测（bash /dev/tcp 或 busybox wget）。
- 验证：push 后确认 backend job 全绿；确认 `TestDockerExecutor_BuildAndRunNode` 真实执行
  （若 Docker-in-Docker 不可用则跳过并记录）。

> ⚠️ **本批次应在 F5 之前完成**，否则 F5-3 无法验证。

---

## 11. F11 Proto/OpenAPI 契约修复（文件：proto/**、buf.gen.yaml、internal/infra/server/grpc.go、sdk/typescript（契约测试））

> ⚠️ 需 `task generate-proto` 重新生成 genproto；改动面大，建议最后批次、独立分支。

### F11-1 🟠 OpenAPI 产物无认证元数据（P1，审查 10）
- 方案：`buf.gen.yaml` openapiv2 插件输出稳定 JSON；proto 引入
  `openapiv2_swagger` options 声明 securityDefinitions（apiKey X-API-Key / Bearer / cookie），
  并把 method_auth 透传到 operation 级（自定义 extension 或文档映射）。

### F11-2 🟠 TS SDK 与 proto 脱节（P1，审查 10/12）
- 方案：补齐 `sdk/typescript/src/server/functions.ts`（16 RPC）与 account 缺失 16 方法；
  建立 CI 契约测试（proto RPC 集合 vs SDK 方法集合比对）。

### F11-3 🟠 REST 保留字路径段遮蔽资源 id（P1，审查 10）
- 位置：`proto/server/v1/databases.proto:93-102`、`proto/server/v1/functions.proto:20-23`
- 方案：改自定义方法风格（`:count`、`:bulkUpdate`）或 Create 时校验保留字 id；
  重新生成后核对 gateway 路由。

### F11-4 🟡 补强项（P2）
- 101/143 方法补方法级 method_auth（至少敏感方法：SetVariables/GetVariables/CreateFileToken/CreateUserToken/APIKeys）
- API key scope 映射从 Go 硬编码改为由注解推导（或启动期一致性断言）
- error.proto 映射补齐（Aborted→CONCURRENT_MODIFICATION、ResourceExhausted→QUOTA_EXCEEDED、DeadlineExceeded→TIMEOUT）
- 时间戳统一 Timestamp；更新类请求补 optional（清空语义）
- buf lint/breaking 接入 CI；删除字段一律 reserved
- 敏感字段（secret/token/client_secret）注释补「仅一次返回」

**验收**：`task generate-proto` 后 `go build ./...`；`buf lint` 通过（按项目风格配置）；
CI 契约测试绿。

---

## 12. 交叉依赖与文件冲突矩阵

| 文件 | 批次 | 冲突风险 |
|------|------|----------|
| pkg/grpc/interceptor/jwt.go | F2 | 唯一 |
| internal/api/serverhttp/functions_handler.go | F2 | 唯一 |
| internal/api/serverhttp/file_handler.go | F6（F2-4 后置可避免） | F2-4 若抽取公共 auth 会触碰 |
| internal/infra/documentdb/postgres.go | F3、F4 | F4-2 与 F3-5 同文件 → **F4-2 由 F3 一并处理**，F4 跳过 |
| internal/app/console/admins.go | F2、F4-6 | F4-6 跳过（F2 处理） |
| pkg/grpc/interceptor/audit.go | F2、F7-6 | F7-6 跳过（F2 处理） |
| proto/client/v1/account.proto | F8-2、F11 | F8-2 若改 proto 需 generate-proto |
| internal/pkg/config/config.proto | F7 | 需 generate-config + wire-all |

> 修正后批次归属：**F4 移除 F4-2（并入 F3-5）与 F4-6（并入 F2）**；
> 级联删除（F4-1）仍属 F4（users.go/teams.go 无冲突）。

## 13. 回归验证清单（全部批次完成后）

1. `task generate-all`（proto/config/wire）无 diff 异常
2. `task test`（需本地基础设施）——修复后应比审查前少失败（新增测试全绿）
3. `task lint`、`task build`、`task console-build`
4. 手工安全冒烟：
   - `*`/`all` scope API Key 调 console Admins API → PermissionDenied
   - Magic URL 响应不含 secret；并发双消费同 token 只成功一次
   - viewer 角色调 CreateAPIKey/CreateUserToken → PermissionDenied
   - 恶意 function ID（`../../x`）→ InvalidArgument
   - 并发 upsert 同冲突值 → 攻击者无法改写他人行
   - 未设置 setup token 时 SignUp → 拒绝
5. CI 全绿（含 docker 集成测试）
