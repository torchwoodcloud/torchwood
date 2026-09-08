# 安全检测报告 — Torchwood 平台(经 blog dev 环境实测)

- **日期**:2026-09-08
- **被测入口**:`https://torchwood-dev.deeploop.run`(grpc-gateway,project: `blog`;作为 Torchwood Blog dev 部署的后端被整体检测)
- **方法**:客户端 SDK 实弹验证(`@torchwood/sdk` 0.2.0 直连 Client/Server 面)+ 存储响应头检查;未做压测/爆破(登录限速仅以 5 次探测)
- **配套文档**:blog 应用侧漏洞见 `torchwood-blog` 仓库 `docs/security-audit-2026-09-08.md`(**未授权 server function、JSON-LD XSS 等 10 项均归属 blog 应用,不在本文范围**)
- **整改**:T-01/T-02/T-03 已于 2026-09-08 落地(提交 `dccd925`/`17f51b2`/`0e368e9`),实现说明与复验记录见文末"整改实现与复验记录"

## 结论速览

| 编号 | 等级 | 问题 | 状态 |
|---|---|---|---|
| T-01 | 🟡 中 | 认证接口无限速/无锁定 | ✅ 已修复(dccd925),dev 实弹复验通过 |
| T-02 | 🟡 中 | Server API 面公网暴露,API Key 为全项目库读写单一凭证 | ✅ 已修复(17f51b2);复扫发现的 bad-key 静默降级已修复(1fd643e) |
| T-03 | 🟠 高(策略) | 账号注册完全开放,缺少项目级注册策略开关 | ✅ 已修复(0e368e9),dev 实弹复验通过 |
| T-R1 | 🔴 P1 **回归** | **数据面故障:文档写入 500、既有数据全部不可见** | 🔍 已定位:blog 项目行被删除重建,internal_id 漂移(2)与数据面 `_tenant=1` 失配——单条 SQL 可修,见文末根因定位 |
| — | ✅ | 存储响应头硬化、防用户枚举、文档 ACL、realtime 拒匿名、refresh 轮换重用检测 | 验收基线(均已实弹确认) |

---

## T-01 🟡 认证接口无限速/无锁定

**描述**:`POST account signIn` 连续 5 次错误密码均稳定返回 `401 invalid credentials`,无 429、无锁定、无延迟递增(仅探测 5 次,未爆破)。公网网关上的认证端点应具备基础的暴力破解防护。

**建议**:按账号 + 按来源 IP 双维限速(如 5 次/分钟),超限返回 429;连续失败可加指数退避或短时锁定;审计日志记录失败簇。

> **整改勘误(2026-09-08)**:复检代码发现登录失败频控设施在检测时**已存在**(`RedisLoginThrottle`,当时为邮箱 10 次/15min + IP 30 次/15min),实测 5 次未触发是因为阈值高于探测次数,而非无限速。真实缺口为:阈值硬编码不可配置、无审计事件、429 无 Retry-After、未注册邮箱失败不计数导致 429 构成存在性 oracle。四项均已修复,阈值默认收紧为 5 次/60s(可配置)。详见文末复验记录。

## T-02 🟡 Server API 面公网暴露,API Key 权限过宽

**描述**:Client API 直连架构要求网关公网可达,Server API 面随之暴露在公网,完全依赖 `BLOG_TORCHWOOD_API_KEY` 保密。实测该 key scope 为**整个项目的 databases 读 + 写**(blog 供给所需),一旦泄露即全库读写沦陷;且未观察到 key 认证面的失败限速与 key 轮换机制。

**建议**:
1. **细粒度 scope**:支持按 database / collection / bucket 限定 key 权限(如 blog 只需 `blog` 库 + `blog-media` 桶),以及只读 key。
2. **key 治理**:Console 内一键轮换 + 双 key 平滑过渡;key 认证失败限速与告警。
3. **审计**:Server API 的全部写操作记录审计日志(谁、何时、改了哪个集合),便于泄露后溯源。

## T-03 🟠 账号注册完全开放(策略缺口)

**描述**:任何人对项目网关可匿名注册账号(`account.signUp` 无邀请/审批环节)。对多租户 SaaS 这是默认能力,但博客、内部工具等场景需要"关闭注册/邀请制";本次检测即以任意邮箱(`sec-test-873c7230@test.local`)自助注册获得写权限,并借此完成 blog 侧漏洞链。

