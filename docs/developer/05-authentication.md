# Torchwood 认证与授权

> 四凭证、Principal 注入、**策略注册表**（proto 注解唯一声明 → `PolicySet` 收集 → 拦截器执行）与纵深防御。
> 以代码为准：`proto/shared/v1/authz.proto`、`internal/runtime/authz_policy.go`（收集）、`internal/domain/auth/policy.go`（策略类型与断言）、`internal/api/interceptor/jwt.go`（执行）、`internal/infra/auth/`（凭证校验）。
> 最新更新：2026-09-07

---

## 1. 四凭证与优先级

`internal/domain/shared/principal.go` 定义两正交维度：

| 维度 | 取值 |
|------|------|
| `CredentialType` | `token`（JWT Bearer）· `session`（cookie 不透明/HMAC）· `api_key` |
| `ActorKind` | `end_user`（终端用户）· `admin`（Console 管理员）· `service`（API Key 自动化） |

`Validator.Authenticate`（`internal/infra/auth/authenticate.go:14`）是 **gRPC / HTTP / Realtime 共用的认证入口**：按以下优先级解析凭证（`shared.ParseAuthnRequest`）后走 `ValidateCredential` 校验；Grant 差异（Realtime 禁 API key、HTTP upload 禁 end-user）由调用方在成功后施加。

| 优先级 | 头 | 映射 |
|--------|----|------|
| 1 | `authorization` | `Bearer <jwt>` → `token`；`Session <val>` → `session`；`ApiKey`/`Apikey <key>` → `api_key` |
| 2 | `cookie` | `TORCHWOOD_session_console` → console；`TORCHWOOD_session_<projectID>` → 对应项目 |
| 3 | `x-api-key` | 一律 `api_key`（header 名可配 `security.api_key.header`，默认 `x-api-key`） |

| 凭证 | 面向 | 说明 |
|------|------|------|
| 终端用户 JWT | Client API | `end-user-jwt` 域密钥签发，claims 含 `pid`/`sid`/`uid`，Roles 实时解析 |
| End-user session | Client API 浏览器 | `TORCHWOOD_session_<projectID>`，`SessionCookieCodec` HMAC（`internal/infra/auth/session_cookie.go`）或 JWT 形态 |
| Console admin session | Console | `TORCHWOOD_session_console` HttpOnly cookie（`internal/api/consolegrpc/cookies.go`），refresh 限 `/v1/console/auth` |
| API Key | Server API | `secret → sha256 hex` 存库，细粒度 scope（§6），以 `keys` + `key:<自身id>` 双角色参与文档 `_acl`（B14 per-key 私有） |

---

## 2. Validator（`internal/infra/auth/validator.go`）

`Validator` 实现 `interceptor.Validator` 接口（`Authenticate` 单方法，`jwt.go:21`）：

| 凭证 | 校验 |
|------|------|
| `api_key` | `sha256(raw)` → `GetAPIKeyBySecretHash`（`validateAPIKey:124`）；查 `Enabled`/`ExpireAt`；**查 `project Status==active`**（`validator.go:143`）否则 `Unauthenticated: project is not active`；成功 `ActorKind=service`、`Roles=["keys", "key:<APIKeyID>"]`（B14：`keys` 承载 scope/API 面，`key:<id>` 承载数据隔离身份）、`Permissions=Scopes`、`ProjectID=key.ProjectID` |
| `token` | 先 `admin-jwt` 域验签，失配再试 `end-user-jwt`（`parseJWT:110`，域分离见 §9）；分发到 `principalFromJWT:161` |
| `session` | 先当 JWT 试解（console JWT），否则 `SessionCookieCodec.Verify` 得 `projectID:sessionID` → `principalFromSession:229` 查 `sessions` 集合 |

`principalFromJWT` 分支：

- `akd=admin`：校验 `ttp==access`、查 `adminRepo`、校验 `RevokeBefore`（`checkAdminTokenRevoked:345`），`IsPlatformAdmin = role∈{owner,admin}`；
- `akd=end_user`（含一次性 JWT：`oneTimeTokens.Consume` 原子消费防重放）、校验绑定 `sessionID` 的会话仍有效（`validateEndUserSession:265`）、校验 `ensureUserCanAuthenticate:298`（用户存在且 `CanAuthenticate`）、**实时 `resolveEndUserRoles:287`（`UserRoleResolver`）fail-closed 拒绝，防 JWT 旧角色残留**。

