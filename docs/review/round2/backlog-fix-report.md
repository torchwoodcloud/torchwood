# Round-2 Backlog 修复报告（B1/B2/B3）

> 执行日期：2026-08-12 ｜ 依据：`docs/review/round2/backlog-next-round.md`
> 执行方式：总控分派子代理实现（B1 与 B3 因同改 proto 串行执行，B2 并行独立），
> 每个子代理交付后总控亲自审查 diff 的真实性与最小性再合流；B1 子代理中途中断，
> 剩余实现由总控亲自补齐并审查。
> 仓库基线 `941ba14`；所有改动 commit 于本批次。
>
> **本批收尾**（总控分派两个子代理并行：Task 1 = Client API 保留字路由同步迁移，
> Task 2 = 历史保留字 id 指引修正）：commit `637a922`，见 §3a 与 §6 遗留项 2/3。

---

## 0. 结果总览

| 项 | 状态 | 要点 |
|----|------|------|
| B1 邮箱变更完整 staging（G3-2 A 档） | ✅ | `UpdateAccountRequest.url=5` + 新 RPC `ConfirmEmailChange`；pending_email 暂存，确认后才切换 |
| B2 worker 重试计数持久化（G6 缓修项） | ✅ | payload 内嵌 attempt，队列消息即唯一事实来源，跨重启/多副本正确 |
| B3 REST 保留字自定义动词迁移（R10-P1-3） | ✅ | `:count`/`:bulkUpdate`/`:bulkDelete`/`:runtimes`/`:specifications`；服务端保留字校验移除 |
| B3-followup Client API 同步迁移（本批收尾） | ✅ | Client `documents/count`→`:count`；clientDocumentIDReserved 移除；历史保留字 id 指引修正（无需清理，附检测 SQL） |
| 本地验证 | ✅ | generate-proto / buf lint / build / vet / test -short / TS SDK 16 用例 / console-build / task build 全绿 |
| CI | 见 §7 | push 后监视至 Backend+Frontend 全绿 |

---

## 1. B1 邮箱变更完整 staging（P1，R05-P1-2）✅

### 现状→目标

改邮箱不再「未验证即生效」：新邮箱验证通过前 `email` 保持旧值（旧邮箱仍可登录/找回），
写入 `pending_email` 暂存；`ConfirmEmailChange` 消费一次性 token 后才切换。

### 关键决策（供产品复核）

1. **契约**：`UpdateAccountRequest` 增加 `string url = 5;`（改邮箱时必填，语义同
   `CreateVerificationRequest.url`）；新增 RPC `ConfirmEmailChange(ConfirmEmailChangeRequest)
   returns (Account)`，REST `PUT /v1/account/email-change`（body `*`），请求
   `{project_id, user_id, secret}` 与 `UpdateVerification` 同形。
2. **ConfirmEmailChange 免登录（ACCESS_PUBLIC，已按产品决策落地）**：初始实现为
   USER 级（需登录态确认），产品复核后改为免登录——点邮件链接即完成，与 recovery
   同一安全模型（256-bit 随机 secret + 24h TTL + GETDEL 一次性消费）。use-case 不再
   校验 principal/user_id 归属，token 本身是唯一凭证。
3. **url 校验复用既有白名单**：`validateRedirectURL` + `validateProjectOAuthRedirectURLs`
   （与 CreateVerification/CreateMagicURLSession 相同语义）。
4. **撤会话时机**：staging 阶段不撤（旧邮箱仍可登录）；`ConfirmEmailChange` 成功时
   「先撤会话后提交」（保持 G3-3 语义，撤失败即返回无半提交）。
5. **tokens/mailer 未配置**：改邮箱返回 `Unimplemented`（staging 无法完成时拒绝而非静默）。
6. token 机制复用 account-token：新 purpose `email_change`，GETDEL 原子消费，消费时返回
   record 中的新邮箱；旧邮箱安全通知（B 档成果）保留，通知文案改为「pending change +
   撤销指引」。