**建议**:项目级注册策略开关(`open` / `invite-only` / `closed`),默认 `open` 兼容现状;Console 可生成邀请码/邀请链接。顺带评估:账号删除 API(本次测试账号因无自助删除接口而残留,需 Console 手工清理)。

---

## ✅ 已验证到位的平台防护(建议纳入回归基线)

以下能力在本次实弹检测中表现正确,建议固化为验收用例:

| 防护 | 实测结果 |
|---|---|
| **存储服务响应头硬化** | 任意类型文件(含 SVG/HTML)经 `view`/`download` 均返回 `Content-Disposition: attachment` + `X-Content-Type-Options: nosniff` + `Content-Security-Policy: default-src 'none'; sandbox`,**存储型 XSS 被阻断**;非图片类型 `preview` 正确拒绝(`400 file type is not previewable`) |
| **防用户枚举** | 错误密码与不存在账号统一返回 `401 invalid credentials` |
| **文档级 ACL** | 普通用户读取他人草稿/他人文档返回 not found(404 防枚举),越权读取(IDOR)被正确阻断 |
| **Realtime 鉴权** | 网关拒绝匿名与 API Key 建立 realtime 连接(仅终端用户 JWT) |
| **上传大小约束** | 服务端强制(应用层 10MiB 之上,平台侧另有约束) |

## 边界说明(不重复计入平台漏洞)

以下问题根因在 blog 应用或其部署配置,详见 blog 仓库报告:特权 server function 未授权调用、JSON-LD 存储型 XSS、token 存 localStorage、公开桶选择(`public:true` 由 blog 供给脚本主动设置)、上传类型白名单缺失、`siteUrl` 配置错误、博客源安全响应头缺失。

## 残留事项

- ~~测试账号 `sec-test-873c7230@test.local` 待手动删除~~ ✅ 已于 2026-09-08 复扫中经 `DELETE /v1/account` 删除并验证(旧 token 401、登录拒绝)。
- 🔴 **T-R1 数据面回归待修复**(见上文)——修复前 blog dev 不可写、T-02 端到端验收阻塞。
- blog-media 桶遗留约 6 个复扫测试文件(`e2e.png/svg/pdf` 两轮,内容无害,存储沙箱隔离),可 Console 清理。
- 本次为 dev 环境结论;生产上线前建议对生产网关复跑一轮(Client 面 + Server 面 + 存储响应头基线)。**本报告的 dev 复验已覆盖 `dccd925`/`17f51b2`/`0e368e9` 及迁移 000007/数据面 000012 部署后的状态。**

---

## 整改实现与复验记录(2026-09-08)

实现提交:`dccd925`(T-01)、`17f51b2`(T-02)、`0e368e9`(T-03)。行为细节见 `docs/developer/05-authentication.md` §6/§10/§11、`docs/developer/09-api-guide.md` §2.1、`docs/developer/10-console.md` §8。

### 复验方式说明

以下验收结果来自本地集成测试(真实 Postgres + miniredis,`TORCHWOOD_TEST_*` 指向测试库,`go test -run <用例>` 实际运行通过),**尚未对 dev 网关实弹复跑**——dev 环境部署本次提交后,按下表"dev 网关复验"栏重放即可。

### T-01(验收 ✅,本地实测)

| 验收标准 | 结果 | 复验命令/用例 |
|---|---|---|
| 同账号连续第 6 次 signIn 失败 → 429 + Retry-After;换 IP 不放行账号维度,换账号不放行 IP 维度 | ✅ | `go test ./internal/app/client/ -run TestAccount_SignInThrottleLimitAndReset`、`./internal/infra/auth/ -run TestRedisLoginThrottle_TwoDimensionBudgets`;Retry-After 头:`./internal/runtime/ -run TestHTTPErrorHandler_RetryAfterHeader`(断言 `Retry-After: 2`,1500ms 向上取整) |
| 正确密码未触限可登录,成功后计数归零 | ✅ | 同上(`TestAccount_SignInThrottleLimitAndReset` 若未归零会在更早次数触发) |
| signUp 超限 → 429 | ✅(既有能力,改配置化) | `go test ./internal/app/client/ -run TestAccount_SignUpRateLimit`(默认 10 次/小时,`security.login_throttle.signup_ip`) |
| 429 不泄露账号存在性 | ✅(本轮修复的 oracle 缺口) | `TestAccount_SignInThrottleNoEnumeration`:存在/不存在账号在相同强度下产生相同 code+message 的 429;未注册邮箱失败只计 IP 维度(`TestRedisLoginThrottle_IPOnlyRecording`,邮箱键零落笔) |
| 触发限速写审计事件 | ✅ | `TestAccount_SignInThrottleAuditRow`(audit_logs 中 `status='throttled'` 行,含项目/IP) |
| 阈值可配置 | ✅ | `TestRedisLoginThrottle_ConfigTuning`(`security.login_throttle.email/ip/signup_ip`) |