`ValidateAdminProjectAccess:318`：非平台 admin 且 `principal.ProjectID` 非空时，校验 `adminProjectRepo.HasProjectAccess`。

---

## 3. 策略注册表（proto 唯一声明 → PolicySet）

授权策略的**唯一声明源是 proto 注解**（`proto/shared/v1/authz.proto`），机制重设计后不再有任何 Go 侧手写规则表（`apiKeyScopeRules`/`adminRoleMethodRules` 已退役）：

```proto
enum AccessLevel {
  ACCESS_LEVEL_UNSPECIFIED = 0;
  ACCESS_PUBLIC = 1;      // 匿名可调（自证凭证型：secret/challenge-token 即授权）
  ACCESS_END_USER = 2;    // 端用户会话/JWT 专属（Client API 面）
  ACCESS_SERVER = 3;      // admin 会话（admin_roles 把关）或 API key（api_key_scope 把关）
  ACCESS_PERMISSION = 4;  // admin 会话专属（permissions 把关），API key 一律拒绝
  ACCESS_SYSTEM = 5;      // 内部系统调用（当前无外部 RPC，预留 worker 面）
}
message MethodAuth {           // 方法级完整策略（扩展号 52001）
  AccessLevel access = 1;      // 凭证族（必填）
  repeated string permissions = 2;   // PERMISSION 面角色门；END_USER 面归一 ["users"]
  repeated AdminRole admin_roles = 3; // SERVER 面 admin 会话角色门（viewer/member/admin/owner）
  APIKeyScope api_key_scope = 4;      // SERVER 面 API key scope 门；缺省 = 不对 key 开放
}
message ServiceAuth { AccessLevel default_access = 1; }  // 服务级默认（扩展号 52002）
```

要点：

- 方法级 `method_auth` 优先，缺省回落服务级 `service_auth.default_access`；细粒度字段（`admin_roles`/`api_key_scope`/`permissions`）仅来自方法级，服务级不携带。
- `ACCESS_END_USER` 面 `permissions` 为空时归一为 `["users"]`（端用户基础角色，`domainauth.RoleEndUserTag`）。
- scope 资源词表是 proto enum `ScopeResource`（databases/users/groups/storage/projects/oauthproviders/functions/payments/**assets**/subscriptions/billing/outbox）；`apikeys` 资源已删除（APIKeysService 为 PERMISSION 面，key 凭证禁入），`economy` 已更名 `assets`。
- console 面 me 型方法的会话标签 `"console"` 不是角色，经 `permissions` 字符串值域登记。

**收集与注入**：`internal/runtime` 的 `BuildMethodPolicies(fileDescs...)`（`authz_policy.go:85`）从业务 proto 文件清单（`authzFileDescriptors()`，`grpc.go:186` 单一清单）构造 `domainauth.PolicySet`——经 Wire provider `ProvideMethodPolicies`（`internal/runtime/provides.go:22`）成为唯一注入点。`PolicySet` 的消费面：

| 消费方 | 用途 |
|--------|------|
| `interceptor.NewAuthInterceptor(validator, policySet)` | 请求期门禁执行（§4） |
| `serverhttp`/`realtime` 镜像点 | 经 `AllowedAdminRoles`/`HasAPIKeyScope` 派生，禁止手写角色集（`policy.go:143/151`） |
| `ProvideScopeVocabulary` → `ScopeVocabulary` | API key 创建校验（`app/server/apikeys.go:32`）与 well-known 下发（`serverhttp/wellknown.go:131`）的合法 scope 词表 |
| `task gen:authz-matrix` | 生成 `docs/developer/authz-matrix.md`（字节级漂移锁定，勿手改） |
| 启动期断言（§7） | 完备性/语义/项目寻址 fail-closed |

---

## 4. 拦截器执行（`UnaryAuthMiddleware`，`jwt.go:126`）

按策略驱动，单实现覆盖全部方法：