### 实现要点

- `internal/domain/auth/account_token.go`：`AccountTokenPurposeEmailChange` +
  `CreateEmailChangeToken` / `VerifyEmailChangeToken`（返回新邮箱）。
- `internal/infra/auth/account_token_redis.go`：实现 + `verifyTokenWithEmail` 内部方法
  （`verifyToken` 改由它薄封装，行为不变）。
- `internal/app/client/account.go`：`UpdateAccountCommand.URL`；UpdateAccount 改 staging
  （URL 必填校验 → 查重 → 签发 token + 发新邮箱验证邮件 → 提交 `pending_email` → 旧邮箱
  通知）；新增 `ConfirmEmailChange` use-case（principal 校验 → GETDEL 消费 → 查重 →
  撤会话 → 写 `email`/清 `pending_email`/`email_verified=true`）。
- `internal/api/clientgrpc/account.go`：`UpdateAccount` 透传 `url`；新增 `ConfirmEmailChange`
  handler 薄透传。
- SDK：`sdk/go` `UpdateAccount` 加 `url` 参数 + 新方法 `ConfirmEmailChange`（fake 与测试同步）；
  `sdk/typescript` `updateAccount` 加 `url?` + 新方法 `confirmEmailChange`（contract 映射与
  HTTP 绑定用例同步）。

### 测试

- 集成（CI 跑，`-short` 跳过）：未验证前旧邮箱可登录/新邮箱不可 → 确认后反转
  （`TestAccount_EmailChangeStaging_OldEmailWorksUntilConfirm`）；token 一次性二次使用
  Unauthenticated（`TestAccount_ConfirmEmailChange_TokenOneTime`）；新邮箱被占用
  AlreadyExists（`TestAccount_ConfirmEmailChange_NewEmailTaken`）；确认路径撤会话失败邮箱
  不变（`TestAccount_ConfirmEmailChange_SessionRevocationFailureLeavesOldEmail`）；
  user_id≠principal PermissionDenied；改邮箱缺 url InvalidArgument；B 档旧邮箱通知保留
  （文案更新）。
- 单元（本地跑）：purpose 隔离（email_change ↔ verification 互不通用）与一次性消费
  （`TestRedisAccountTokenStore_EmailChange`，miniredis）；handler 端到端
  （`TestAccountService_ConfirmEmailChange_Passthrough`，内存 fakeDocDB + miniredis，
  `-short` 真实执行）。

---

## 2. B2 worker 重试计数持久化（P3，R07-P3-8）✅

### 现状→目标

删除进程内存 `map[string]int` 计数（重启清零），改为**队列 payload 内嵌 attempt 计数**：
每次瞬时失败重抛回队前 `attempt+1`，超限（> `maxProcessAttempts`=3）走 `failPayload`
兜底标 failed。队列消息是唯一事实来源，worker 重启/多副本天然正确。

### 关键决策

1. **方案 (a) payload 内嵌**（任务书推荐；未选 (b) ExecutionRecord 加列——避免 schema
   迁移且与 (a) 不双写）。
2. `requeue` 改为包级函数 `requeue(payload) (next []byte, ok bool)`：worker 侧 `retryMessage`
   扩展为完整往返结构（`execution_id/function_id/project_id/data/attempt`，与 functions 包
   `queueMessage` json 字段一致，JSON 往返无损）；旧消息无 attempt 字段视为 0，首次重试
   即 attempt=1。
3. 超限语义与旧实现逐位一致：attempt=3 时再次失败（第 4 次）才超限。
4. 防御分支：payload 解析失败视为超限不重试（正常不可达，防坏消息无限重试）。

### 实现要点

- `internal/app/functions/executions.go`：`queueMessage` 加 `Attempt` 字段 + 注释。
- `cmd/worker/worker.go`：移除 attemptMu/attempts；`requeue` 重写；旧「已知限制」注释
  更新为新机制说明。