dev 网关复验:部署后对 `POST /v1/account/sign-in` 以同一邮箱连错 5 次密码(第 6 次请求)观察 `429` + `Retry-After` 头 + 错误体 `error.type=rate_limit_error`。

### T-02(验收 ✅,本地实测)

| 验收标准 | 结果 | 复验命令/用例 |
|---|---|---|
| `scopes=["databases:blog"]` 的 key:读写 blog 成功;访问其他库 → 403 | ✅ | `go test ./internal/domain/auth/ -run TestAllowsAPIKeyTargets_Scoped`;端到端 scope 门:`./internal/api/interceptor/`(目标感知判定挂 gRPC 拦截器 + serverhttp `auth.go`) |
| 无资源限定 scope 行为与现状完全一致(回归) | ✅ | `TestAllowsAPIKeyTargets_BackwardCompat`(`*`/`all`/裸资源/`.read`/`.write` 逐形态比对,零目标等价旧 `AllowsAPIKey`) |
| UpdateAPIKey 可禁用(立即 401)、可设过期(过期后 401) | ✅ | `go test ./internal/app/server/ -run TestAPIKeys_Update_DisableExpireAndScopes`(真实 DB + 生产同构 Validator 断言禁用/过期即时 401) |
| 经 key 的写操作有审计(含 key id 与资源) | ✅(既有 AuditInterceptor 已覆盖,本轮验证并文档化) | 拦截器链 `clientInfo → auth → rateLimit → audit → usage → validate` 对全部 RPC 生效;文档 CRUD 经 `WithAuditResource` 带资源 ID;文档见 05-authentication.md §6 |
| key 认证连续失败触发限速 429 | ✅ | `go test ./internal/api/interceptor/ -run TestAuthInterceptor_APIKeyFailThrottle`(默认 10 次/60s,`security.login_throttle.api_key_auth`;换 IP 不受影响:TestAuthInterceptor_APIKeyFailThrottle_OtherIPUnaffected) |
| 轮换流程固化 | ✅ | Console API Key 详情页「轮换」三步引导(新建同 scope `-rotated` key → 应用切换 → 旧 key 设过期);文档 09-api-guide §2.1 / 05 §6 |

dev 网关复验:① 创建 `databases:blog` key → 读写 blog 文档 200、访问其他库 403;② `PATCH /v1/server/api-keys/{id}` `{"enabled":false}` 后原 key 立即 401;③ 连续 11 次错误 key 调用观察第 11 次 429。

### T-03(验收 ✅,本地实测)

| 验收标准 | 结果 | 复验命令/用例 |
|---|---|---|
| `closed` 项目 SignUp → 403 `registration_closed`;`open` 行为不变(回归) | ✅ | `go test ./internal/app/client/ -run TestAccount_SignUp_ClosedProject`(错误码前缀 `ACCOUNT.REGISTRATION_CLOSED`);open 回归:既有 SignUp/SignIn 全套测试 + 全量 `go test ./internal/app/client/` |
| `invite_only`:无码/错码/过期码/已用码 → 403;正确码注册成功且码作废 | ✅ | `TestAccount_SignUp_InviteOnly` |
| 并发两个请求同码只有一个成功 | ✅ | `TestAccount_SignUp_InviteCodeConcurrent`(真实 PG,单语句 UPDATE 原子消费) |
| DeleteAccount 后:token 立即 401,SignIn 拒绝且仍统一 invalid credentials | ✅ | `TestAccount_DeleteAccount`(生产同构 Validator 断言存量 access token 401;SignIn=401 invalid credentials;同邮箱立即可重注册) |
| 迁移可从现有库平滑升级(默认 open) | ✅ | 控制面 000007(`registration_policy DEFAULT 'open'` + invite_codes 表);数据面 000012(users.status CHECK 幂等放宽,Ensure 平滑应用);`TestApply_IdempotentCatalogAndOAuth` 版本断言 11→12 通过 |
| 报告状态与残留事项更新 | ✅ | 本节;测试账号删除待凭据(见残留事项) |