```
PolicySet.Get(fullMethod) 未命中 → 403 policy_missing（fail-closed；启动期另有断言兜底）
ACCESS_PUBLIC → 尽力解析凭证（成功则注入 Principal）→ 直接放行
Authenticate 失败 / 无 principal → 401（拒绝原因写审计，见下）
ACCESS_SERVER（凭证族 = API key 或 admin 会话）：
  ├─ 其他凭证族 → 401 "developer API requires x-api-key header or admin session"
  └─ api_key：policies.AllowsAPIKey(scope) 不命中 → 403（未声明 scope 的方法 fail-closed，通配符不豁免）
admin 会话主体：
  ├─ admin_roles 非空且 HasAnyRole 不命中 → 403（nil 语义 = 不限角色，viewer 可调）
  ├─ X-Torchwood-Project 多值 → 400；单值写入 principal.ProjectID
  └─ ValidateAdminProjectAccess → 403
permissions 非空（PERMISSION/END_USER 面）：
  ├─ api_key 凭证 → 403（scope */all 也不放行——console/owner 类权限是 admin 会话专属）
  └─ HasAnyRole(perms) 不命中 → 403
→ contexts.WithPrincipal(ctx, principal) → handler
```

- **拒绝并联审计（M5 C6）**：`WithDenyAuditSink(auditRepo)` 把拦截器层拒绝（policy_missing/credential_invalid/scope 缺失/角色拒绝等）直接落审计——这些请求到不了后面的 audit 中间件，无双写。
- `Principal`（`domain/shared/principal.go`）：`ActorID`/`ActorKind`/`CredentialType`/`IsPlatformAdmin`/`ProjectID`/`UserID`/`SessionID`/`APIKeyID`/`Roles`/`Permissions`（API Key 的 scopes 存 `Permissions`）。

---

## 5. 拦截器链全景（`internal/runtime/grpc.go:100`）

```
clientInfo → auth → rateLimit → audit → usage → validate(protovalidate) → handler
```

| 拦截器 | 挂点理由 |
|--------|----------|
| `ClientInfo`（`interceptor/client.go`） | 最先：经 `security.trusted_proxies` 校验解析真实 IP，供后续限流/审计 |
| `Auth`（`interceptor/jwt.go`） | 需要凭证与策略；拒绝经 deny-audit 直落审计 |
| `RateLimit`（`interceptor/ratelimit.go`） | 需要 trusted-proxy 后的 IP 与 principal（匿名按 IP、认证按主体）；Redis 固定窗口，基础设施故障按熔断策略（匿名公开读 fail-closed 语义见 C8 系列） |
| `Audit`（`interceptor/audit.go`） | 请求审计落库（含校验失败的 4xx） |
| `Usage`（`interceptor/usage.go`） | 用量计数 |
| `Validate`（`interceptor/validate.go`） | **链尾**：protovalidate 形状校验（`buf.validate` 注解统一求值，见 `09-api-guide.md` §2.3）——失败请求照常产生审计行与用量计数，仅把 handler 开头的形状检查外提为 proto 声明 |

---

## 6. API Key 与 scope（词表从策略派生）

**存储**：`secret = uuid()+uuid()`，库中仅 `sha256(secret)` hex（`internal/app/server/apikeys.go`），明文只在创建响应出现一次。

**词表单一来源**：`ProvideScopeVocabulary(PolicySet)`（`internal/runtime/provides.go:35`）从策略注册表派生合法 scope 词表——每个被方法引用的资源贡献 `{资源名, 资源名.read, 资源名.write}`，叠加 `*`/`all`。key 创建校验与 well-known 下发都消费同一词表（死 scope 断言保证词表内资源均被引用，见 §7）。

**匹配语义**（`PolicySet.AllowsAPIKey`，`policy.go:164`）：方法门 = 该方法 `method_auth` 声明的 `api_key_scope`；key 的 scope 集合中 `*`/`all` 全量放行、裸资源名放行该资源全部方法、`<res>.read` 仅读方法、`<res>.write` 仅写方法。**未声明 `api_key_scope` 的方法（PERMISSION/END_USER/PUBLIC 面）一律拒绝——与通配符无关**。