- 测试：`cmd/worker/requeue_test.go`（递增/旧格式兼容/往返保留 data/超限/非法 payload）；
  `cmd/worker/consume_test.go`（stub FunctionRepo+Queue 驱动真实 consume 循环：enqueued
  恰 3 条 attempt=[1,2,3]、第 4 次失败后 MarkExecutionFailed 标 failed 且 error 含
  "worker retries exhausted"；旧格式首重试 attempt=1）。`-race` 下也绿。

---

## 3. B3 REST 保留字自定义动词迁移（P1，R10-P1-3，breaking change）✅

### 现状→目标

字面量路由与 `{id}` 通配路由冲突根除：`documents/count`→`documents:count`、
`documents/bulk`→`documents:bulkUpdate`、`documents/bulk/delete`→`documents:bulkDelete`、
`functions/runtimes`→`functions:runtimes`、`functions/specifications`→`functions:specifications`；
服务端创建校验兜底（`documentIDReserved`/`functionIDReserved`）移除，
`count/bulk/runtimes/specifications` 成为合法 id。

### 关键决策（供产品复核）

1. **不保留旧字面量路由（直接 breaking + 升级说明）**：按任务书推荐执行——双路由正是
   要根除的冲突；旧路径请求一律 404，升级声明已入 roadmap/09-api-guide/12-sdk。
2. **Client API（`proto/client/v1/databases.proto`）未迁移**：本任务书范围仅 Server API；
   Client 侧 `documents/count` 字面量路由仍在，`clientDocumentIDReserved`（仅 `count`）与
   校验保持不动。如需后续统一为自定义动词请另立任务。
3. 函数侧保留字集成测试降级为纯单测：`internal/app/functions` 包无真实 DB fixture 且
   禁止引入新依赖，用 mockRepo 覆盖「创建+回读」；文档侧集成测试放 server 包
   （`TestDatabases_ReservedIDDocumentCRUD`，复用 SetupTestDB，CI 跑）。
4. 升级指引落点：roadmap §2.4 backlog 备注（已迁移 + 历史保留字 id 数据先重命名/删除）、
   09-api-guide §1.3、12-sdk §5.3。

### 实现要点

- proto 5 处绑定迁移 + 保留字注释改为自定义动词说明；`task generate-proto` 产物仅限
  server/v1/databases.* 与 functions.*（buf lint 通过）。
- `internal/app/server/databases.go`、`internal/app/functions/management.go`：删除保留字
  拒绝逻辑。
- TS SDK `src/server/{databases,functions}.ts` 5 处路径；Console `src/api/{databases,functions}.ts`
  4 处路径；contract.test.ts 方法名不变自动比对新 swagger 路径（16/16 绿）。
- `cmd/client` grep 确认无硬编码旧路径（走 sdk/go gRPC stub）。
- 测试：`databases_reserved_test.go` 重写为「保留字 id 可正常创建并 CRUD」（fake + 集成）；
  `security_test.go` runtimes/specifications 移入合法清单 + 新增创建回读单测。

### 3a. Client API 同步迁移（本批收尾）✅

B3 仅覆盖 Server API；本批按 B3 决策将 Client API 一并迁移（breaking change），
遗留项 2/3 关闭（见 §6）。

### 关键决策（供产品复核）

1. **与 Server API 完全对齐**：Client `CountDocuments` 路由
   `get: "/v1/databases/{database_id}/collections/{collection_id}/documents/count"` →
   `".../documents:count"`；`clientDocumentIDReserved`（仅 `count`）与 CreateDocument
   拒绝分支一并移除，`document_id="count"` 在 Client API 成为合法 id。不保留旧字面量
   路由（与 B3 决策 1 一致，旧路径 404）。
2. **历史保留字 id 无需数据清理**：路由冲突根除后，历史 `count`/`bulk`/`runtimes`/
   `specifications` id 资源经 REST 自动恢复可访问；roadmap §2.4 升级指引由「升级前
   重命名/删除」修正为「自动恢复、无需处理」，并附可选检测 SQL（functions 静态表 +
   动态文档层按 collection 表，注明表命名规则与替换提示）。