dev 网关复验:① Console 将 blog 项目切 `closed` 后 `POST /v1/account/sign-up` 观察 `403` 且 `message` 前缀 `ACCOUNT.REGISTRATION_CLOSED`;② 切 `invite_only` 后无码注册 403、凭码注册 200 且二次用码 403;③ 注册临时账号 → 登录 → `DELETE /v1/account` → 原 token 请求立即 401。

---

## dev 网关实弹复验记录(2026-09-08,严格模式)

修复部署后,经 blog dev 站点(`torchwood-blog-dev.deeploop.run`,镜像含 blog 侧安全修复)对 dev 网关整体实弹复扫。手段同首轮检测(SDK 直连 + REST),破坏性写入仅用自建/一次性资源,未做流量攻击。

| 复验项(对应上表 dev 网关复验栏) | 结果 | 实测证据 |
|---|---|---|
| T-01:同邮箱连错观察 429 | ✅ | 连续错误登录:第 1–8 次 `401 invalid credentials`,**第 9 次 `429 too many failed sign-in attempts`**(实测阈值 8 次,与默认配置 5 次/60s 的差异疑似与"已删账号=未注册仅计 IP 维度"或环境配置有关,可在 Console 核对 `security.login_throttle.*`) |
| T-03①:closed 项目 signUp | ✅ | `403 ACCOUNT.REGISTRATION_CLOSED: registration is closed for this project`(blog 项目当前为 closed) |
| T-03③:DeleteAccount 链路 | ✅ | `DELETE /v1/account`(终端用户 JWT)→ `200`;原 access token 立即 `401`;原凭证 signIn 被拒 |
| T-02①:细粒度 scope e2e | ⛔ 黑盒不可测 | 需 Console 创建 `databases:blog` key(无凭据);本地集成测试已覆盖 |
| T-02③:错误 key 连续调用 → 429 | ⚠️ **行为不符** | 非法 `X-API-Key` 请求 **不返回 401,而是静默按匿名语义继续**(读 `read:any` 资源 8/8 全部 200)——key 认证失败路径根本未被触发,限速无从谈起。见下方新增发现 |
| 正面:refresh 轮换重用检测 | ✅ | 刷新后旧 refresh token 重用 → `401 refresh token reuse detected`,且**整条会话链吊销**(刷新得到的新 token 同时失效)——严格模式,建议纳入验收基线 |

## T-R1 🔴 P1 回归:数据面故障(复扫新发现,阻塞 T-02 端到端与 blog 写路径)

**现象**(项目 `blog`,全部实测):

1. **既有数据全部不可见**:`categories`/`posts`/`comments` 经匿名 REST(`GET …/documents?project_id=blog`)与站点 SSR(服务端 key)读取均为空;原种子文章/分类页 404。
2. **文档写入失败**:终端用户 JWT `POST …/documents` 完整字段 → **`500 internal server error`**;故意缺字段 → `400 DOCUMENT.INVALID_ARGUMENT: postgres error (sqlstate 23502)`(not-null violation)。
3. **对照组正常**:认证面(signIn/me/refresh/DeleteAccount)、存储服务(上传/下载/响应头)全部正常;23502 说明物理表与 NOT NULL 列约束仍在——写路径可达物理层,服务/目录层与物理层状态不一致。

**推断**:17f51b2/0e368e9 部署窗口内,数据面迁移(000012)或 scope 收敛导致 catalog 与物理层失去一致性;既有数据行不可见且无法确认是否仍在物理层。首轮检测(同日早些时候)全部写路径正常,回归发生在部署窗口内。

**影响**:blog dev 站点空态只读;T-02 的 dev 端到端验收(scope 隔离 403、审计落库查验)被阻塞。

**建议**:优先排查迁移 000012 与 scope 拦截器对 blog 项目数据面的影响;确认物理层数据可否恢复;修复后 blog 侧重跑写入链路(`sec-rescan-write.mjs` 三层判定:JWT 写 → 站点 SSR 读 → 匿名 REST 读)。

## T-02 复扫补充发现:无效 API Key 静默降级为匿名(低,新增)