**防护**：APIKeysService 是 PERMISSION 面（platform_only 档），API key 凭证天然禁入（防自铸提权）；API Key 以 `Roles=["keys", "key:<自身id>"]` 参与文档 `_acl`（不默认 bypass；仅 `SystemPrincipal`/平台 admin 绕过——见 `06-databases.md` §7）。**per-key 私有（B14，C6 决议）**：key 创建文档的空 ACE 种子绑 `read/update/delete:key:<自身id>`——其他 key 不可见（Get = NotFound 防枚举）；跨 key 协作需显式授予对方 `key:<id>` ACE；存量显式 `keys` ACE 保留有效（默认种子不再产生）。

---

## 7. 启动期 fail-closed 断言

策略在启动期过三道闸，任一违例直接启动失败：

1. **语义断言**（`domainauth.AssertSemantic`，`policy.go:405`，经 `ProvideMethodPolicies` 求值）：
   - 完备性：access 未声明 → `missing auth policy`；`ACCESS_SYSTEM` 当前禁用；
   - SERVER 面必须声明 `api_key_scope`（不对 key 开放请改 PERMISSION）；scope 资源/方向必须在词表；
   - **死 scope 检测**：`ScopeResource` 词表内每个资源必须被至少一个方法引用（词表演进后残留即红）；
   - client/console 面值域：client 面只允许 PUBLIC/END_USER 且 END_USER 恒 `["users"]`，client PUBLIC 方法必须显式登记白名单（`clientPublicMethodWhitelist`，防误标公开）；console 面只允许 PUBLIC/PERMISSION，permissions 值域 `[console]`/`[owner]`/`[owner,admin]` 且写动词（Create*/Update*/Delete*）必须 `[owner]`；
   - **项目寻址不变量**：server 面请求体不得携带 `project_id` 字段（项目上下文一律来自凭证），存量违例登记 `ProjectIDAllowlist` 渐进清空（`policy.go:332`，ProjectsService 自身豁免），新增即失败；
   - streaming RPC 禁用（当前无 stream 接入，出现即断言失败）。
2. **档位断言**（`ClassifyTier`，`policy.go:289`——档位是从声明派生的分类，不进 proto，避免第二策略源）：SERVER/PERMISSION 面方法必须落入四个已声明档位之一，否则启动失败。档位语义见 `authz-matrix.md` 档位列：`read_only`（read + 不限角色）/ `business_write`（write + member,admin,owner）/ `delegated_platform`（admin,owner）/ `platform_only`（PERMISSION 面 permissions ⊆ {admin,owner}）。
3. **注册完备断言**（`assertRegisteredMethodsHaveAuthz`，`grpc.go:157`）：每个已注册 gRPC 方法必须命中 PolicySet，缺失即 `registered grpc methods missing authz annotation`；`grpc.health.v1`/`grpc.reflection.` 框架服务豁免（部署层网络策略保护）。

当前矩阵规模见 `authz-matrix.md` 头部（方法总数与四 Access 分布）；策略变更后 `task gen:authz-matrix` 重新生成，漂移由 `internal/runtime/authz_matrix_doc_test.go` 字节级锁定。

---

## 8. 用例层纵深防御（`internal/app/shared/authz.go`）

拦截器是第一层；绕过拦截器直调 use-case 时由四守卫兜底（机制 v8 评审 B1 归组，双面共享 use-case 用 `RequireAnyOf` 组合）：

- `RequireEndUser:17`（纯端用户 actor 语义；项目绑定等上下文校验留在调用面）；
- `RequirePlatformPrincipal:34`（Functions 写、API Key 管理、用户密码/令牌等平台级：仅 `admin.IsPlatformAdmin`，API Key / 受限 admin 一律拒绝）；
- `RequireConsolePrincipal:48`（Console 专属；角色细粒度由拦截器 permissions 门禁把关）；
- `RequireServerPrincipal:61`（业务写：console admin 会话或 API key 主体；匿名/端用户 `PermissionDenied`）。

Functions DDL 与 Storage 已对齐 `RequireServerPrincipal` 口径（Databases 组自 Round3 起与 Functions 同口径，API Key 持 `databases.write` 可做 DDL；schema DDL 不在 `RequirePlatformPrincipal` 内）。

---

## 9. JWT / cookie / 加密工具