### 实现要点

- `proto/client/v1/databases.proto`：路由改 `:count`；CreateDocument 与
  CreateDocumentRequest.document_id 的保留字注释改为自定义动词说明（措辞对齐
  proto/server/v1/databases.proto:115-117/:288-290）。`task generate-proto` 产物仅
  client/v1/databases.*（buf lint 通过）。
- `internal/app/client/databases.go`：删除 `clientDocumentIDReserved` 变量与
  CreateDocument 的保留字拒绝分支（净 -7 行）。
- 测试：`internal/app/client/databases_reserved_test.go` 新增
  `TestClientDatabases_ReservedIDDocumentCRUD`（真实 PG 集成，与
  `TestDatabases_ReservedIDDocumentCRUD` 同构：SignUp 用户 + 建库建集合 +
  `document_id="count"` 创建/Get/Update/Delete 全成功，删后 Get NotFound）。
- TS SDK：`sdk/typescript/src/client/databases.ts` countDocuments 路径改 `:count`
  （方法名签名不变）；contract.test.ts 无需改动（CountDocuments 映射已存在且
   HTTP 绑定用例不含该路径，swagger 自动比对仍 16/16 绿）。
- 文档：09-api-guide §1.3 与 12-sdk §5.3 breaking 块补 Client API 同步迁移声明；
  roadmap §2.4 backlog 备注更新（Client 迁移 + 自动恢复 + 检测 SQL，由 Task 2 子代理
  与总控合流完成）。
- 复核：`rg "documents/count|/v1/databases" console/src sdk/go cmd/client` 零匹配
  （console 走 /server/ 前缀，gRPC stub 无路径）。

### 本地验证（本批，全部真实执行）

| 命令 | 结果 |
|------|------|
| `task generate-proto`（buf lint + generate，二次运行幂等） | ✅ exit 0 |
| `gofmt -l .` | ✅ 无输出 |
| `go vet ./...` / `go build ./...` | ✅ exit 0 |
| `go test -short -count=1 ./...` | ✅ 52 包全绿 |
| `go test -count=1 -run 'TestClientDatabases_ReservedIDDocumentCRUD\|TestDatabases_ReservedIDDocumentCRUD' ...`（真实 PG） | ✅ 双 PASS（1.06s / 1.13s） |
| sdk/typescript `npx tsc --noEmit` + `npm run test` | ✅ 16/16 |

---

## 4. 改动文件清单

### B1（19 文件）

| 文件 | 改动 |
|------|------|
| proto/client/v1/account.proto | `url=5` + `ConfirmEmailChange` RPC + request message |
| genproto/client/v1/account.{pb,gw,swagger,grpc}.* | 重新生成 |
| internal/domain/auth/account_token.go | email_change purpose + 2 接口方法 |
| internal/infra/auth/account_token_redis.go | 实现 + verifyTokenWithEmail 重构 |
| internal/infra/auth/account_token_redis_test.go | email_change 单测（purpose 隔离） |
| internal/app/client/account.go | staging + ConfirmEmailChange use-case |
| internal/app/client/account_g3_test.go | B 档测试更新 + 6 个新集成测试 |
| internal/api/clientgrpc/account.go | handler 透传 + 新 RPC |
| internal/api/clientgrpc/account_test.go | 端到端 staging 测试（-short 可跑） |
| sdk/go/client/account.go | UpdateAccount+url、ConfirmEmailChange |
| sdk/go/client/client_test.go | fakeAccount.ConfirmEmailChange |
| sdk/go/client/account_test.go | 新签名 + 新测试 |
| sdk/typescript/src/client/account.ts | updateAccount url?、confirmEmailChange |
| sdk/typescript/src/__tests__/contract.test.ts | 映射 + 绑定用例 |

### B2（5 文件）