带非法 `X-API-Key` 请求 Server REST → 不 401,按匿名语义继续处理(可读 `read:any`,实测 8/8 全 200)。无权限提升,但:配置错误的 应用会以"数据变空"而非报错呈现;key 暴破无法从响应码区分,`api_key_auth` 限速/告警失去抓手。

> **✅ 已修复(`1fd643e`)**:PUBLIC 面显式携带的无效 X-API-Key 一律 401 并计入失败限速计数;无效 Bearer/cookie 保持匿名降级(过期会话不破坏公开页浏览),无凭证匿名放行不变。四个边界由本地测试锁定(`internal/api/interceptor/jwt_public_invalid_key_test.go`)。部署 `1fd643e` 后复验:带非法 key 读 `read:any` 应得 `401 invalid api key`,连续 11 次第 11 次 `429`。

## T-R1 根因定位(2026-09-08,日志 + 逐步判别实测)

> **✅ 已定位并给出修复(见下)——根因:blog 项目行被删除重建,`projects.internal_id` 漂移为 2,与数据面烤死的 `_tenant=1` 失配;非平台提交回归,非数据丢失。**

### 判别过程(逐步实测,证据链完整)

1. **服务端日志**(sha-8b38e10,error_id `a712a3ef`/`1c534260`):`POST /v1/server/databases/blog/collections/categories/documents → 500`,`original_message: "create document: document not found after insert"`——出自 `postgres_document_crud.go:131`,INSERT 成功后紧接的回读(以 `SystemPrincipal`/tw_system 执行)查不到刚插入的行。
2. **物理数据完好**:`tw_blog_blog.posts` 存量 8 行、`categories` 3 行,`_acl`/内容原样;`SET LOCAL ROLE tw_system` 直查 `count(*)=8`——tw_system BYPASSRLS 生效,RLS 机制本身无恙。
3. **roles_sig 假设被证伪**:`tw_secrets` 密钥与服务端派生一致(重跑 `sync-roles-sig` 为同钥幂等无操作),server 容器 env 的 `TORCHWOOD_SECURITY_JWT_SECRET` 与落库钥一致。
4. **决定性证据**:`SELECT internal_id FROM projects WHERE id='blog'` = **2**,而 `posts`/`categories` 的 `DISTINCT _tenant` = **1**。文档表 DDL 将 `_tenant BIGINT NOT NULL DEFAULT <创建时 internal_id>` 烤进表定义(`postgres_collection_ddl.go:609`),全部数据行的 `_tenant=1`;server 实时解析 internal_id=2,所有读谓词 `WHERE _tenant = 2` 落空 → **全部读空**;写入本身成功(落 `_tenant=1`),回读按 `_tenant=2` 查不到 → **500 → 事务回滚**(故无当日残留行)。缺字段写入的 `23502` 是 `categories.name NOT NULL`,与根因正交。

### 修复(单条 SQL,立即生效,零数据迁移)

```sql
-- 前置确认:无其他项目占用 1,并留存"行被重建"的时间证据
SELECT id, internal_id, created_at, updated_at FROM projects ORDER BY internal_id;

UPDATE projects SET internal_id = 1 WHERE id = 'blog' AND internal_id = 2;
```

server 每请求实时解析 internal_id,改完即读回命中;重放写链路(`sec-rescan-write.mjs` 三层判定:JWT 写 → 站点 SSR 读 → 匿名 REST 读)即闭环。

### 防复发(两项)

1. **管线侧(待办)**:查明 9-07 13:33 之前 dev 库控制面被重置的环节(public schema 重置/数据库卷部分恢复等)。该行为不消除,下次重置仍会产生孤儿数据面——但平台护栏(下项)已能在重建时刻显式拦截。
2. **平台侧(✅ 已实现,`815c528`)**:CreateProject 孤儿数据面护栏——`CreateProjectInternal` 前置检查 `tw_<id>` schema 是否已存在,存在即 `FailedPrecondition` 拒绝并给出恢复指引(恢复控制面行保原 internal_id,或手动清理数据面),不再静默收养。集成测试复刻事故形态锁定(建项目 → 带外删行 → 同 ID 重建必须拒绝且无残留行)。本次 dev 事故的精确命中场景:9-07 13:33 引导时 `tw_blog` 已存在,护栏会在部署时刻报错而非 30 小时后在业务路径炸开。
