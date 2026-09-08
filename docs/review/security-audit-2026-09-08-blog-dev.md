# 安全检测报告 — Torchwood 平台(经 blog dev 环境实测)

- **日期**:2026-09-08
- **被测入口**:`https://torchwood-dev.deeploop.run`(grpc-gateway,project: `blog`;作为 Torchwood Blog dev 部署的后端被整体检测)
- **方法**:客户端 SDK 实弹验证(`@torchwood/sdk` 0.2.0 直连 Client/Server 面)+ 存储响应头检查;未做压测/爆破(登录限速仅以 5 次探测)
- **配套文档**:blog 应用侧漏洞见 `torchwood-blog` 仓库 `docs/security-audit-2026-09-08.md`(**未授权 server function、JSON-LD XSS 等 10 项均归属 blog 应用,不在本文范围**)
- **整改**:T-01/T-02/T-03 已于 2026-09-08 落地(提交 `dccd925`/`17f51b2`/`0e368e9`),实现说明与复验记录见文末"整改实现与复验记录"

## 结论速览

| 编号 | 等级 | 问题 | 状态 |
|---|---|---|---|
| T-01 | 🟡 中 | 认证接口无限速/无锁定 | ✅ 已修复(dccd925) |
| T-02 | 🟡 中 | Server API 面公网暴露,API Key 为全项目库读写单一凭证 | ✅ 已修复(17f51b2) |
| T-03 | 🟠 高(策略) | 账号注册完全开放,缺少项目级注册策略开关 | ✅ 已修复(0e368e9) |
| — | ✅ | 存储服务响应头硬化、防用户枚举、文档级 ACL、realtime 拒匿名 | 验收基线 |

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

- 测试账号 `sec-test-873c7230@test.local`(项目 `blog`):**待手动删除**——平台侧 `DELETE /v1/account`(T-03,匿名化软删)已具备自助删除能力,但本仓库环境无该账号密码与 dev Console 管理员凭据,无法代执行;可用以下任一方式勾销:① 以该账号凭据调 `DELETE /v1/account`;② dev Console 管理员在用户管理中删除;③ 项目若仅需清理,可在 Console 直接对该用户执行删除。删除后同邮箱可立即重新注册、SignIn 恒为统一 invalid credentials(无存在性泄露)。
- 本次为 dev 环境结论;生产上线前建议对生产网关复跑一轮(Client 面 + Server 面 + 存储响应头基线)。**本报告的 dev 复验须先部署 `dccd925`/`17f51b2`/`0e368e9` 及迁移 000007/数据面 000012。**

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