| 文件 | 改动 |
|------|------|
| internal/app/functions/executions.go | queueMessage.Attempt |
| internal/app/functions/executions_test.go | 新入队 attempt=0 断言 |
| cmd/worker/worker.go | requeue 重写为 payload 内嵌计数 |
| cmd/worker/requeue_test.go（新增） | 5 个纯单测 |
| cmd/worker/consume_test.go（新增） | 2 个 consume 循环测试 |

### B3（14 文件）

| 文件 | 改动 |
|------|------|
| proto/server/v1/databases.proto | 3 处绑定 + 注释 |
| proto/server/v1/functions.proto | 2 处绑定 + 注释 |
| genproto/server/v1/{databases,functions}.* | 重新生成 |
| internal/app/server/databases.go | 移除 documentIDReserved |
| internal/app/functions/management.go | 移除 functionIDReserved |
| internal/app/server/databases_reserved_test.go | 保留字 id CRUD（fake + 集成） |
| internal/app/functions/security_test.go | 保留字移入合法清单 + 新单测 |
| sdk/typescript/src/server/{databases,functions}.ts | 路径切换 |
| console/src/api/{databases,functions}.ts | 路径切换 |
| docs/roadmap.md | §2.4 表 + backlog 备注（已迁移 + 升级指引） |
| docs/developer/09-api-guide.md | §1.3 breaking 声明 |
| docs/developer/12-sdk.md | §5.3 breaking 声明 |

---

## 5. 本地验证命令与结果（全部真实执行）

| 命令 | 结果 |
|------|------|
| `task generate-proto`（buf lint + generate） | ✅ exit 0，genproto 幂等 |
| `go build ./...` | ✅ exit 0 |
| `go vet ./...` | ✅ exit 0 |
| `gofmt -l .` | ✅ 无输出（一处 B1 测试 gofmt 修正后清零） |
| `go test -short -count=1 ./...` | ✅ 40 包全绿 |
| `go test -short -count=1 ./cmd/worker/... ./internal/app/functions/...` | ✅ 全绿 |
| `go test -short ./internal/api/clientgrpc/...` | ✅ 含新 staging 端到端用例 |
| `go test -short ./internal/infra/auth/...` | ✅ 含 email_change purpose 隔离用例 |
| `go test ./...`（sdk/go 模块） | ✅ 3 包全绿 |
| `cd sdk/typescript && npx tsc --noEmit && npm run test` | ✅ 16/16 |
| `task console-build` | ✅ vite build 成功 |
| `task build` | ✅ server/worker/client 三二进制 |

集成测试（真实 PG/Redis/Postgres 用例：B1 staging 全流程、B3 保留字 id CRUD）依赖
Docker，本地无环境，按约定交 CI。

---

## 6. 遗留问题

1. ~~**B1 免登录确认**~~（已落地）：ConfirmEmailChange 已按产品决策改为 `ACCESS_PUBLIC`
   （点链接即完成，recovery 同级安全模型），见 §1 决策 2。
2. ~~**Client API 保留字未迁移**~~（✅ 已关闭）：本批已完成 Client API 迁移——
   `/v1/databases/.../documents/count` → `documents:count`，`clientDocumentIDReserved` 已移除，
   `document_id="count"` 在 Client API 现为合法 id（详见 §3a）。
3. ~~**历史保留字 id 数据**~~（✅ 已关闭）：自定义动词迁移（B3 + 本批 Client API）完成后
   路由冲突根除，历史保留字 id 资源经 REST 自动恢复可访问，无需数据清理/重命名；
   检测 SQL 已附于 docs/roadmap.md §2.4 backlog 备注。
4. **旧文档路径字面量**：`docs/developer/08-functions.md`、`docs/manual-acceptance-checklist.md`
   等历史文档仍含旧路径（评审/验收记录性质），未在本批次清理。
5. B2 的 payload 内嵌计数与 MarkExecutionFailed 兜底并存：若未来引入死信队列可再演进。

---

## 6a. CI 第一轮失败与修复记录