| 工具 | 位置 | 要点 |
|------|------|------|
| `jwtparser` | `pkg/jwtparser/jwt.go` + `keys.go` | HS256，`tid/uid/usn/akd/pid/sid/ttp/rls/scp/exp/iat` 短名；`exp`+`iat` 必校验，仅 `HS256`；`PurposeEndUserJWT/AdminJWT/SessionCookie` 三域 `HMAC-SHA256(master,purpose)` 派生，跨域不通用（改 master 即全域失效） |
| Console cookie | `internal/api/consolegrpc/cookies.go` | `TORCHWOOD_session_console`（`Path /`）+ `TORCHWOOD_console_refresh`（`Path /v1/console/auth`），`HttpOnly`+`SameSite=Lax`+`Secure(https)`；refresh 带 rotation + 重用 `RotateMismatch` 撤销；登出 `Max-Age=0` |
| SessionCodec | `internal/infra/auth/session_cookie.go` | `base64url(projectID:sessionID):HMAC-SHA256`，验签后查 `sessions` 集合 |
| `password` | `pkg/password/password.go` | Argon2id `t=3 m=65536 p=4`，`$argon2id$v=19$...`，`ConstantTimeCompare` |
| `secretbox` | `pkg/secretbox/secretbox.go` | `sha256("torchwood-secretbox:"+secret)` → AES-256-GCM，`enc:v1:` 前缀，空透传兼容旧明文；OAuth `client_secret`（`bunrepo/oauth_provider_repo.go:24`）、TOTP `factor.Secret`（`infra/auth/totp.go:52`） |

> 详见 `docs/developer/06-databases.md` §7（文档面 `_acl` 权限模型与 `keys`/`key:{id}` 角色、RLS 判定执行点、roles_sig 验签）、`docs/developer/authz-matrix.md`（全方法授权矩阵，生成物）、`09-api-guide.md` §2.1/§2.3（authz 注解与 protovalidate 校验写法）、`03-configuration.md` §6.2（会话 cookie）。

---

## 10. 认证面局部频控（T-01：登录/注册暴力破解防护）

除 §5 的通用 API 限流（IP 300/min 量级）外，认证面另有一层**失败计数型**局部频控（`internal/infra/auth/login_throttle_redis.go`，安全审计 2026-09-08 T-01 整改）：

### 10.1 SignIn：账号 + IP 双维失败计数

- **邮箱维度**：按 email 小写规范化计数；**IP 维度**：按 trusted-proxy 校验后的来源 IP 计数。两维独立累计、任一触顶即拒。
- **计数时机**：密码错误（账号存在）双维各 +1；**未注册邮箱只计 IP 维度，邮箱键永不落笔**（R05-P1-5：防"探测锁死任意邮箱"DoS；同时使 IP 维度计数与账号存在性无关——探测存在/不存在账号在相同强度下同样触发 429，**429 不构成账号存在性 oracle**）。
- **成功重置**：登录成功（含 SignUp 完成后的首次登录）即清零该 email+IP 的计数。
- **拒绝语义**：超限返回 `429 ResourceExhausted`，错误体为统一 `shared.v1.ErrorResponse`（`error.type=rate_limit_error`），文案恒为 `too many failed sign-in attempts, try again later`，**不区分触发维度与账号是否存在**；响应携带 `Retry-After` 头（由 status 的 `google.rpc.RetryInfo` detail 转译，秒向上取整）。
- **审计**：触发限速时写一条 `status="throttled"` 审计行（action 为 SignIn 的 full method，带项目/IP/UA）。
- **默认阈值**：双维各 **5 次失败 / 60s 窗口**。

### 10.2 SignUp：按 IP 注册频控

注册按 project+IP 计数，默认 **10 次/小时**；超限 429（含 Retry-After，语义同上）。无效 project 不消耗频控预算（与 SignIn 同序：project 校验先行）。

### 10.3 配置（`security.login_throttle`）

三个维度均可配置（复用 `RateLimit.Dimension` 形状：`limit` + `window` 时长串），未配置/非法值回落内置默认：

```yaml
security:
  login_throttle:
    email: { limit: 5, window: "60s" }      # 账号维度失败计数
    ip: { limit: 5, window: "60s" }         # IP 维度失败计数
    signup_ip: { limit: 10, window: "1h" }  # 注册 IP 频控
```

实现要点：窗口为 Redis 滑动窗口（`INCR`+首次 `EXPIRE` 原子化）；与通用限流拦截器的键空间不同，叠加生效；admin console 登录共用同一组件（`admin` namespace，双维计数）。

---