第一轮 push 后 CI（run 31603870245）Backend 集成测试失败，总控定位并修复：

1. **`column "pending_email" does not exist (SQLSTATE=42703)`**：`users` 是 documentdb
   动态列系统集合，`pending_email` 未在系统集合 attribute spec 中 → 无物理列。
   **修复**：`internal/infra/documentdb/system_collection_specs.go` users spec 增加
   `users_pending_email`（email 类型，Size 320）。新项目建表即含该列；存量项目由
   `EnsureSystemCollections` 的 `reconcileSystemCollectionAttrs` 幂等补列
   （ADD COLUMN IF NOT EXISTS + document_attributes 元数据），**无需人工迁移步骤**。
   同时把 `pending_email` 加入 `serverSensitiveCollectionFields` 脱敏清单
   （Server API 读 users 集合时不泄露暂存邮箱），并同步 `docs/developer/06-databases.md`
   系统集合字段表。
2. **既有集成测试未适配 staging 语义**（`account_security_test.go`）：
   - `TestAccount_UpdateEmailRequiresOldPasswordAndRevokesSessions` 改为
     `TestAccount_UpdateEmailRequiresOldPasswordAndStages`——改邮箱必须带 url + 旧密码
     （缺失分别 InvalidArgument / Unauthenticated）；成功后 email 保持旧值、**会话不撤销**、
     旧邮箱仍可登录、新邮箱不可（撤会话语义由 ConfirmEmailChange 测试覆盖）。
   - `TestAccount_AnonymousUpgradeSetsPasswordWithoutOldPassword` 改为：密码立即生效并撤
     会话（占位邮箱 + 新密码可登录），邮箱变更走 staging（确认前保持占位邮箱）。
   - `TestAccount_ConfirmEmailChange_NewEmailTaken` 修正为真实竞态窗口：stage 后、确认前
     新邮箱被他人注册（pending_email 不占 email 唯一约束，SignUp 查重查不到）→ 确认时
     AlreadyExists。
3. **ConfirmEmailChange 唯一索引兜底**：查重与写入间的 TOCTOU 竞态由 email 唯一索引兜底，
   `documentdb.ErrDuplicateKey` 映射为 AlreadyExists（与 UpdateAccount 一致）。

修复后本地全量验证再次全绿；CI 第二轮/第三轮依次修复 authCtx 与 gofmt tab 问题后，
最终 run 31605512363 全绿（详见 §7）。

---

## 7. CI 验证

| Run | commit | 结果 | 说明 |
|-----|--------|------|------|
| [31603870245](https://github.com/torchwoodcloud/torchwood/actions/runs/31603870245) | 3ece633 | ❌ Backend 失败 | pending_email 无物理列 + 既有集成测试未适配 staging（修复见 §6a） |
| [31604815681](https://github.com/torchwoodcloud/torchwood/actions/runs/31604815681) | 117f960 | ❌ Backend 失败 | 集成测试以无 principal ctx 直调 ConfirmEmailChange → Unauthenticated（修复：改 authCtx） |
| [31605239552](https://github.com/torchwoodcloud/torchwood/actions/runs/31605239552) | 1b36132 | ❌ Backend 失败 | gofmt 检查：replaceAll 引入多余 tab（修复：gofmt -w） |
| [31605512363](https://github.com/torchwoodcloud/torchwood/actions/runs/31605512363) | 93f09e3 | ✅ **Backend + Frontend 全绿** | 最终全链路通过 |
| [31610640773](https://github.com/torchwoodcloud/torchwood/actions/runs/31610640773) | 637a922 | ✅ **Backend + Frontend 全绿** | 本批收尾（Client API `:count` 迁移 + 历史保留字 id 指引修正），一次通过 |

最终 run 覆盖：buf lint、gofmt、go vet、单元 + 集成测试（真实 PG/Redis/MinIO/Docker，含 B1 staging 全流程与 B3 保留字 id CRUD）、TS SDK test、console embed 构建、server/worker/client 二进制构建。
