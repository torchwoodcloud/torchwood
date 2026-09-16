# 认证与授权

本章说明 Torchwood 的认证与授权体系：四类凭证、Principal 注入、策略注册表（proto 注解唯一声明 → 启动期收集为 PolicySet → 拦截器执行）、API Key 与 scope 词表、启动期 fail-closed 断言、用例层纵深防御，以及认证面的频控与注册治理。

> 事实源：`proto/shared/v1/authz.proto`、`cmd/server/internal/runtime/authz_policy.go`（策略收集）、`internal/domain/auth/policy.go`（策略类型与断言）、`internal/api/interceptor/jwt.go`（拦截器执行）、`internal/infra/auth/`（凭证校验）。

---

## 1. 四类凭证与解析优先级

`internal/domain/shared/principal.go` 定义了两个正交维度：

| 维度 | 取值 |
|------|------|
| `CredentialType` | `token`（JWT Bearer）· `session`（cookie，不透明/HMAC）· `api_key` · `execution`（`twx_` 前缀的函数执行 token） |
| `ActorKind` | `end_user`（终端用户）· `admin`（Console 管理员）· `service`（API Key 自动化）· `execution`（函数执行）· `system`（内部系统） |

`Validator.Authenticate`（`internal/infra/auth/authenticate.go`）是 gRPC、HTTP、Realtime 三个表面共用的认证入口。它先按以下优先级从请求中解析凭证（`shared.ParseAuthnRequest`），再交给 `ValidateCredential` 校验。各表面的差异化限制（如 Realtime 禁用 API Key、HTTP upload 禁用端用户凭证）由调用方在认证成功后自行施加。

| 优先级 | 来源 | 映射 |
|--------|------|------|
| 1 | `authorization` 头 | `Bearer <jwt>` → token；`Session <val>` → session；`ApiKey` / `Apikey <key>` → api_key |
| 2 | `cookie` | `TORCHWOOD_session_console` → console 会话；`TORCHWOOD_session_<projectID>` → 对应项目会话 |
| 3 | `x-api-key` 头 | 一律 api_key（头名可经 `security.api_key.header` 配置，默认 `x-api-key`） |

| 凭证 | 面向 | 说明 |
|------|------|------|
| 终端用户 JWT | Client API | `end-user-jwt` 域密钥签发，claims 含 `pid` / `sid` / `uid`，角色实时解析 |
| End-user session | Client API 浏览器 | cookie `TORCHWOOD_session_<projectID>`，`SessionCookieCodec` HMAC 编码（`internal/infra/auth/session_cookie.go`）或 JWT 形态 |
| Console admin session | Console | `TORCHWOOD_session_console` HttpOnly cookie（`internal/api/consolegrpc/cookies.go`），refresh 限定 `/v1/console/auth` 路径 |
| API Key | Server API | 库中仅存 `sha256(secret)` hex；细粒度 scope（§6）；以 `keys` + `key:<自身id>` 双角色参与文档 `_acl` 判定（per-key 私有，见 §6.4） |

---

## 2. Validator：凭证校验

`Validator` 实现 `interceptor.Validator` 接口（单方法 `Authenticate`），按凭证类型分发：

| 凭证 | 校验逻辑 |
|------|----------|
| `api_key` | `sha256(raw)` 查 `GetAPIKeyBySecretHash`；检查 `Enabled` / `ExpireAt`；检查所属项目 `Status==active`（否则 `Unauthenticated: project is not active`）。成功后 `ActorKind=service`、`Roles=["keys", "key:<APIKeyID>"]`（`keys` 承载 scope/API 面，`key:<id>` 承载数据隔离身份）、`Permissions=Scopes`、`ProjectID=key.ProjectID` |
| `token` | 先按 `admin-jwt` 域验签，失配再试 `end-user-jwt`（域分离见 §9）；分发到 `principalFromJWT` |
| `session` | 先尝试按 JWT 解析（console JWT 形态），否则 `SessionCookieCodec.Verify` 解出 `projectID:sessionID` 后查 `sessions` 集合 |

`principalFromJWT` 的两个分支：

- **admin**（`akd=admin`）：校验 `ttp==access`，查 `adminRepo`，校验 `RevokeBefore` 撤销状态；`IsPlatformAdmin = role∈{owner, admin}`。
- **end_user**（`akd=end_user`，含一次性 JWT——`oneTimeTokens.Consume` 原子消费防重放）：校验绑定的 `sessionID` 会话仍有效；校验 `ensureUserCanAuthenticate`（用户存在且 `CanAuthenticate`）；**实时调用 `resolveEndUserRoles`（`UserRoleResolver`）解析角色，解析失败 fail-closed 拒绝**——防止 JWT 里残留的旧角色继续生效。

非平台 admin 且 `principal.ProjectID` 非空时，`ValidateAdminProjectAccess` 校验 `adminProjectRepo.HasProjectAccess`。

---

## 3. 策略注册表：proto 唯一声明

授权策略的**唯一声明源是 proto 注解**（`proto/shared/v1/authz.proto`）。没有 Go 侧手写规则表——历史上的 `apiKeyScopeRules` / `adminRoleMethodRules` 已退役。

```proto
enum AccessLevel {
  ACCESS_LEVEL_UNSPECIFIED = 0;
  ACCESS_PUBLIC = 1;      // 匿名可调（自证凭证型：secret/challenge-token 即授权）
  ACCESS_END_USER = 2;    // 端用户会话/JWT 专属（Client API 面）
  ACCESS_SERVER = 3;      // admin 会话（admin_roles 把关）或 API key（api_key_scope 把关）
  ACCESS_PERMISSION = 4;  // admin 会话专属（permissions 把关），API key 一律拒绝
  ACCESS_SYSTEM = 5;      // 内部系统调用（当前无外部 RPC，预留 worker 面）
}
message MethodAuth {                   // 方法级完整策略（扩展号 52001）
  AccessLevel access = 1;              // 凭证族（必填）
  repeated string permissions = 2;     // PERMISSION 面角色门；END_USER 面归一 ["users"]
  repeated AdminRole admin_roles = 3;  // SERVER 面 admin 会话角色门（viewer/member/admin/owner）
  APIKeyScope api_key_scope = 4;       // SERVER 面 API key scope 门；缺省 = 不对 key 开放
}
message ServiceAuth { AccessLevel default_access = 1; }  // 服务级默认（扩展号 52002）
```

要点：

- 方法级 `method_auth` 优先；缺省回落到服务级 `service_auth.default_access`。细粒度字段（`admin_roles` / `api_key_scope` / `permissions`）只来自方法级，服务级不携带。
- `ACCESS_END_USER` 面的 `permissions` 为空时归一为 `["users"]`（端用户基础角色）。
- scope 资源词表是 proto enum `ScopeResource`，共 **16 个资源**：databases / users / groups / storage / projects / oauthproviders / functions / payments / assets / subscriptions / billing / outbox / audit_logs / leaderboards / analytics / runbooks。词表由 `VocabularyFromPolicies` 从策略派生，经 well-known 端点下发（`internal/api/serverhttp/wellknown.go`）；`apikeys` 资源已删除（APIKeysService 是 PERMISSION 面，key 凭证禁入），`economy` 已更名为 `assets`。
- **scope 方向**：`read` / `write` 是默认两档；`admin` 是**配置面方向**，按资源 opt-in——仅当某资源存在"热路径凭证与控制面凭证需最小特权分离"的诉求时启用（首个落地：leaderboards 的 board 配置管控，提交分值的密钥不得改榜配置），并非全资源默认第三档。三个方向独立匹配：`<res>.write` 不命中 admin 门方法，`<res>.admin` 也不命中 write / read 门方法；裸资源 `<res>` 与通配符 `*` / `all` 放行全部方向。注意：新增 admin 门方法对存量裸资源 / 通配符 key 立即生效，发布说明须点名。Functions 执行 principal 的 declared scopes 刻意不收录 admin。
- console 面 me 型方法的会话标签 `"console"` 不是角色，经 `permissions` 字符串值域登记。

### 3.1 收集与消费

`cmd/server/internal/runtime` 的 `BuildMethodPolicies(fileDescs...)` 从业务 proto 文件清单构造 `domainauth.PolicySet`，经 Wire provider `ProvideMethodPolicies` 成为唯一注入点。PolicySet 的消费方：

| 消费方 | 用途 |
|--------|------|
| `interceptor.NewAuthInterceptor(validator, policySet)` | 请求期门禁执行（§4） |
| `serverhttp` / `realtime` 镜像点 | 经 `AllowedAdminRoles` / `HasAPIKeyScope` 派生，禁止手写角色集 |
| `ProvideScopeVocabulary` → `ScopeVocabulary` | API Key 创建校验与 well-known 下发的合法 scope 词表 |
| `task gen:authz-matrix` | 生成 `docs/developer/authz-matrix.md`（字节级漂移锁定，勿手改） |
| 启动期断言（§7） | 完备性 / 语义 / 项目寻址 fail-closed |

---

## 4. 拦截器执行

gRPC 侧由 `UnaryAuthMiddleware`（`interceptor/jwt.go`）按策略驱动，单实现覆盖全部方法：

```
PolicySet.Get(fullMethod) 未命中 → 403 policy_missing（fail-closed；启动期另有断言兜底）
ACCESS_PUBLIC → 尽力解析凭证（成功则注入 Principal）→ 直接放行
Authenticate 失败 / 无 principal → 401（拒绝原因写审计）
ACCESS_SERVER（凭证族 = API key、execution token 或 admin 会话）：
  ├─ 其他凭证族 → 401 "developer API requires x-api-key header or admin session"
  └─ api_key / execution：policies.AllowsAPIKey(scope) 不命中 → 403
      （未声明 scope 的方法 fail-closed，通配符不豁免；两者同一 scope 求值路径）
admin 会话主体：
  ├─ admin_roles 非空且 HasAnyRole 不命中 → 403（nil 语义 = 不限角色，viewer 可调）
  ├─ X-Torchwood-Project 多值 → 400；单值写入 principal.ProjectID
  └─ ValidateAdminProjectAccess → 403
permissions 非空（PERMISSION / END_USER 面）：
  ├─ api_key 凭证 → 403（scope */all 也不放行——console/owner 类权限是 admin 会话专属）
  └─ HasAnyRole(perms) 不命中 → 403
→ contexts.WithPrincipal(ctx, principal) → handler
```

补充说明：

- **拒绝并联审计**：`WithDenyAuditSink(auditRepo)` 把拦截器层拒绝（policy_missing、凭证无效、scope 缺失、角色拒绝等）直接落审计——这些请求到不了后面的 audit 中间件，无双写。
- `Principal` 结构（`domain/shared/principal.go`）：`ActorID`、`ActorKind`、`CredentialType`、`IsPlatformAdmin`、`ProjectID`、`UserID`、`SessionID`、`APIKeyID`、`Roles`、`Permissions`（API Key 的 scopes 存放在 `Permissions`）。
- SERVER 面放行集合包含 `ActorKindExecution`（`twx_` 执行 token，与 API key 走同一 scope 求值路径）；`ActorKindSystem` 供内部系统调用。

---

## 5. 拦截器链全景

```
clientInfo → auth → rateLimit → audit → usage → validate(protovalidate) → handler
```

| 拦截器 | 挂点理由 |
|--------|----------|
| `ClientInfo`（`interceptor/client.go`） | 最先执行：经 `security.trusted_proxies` 校验解析真实 IP，供后续限流与审计使用 |
| `Auth`（`interceptor/jwt.go`） | 依赖凭证与策略；拒绝经 deny-audit 直落审计 |
| `RateLimit`（`interceptor/ratelimit.go`） | 依赖 trusted-proxy 后的 IP 与 principal（匿名按 IP、认证按主体计数）；Redis 固定窗口，基础设施故障按熔断策略处理 |
| `Audit`（`interceptor/audit.go`） | 请求审计落库，以 `auditRowEligible` 准入门为前置（§6.5）；管理面写操作附加脱敏请求摘要与 client metadata |
| `Usage`（`interceptor/usage.go`） | 用量计数 |
| `Validate`（`interceptor/validate.go`） | **链尾**：protovalidate 形状校验（`buf.validate` 注解统一求值，写法见 `09-api-guide.md`）。失败请求照常产生审计行与用量计数 |

---

## 6. API Key 与 scope

### 6.1 存储与词表

**存储**：secret 由 `uuid()+uuid()` 生成，库中仅存 `sha256(secret)` hex（`internal/app/server/apikeys.go`），明文只在创建响应里出现一次。

**词表单一来源**：`ProvideScopeVocabulary(PolicySet)` 从策略注册表派生合法 scope 词表——每个被方法引用的资源贡献 `{资源名, 资源名.read, 资源名.write}`，叠加 `*` / `all`。Key 创建校验与 well-known 下发消费同一词表；死 scope 断言（§7）保证词表内资源均被至少一个方法引用。

### 6.2 scope 语法

| scope 形态 | 语义 |
|------------|------|
| `*` / `all` | 全量放行（通配） |
| `<resource>` | 该资源族全项目读写 |
| `<resource>.read` / `<resource>.write` | 该资源族全项目单向 |
| `databases:<database_id>` | 限定单个 database 的读写；访问其他 database 一律 403 |
| `databases:<database_id>.read` / `.write` | 单个 database 的单向 |
| `storage:<bucket_id>`（及 `.read` / `.write`） | 限定单个 bucket |

规则：

- 实例 ID 规则：`database_id` 为 `^[a-z][a-z0-9]{0,27}$`（与物理命名同源）；`bucket_id` 为 `^[0-9a-zA-Z_-]{1,64}$`。
- 可寻址资源目前仅 `databases` / `storage`；其余资源携带 `:` 的 scope 创建期即 400，执行期不匹配（fail-closed）。
- 执行点在 `PolicySet.AllowsAPIKeyTargets`（gRPC 拦截器 + serverhttp `auth.go`）：按方法声明的资源族从请求体提取目标实例（`database_id` / `bucket_id`；`CreateDatabase` / `GetBucket` 等取 `id`）。**无实例寻址的方法**（`ListDatabases` / `ListBuckets` / `GetStorageUsage` / `CreateBucket` 等）对实例限定 scope 一律 403——`databases:blog` 的 key 不能列出或创建其他库，也不能跨库寻址。
- **DDL 归属**：server 面 DatabasesService 的 DDL（CreateDatabase / CreateCollection / CreateAttribute / CreateIndex…）与文档 CRUD 共用 `databases` 资源 scope——`databases:blog` 天然覆盖 blog 库的全部 DDL 与数据读写，无需组合其他 scope。单一库应用的最小组合示例：`scopes: ["databases:blog", "storage:blog-media"]`。
- CreateAPIKey / UpdateAPIKey 对非法 scope（未知服务、不可寻址资源、非法实例 ID、非法方向）直接 400。

### 6.3 Key 治理与轮换

`UpdateAPIKey`（PERMISSION owner/admin）可修改 `name` / `scopes` / `enabled` / `expire_at`（proto3 optional，未设置 = 不修改）。禁用与过期**立即生效**（`validateAPIKey` 每请求读库校验）。

secret 无原地轮换。平滑轮换流程（Console 详情页「轮换」引导固化同一流程）：

1. 创建新 key（相同 scopes，名称加 `-rotated` 后缀；secret 仅显示一次）；
2. 双 key 并存，应用切换到新 key（旧 key 此期间继续可用）；
3. 旧 key 经 `UpdateAPIKey` 设置 `expire_at`（或直接禁用 / 删除）下线。

### 6.4 防护语义

- **防自铸提权**：APIKeysService 是 PERMISSION 面，API key 凭证天然禁入——key 永远无法管理 key。
- **不默认 bypass 文档权限**：API Key 以 `Roles=["keys", "key:<自身id>"]` 参与文档 `_acl` 判定；仅 `SystemPrincipal` 与平台 admin 绕过（见 `06-databases.md`）。
- **per-key 私有**：key 创建文档时，空 ACE 种子绑 `read/update/delete:key:<自身id>`——其他 key 不可见（Get 返回 NotFound 防枚举）；跨 key 协作需显式授予对方 `key:<id>` ACE；存量显式 `keys` ACE 保留有效（默认种子不再产生）。
- **认证失败限速**：X-API-Key 认证失败（哈希不匹配 / 禁用 / 过期）按来源 IP 计数（`security.login_throttle.api_key_auth`，默认 10 次/60s），超限 429；gRPC 面由拦截器统一执行，multipart HTTP 面暂仅做拒绝审计。

### 6.5 审计

- 审计落行以 `auditRowEligible` 准入门为前置（`internal/api/interceptor/audit.go`，噪声治理）：server / console 面仅非读动词落审计（读方法不记，与凭证类型无关）；client 面仅 AccountService 非读安全动作记录；`AnalyticsService/IngestEvents` 显式静默；grpc.health / reflection 不记。**拒绝（deny）审计与限速（throttled）审计不经此门、全保留**。
- eligible 请求的审计行统一含 actor（API key 即 key id）、项目、full method、资源 ID（`WithAuditResource`）、结果，无需逐 handler 记录。
- 查询面：`AuditLogsService.ListAuditLogs`（SERVER 面，admin_roles admin/owner + `audit_logs.read` scope），审计行带 request 摘要与 client metadata；CLI `torchwood audit-logs` 可消费。

---

## 7. 启动期 fail-closed 断言

策略在启动期过三道闸，任一违例直接启动失败：

**1. 语义断言**（`domainauth.AssertSemantic`，经 `ProvideMethodPolicies` 求值）：

- 完备性：access 未声明 → `missing auth policy`；`ACCESS_SYSTEM` 当前禁用。
- SERVER 面必须声明 `api_key_scope`（不对 key 开放的方法应改用 PERMISSION 面）；scope 资源 / 方向必须在词表内。
- **死 scope 检测**：`ScopeResource` 词表内每个资源必须被至少一个方法引用——资源从词表退役后残留即启动失败。
- client / console 面值域：client 面只允许 PUBLIC / END_USER，且 END_USER 恒为 `["users"]`；client PUBLIC 方法必须显式登记白名单（`clientPublicMethodWhitelist`，防误标公开）。console 面只允许 PUBLIC / PERMISSION，permissions 值域为 `[console]` / `[owner]` / `[owner,admin]`，且写动词（Create* / Update* / Delete*）必须 `[owner]`。
- **项目寻址不变量**：server 面请求体不得携带 `project_id` 字段（项目上下文一律来自凭证）；存量违例登记在 `ProjectIDAllowlist` 渐进清空（ProjectsService 自身豁免），新增即失败。
- streaming RPC 禁用（当前无 stream 接入，出现即断言失败）。

**2. 档位断言**（`ClassifyTier`）：档位是从声明**派生**的分类，不进 proto（避免第二策略源）。SERVER / PERMISSION 面方法必须落入四个已声明档位之一，否则启动失败。档位语义见 `authz-matrix.md` 档位列：`read_only`（read + 不限角色）/ `business_write`（write + member,admin,owner）/ `delegated_platform`（admin,owner）/ `platform_only`（PERMISSION 面 permissions ⊆ {admin,owner}）。

**3. 注册完备断言**（`assertRegisteredMethodsHaveAuthz`）：每个已注册 gRPC 方法必须命中 PolicySet，缺失即 `registered grpc methods missing authz annotation`；`grpc.health.v1` / `grpc.reflection` 框架服务豁免（由部署层网络策略保护）。

策略变更后 `task gen:authz-matrix` 重新生成矩阵文档，漂移由 `authz_matrix_doc_test.go` 字节级锁定。

---

## 8. 用例层纵深防御

拦截器是第一层；绕过拦截器直调 use-case 时，由 `internal/app/shared/authz.go` 的四守卫兜底（双面共享的 use-case 用 `RequireAnyOf` 组合）：

| 守卫 | 语义 |
|------|------|
| `RequireEndUser` | 纯端用户 actor；项目绑定等上下文校验留在调用面 |
| `RequirePlatformPrincipal` | 平台级操作（Functions 写、API Key 管理、用户密码 / 令牌等）：仅 `admin.IsPlatformAdmin`，API Key 与受限 admin 一律拒绝 |
| `RequireConsolePrincipal` | Console 专属；角色细粒度由拦截器 permissions 门禁把关 |
| `RequireServerPrincipal` | 业务写：console admin 会话或 API key 主体；匿名 / 端用户返回 `PermissionDenied` |

Functions DDL 与 Storage 已对齐 `RequireServerPrincipal` 口径（Databases 组与 Functions 同口径：API Key 持 `databases.write` 可做 DDL；schema DDL 不在 `RequirePlatformPrincipal` 内）。

---

## 9. JWT / cookie / 加密工具

| 工具 | 位置 | 要点 |
|------|------|------|
| `jwtparser` | `pkg/jwtparser/` | HS256；短名 claims：`tid/uid/usn/akd/pid/sid/ttp/rls/scp/exp/iat`；`exp`+`iat` 必校验，仅接受 HS256。三个用途域（`PurposeEndUserJWT` / `AdminJWT` / `SessionCookie`）各自以 `HMAC-SHA256(master, purpose)` 派生密钥，跨域不通用；更换 master 即全域失效 |
| Console cookie | `internal/api/consolegrpc/cookies.go` | `TORCHWOOD_session_console`（`Path=/`）+ `TORCHWOOD_console_refresh`（`Path=/v1/console/auth`）；`HttpOnly` + `SameSite=Lax` + `Secure(https)`；refresh 带 rotation，重用检测 `RotateMismatch` 撤销全部 token；登出 `Max-Age=0` |
| SessionCodec | `internal/infra/auth/session_cookie.go` | `base64url(projectID:sessionID):HMAC-SHA256`，验签后查 `sessions` 集合 |
| `password` | `pkg/password/` | Argon2id（t=3 m=65536 p=4），`$argon2id$v=19$...` 格式，常量时间比较 |
| `secretbox` | `pkg/secretbox/` | `sha256("torchwood-secretbox:"+secret)` 派生 AES-256-GCM 密钥，密文带 `enc:v1:` 前缀，空值透传兼容旧明文。用于 OAuth `client_secret` 与 TOTP `factor.Secret` 加密 |

---

## 10. 认证面局部频控

除 §5 的通用 API 限流外，认证面另有一层**失败计数型**局部频控（`internal/infra/auth/login_throttle_redis.go`），用于登录 / 注册暴力破解防护。

### 10.1 SignIn：账号 + IP 双维失败计数

- **邮箱维度**按 email 小写规范化计数；**IP 维度**按 trusted-proxy 校验后的来源 IP 计数。两维独立累计，任一触顶即拒。
- **计数时机**：密码错误（账号存在）时双维各 +1。**未注册邮箱只计 IP 维度，邮箱键永不落笔**——防"探测锁死任意邮箱"的 DoS，同时使 IP 维度计数与账号存在性无关：探测存在 / 不存在账号在相同强度下同样触发 429，**429 不构成账号存在性 oracle**。
- **成功重置**：登录成功（含 SignUp 完成后的首次登录）清零该 email+IP 的计数。
- **拒绝语义**：超限返回 `429 ResourceExhausted`，错误体为统一 `shared.v1.ErrorResponse`（`error.type=rate_limit_error`），文案恒为 `too many failed sign-in attempts, try again later`，不区分触发维度与账号是否存在；响应携带 `Retry-After` 头（由 `google.rpc.RetryInfo` detail 转译，秒向上取整）。
- **审计**：触发限速时写一条 `status="throttled"` 审计行（action 为 SignIn 的 full method，带项目 / IP / UA）。
- **默认阈值**：双维各 5 次失败 / 60s 窗口。

### 10.2 SignUp：按 IP 注册频控

注册按 project+IP 计数，默认 10 次/小时；超限 429（含 Retry-After，语义同上）。无效 project 不消耗频控预算（project 校验先行）。

### 10.3 配置

四个维度均可配置（`limit` + `window` 时长串），未配置或非法值回落内置默认：

```yaml
security:
  login_throttle:
    email: { limit: 5, window: "60s" }         # 账号维度失败计数
    ip: { limit: 5, window: "60s" }            # IP 维度失败计数
    signup_ip: { limit: 10, window: "1h" }     # 注册 IP 频控
    api_key_auth: { limit: 10, window: "60s" } # X-API-Key 认证失败 IP 频控
```

实现要点：窗口为 Redis 滑动窗口（`INCR` + 首次 `EXPIRE` 原子化）；与通用限流拦截器的键空间不同，两者叠加生效；admin console 登录共用同一组件（`admin` namespace，双维计数）。

---

## 11. 项目注册策略与账号注销

### 11.1 注册策略

项目设置 `projects.registration_policy`（默认 `open`，保持存量行为），经 `UpdateProject` 切换，Console 项目详情页提供面板：

| 策略 | SignUp 语义 |
|------|-------------|
| `open`（默认） | 匿名自助注册 |
| `invite_only` | 必须携带有效 `invite_code`；无码 / 错码 / 过期 / 耗尽 / 吊销统一 403 `ACCOUNT.INVITE_CODE_INVALID`（不区分原因，无探测面） |
| `closed` | 一律 403 `ACCOUNT.REGISTRATION_CLOSED` |

- 错误码走 `"CODE: message"` 消息前缀约定（对齐 docdb 域码体系），HTTP 403 + `permission_error`。
- **邀请码**：存控制面 `public.invite_codes` 表，`twi_` 前缀 128-bit 随机（无枚举面）；一次性为默认，可限次（1..10000）、可过期、可吊销（owner / admin 管理：Create / List / DeleteInviteCode，key 凭证禁入）。**消费原子**：单语句 `UPDATE … WHERE 有效性 AND used_count < max_uses RETURNING`，行锁串行化——并发同码恰好一个成功。
- 未知策略值 fail-closed（按 closed 处理，防脏数据意外开放注册）。

### 11.2 DeleteAccount：匿名化软删

`DELETE /v1/account`（端用户 JWT）注销当前登录账号：

1. **凭据立即失效**：全部会话撤销（refresh 失去锚点）+ `status=deleted`——validator 每次鉴权实时读库 `CanAuthenticate`，存量 access token 立即 401。
2. **不泄露"曾存在"**：`email` / `pending_email` / `phone` / `name` / `prefs` / `factors` / `password_hash` 就地清洗（email 置为 `deleted-<userID>@deleted.invalid` 项目内唯一占位值），**同邮箱可立即重新注册**；SignIn 依旧统一返回 `invalid credentials`；OAuth identities 一并删除。
3. **数据保留（显式决策，不做级联删除）**：其名下文档、文件、memberships、审计行保留为孤儿数据——文档 / 文件按既有 ACL 收敛到不可见（owner 角色随账号消失）；审计是追责记录，不随账号抹除。物理清除归运维面保留策略。
4. 软删行不得复生：`deleted` 状态仅删除路径可写，任何外部入参不可设置。

### 11.3 OAuth 重定向白名单

项目 settings JSONB 键 `auth.oauth_allowed_redirect_urls`（条目数组）是**全部端用户重定向流的落点白名单**，四处消费同一校验器（`validateProjectOAuthRedirectURLs`）：OAuth2 浏览器回调发起、魔法链接、恢复、验证。

- **匹配规则**（`projects.MatchRedirectURL`）：条目 = scheme + host（大小写不敏感）+ 可选路径前缀；条目无 path 时放行该 host 全部路径。
- **回落语义**：键缺失 / 为空时，默认白名单 = `localhost` / `127.0.0.1`（http+https）+ 本站 `server.http.public_url` origin。跨域前端（独立站点域名）必须显式配置，否则发起端 400 `success url is not allowed for this project`。
- **发起端点**：浏览器流推荐走 302 发起端点 `GET /v1/account/oauth2/{provider}/authorize?project_id=&success=&failure=`（`serverhttp/oauth_handler.go`，命名对齐 Auth0 / Supabase 等主流与 RFC 6749 的授权入口心智）。服务端完成与 JSON 发起面同一套校验后 `Set-Cookie` nonce 并 302 到 provider 授权页。发起是 **top-level 导航**，nonce cookie 落在 API 域第一方上下文，回调（同为 top-level 导航）必然携带——**任意客户前端域零 CORS 配置、不受第三方 cookie 政策影响**（JSON 发起面的跨源 fetch 会丢失 `Set-Cookie`，除非前端 `credentials:"include"` 且 CORS 对该 origin 放行凭据）。端点自带 per-IP 限流（复用 `security.rate_limit.ip` 维度，limiter 故障 fail-open）；全部响应 `Cache-Control: no-store`。失败分层：白名单校验前失败（项目不存在 / URL 未过白名单）返回 400 纯文本不跳转（此时 failure URL 尚不可信）；校验后失败（provider 未启用）302 回 `failure?error=oauth_failed`。JSON 发起面 `GET /v1/account/sessions/oauth2/{provider}` 保留（token 面 / 服务端调用），浏览器流建议全部迁移到 authorize 端点。
- **管理入口**：`PUT /v1/server/projects/{project_id}/oauth-redirect-allowlist`（整表替换；空数组 = 清空回落默认）与 Console 项目详情页 Redirect Allowlist 卡片。PERMISSION `[owner,admin]` 平台专属面（key 凭证禁入）——白名单是钓鱼劫持面（可改写登录流落点）。读取走 `GET /v1/server/projects/{id}` 的 `oauth_allowed_redirect_urls` 投影（仅投影该键，不透出其余 settings）。
- **持久化**：`SettingsWriter.SetProjectSetting` 单键原子写（`jsonb_set` / `'-'` 操作符），不同 settings 键并发写互不覆盖；`settings` 列不进 `UpdateProject` 白名单。

---

## 12. 公开认证面威胁模型：project_id 不是机密

**`project_id` 与 endpoint 是公开标识，不是机密**——与 OAuth 的 client_id 同构，本来就内嵌在所有前端应用里。`SignUp` / `SignIn` / `RefreshToken` 在 proto 上显式声明 `ACCESS_PUBLIC`、请求体携带 `project_id`，TS SDK 侧对应 `X-Torchwood-Project` 头。因此"知道 endpoint + project_id 就能调登录注册"是 BaaS 公开注册模式的固有形态，**安全边界不依赖隐藏 project_id**，而由四层独立承担：

1. **注册策略（§11.1）**：`closed` 一律 403、`invite_only` 凭一次性邀请码放行、未知策略值 fail-closed——不想公开注册的项目必须显式收口，而非指望 project_id 保密。
2. **认证面频控（§10）+ 通用限流**：登录失败按 email+IP 双维计数、注册按 project+IP 计数，公开端点的撞库 / 枚举 / 批量注册被限速压制（429 不构成账号存在性 oracle）；通用 IP 限流叠加生效。
3. **project_id 只选租户、不授权**：登录签发的端用户 JWT 绑定 `pid` claim；请求期 `X-Torchwood-Project` 头仅对 Console admin 会话生效（多值 / 越权走审计失败路径），端用户无法借该头跨项目访问。
4. **数据安全与注册可达性解耦**：注册接口可调不意味着数据可碰——数据面按 `06-databases.md` 的权限内核（集合 / 文档两级 ACL + RLS fail-closed）判定，新注册账号默认对既有数据零可见面。

运维取舍：`open` 策略下垃圾账号是固有残余风险（频控是缓解不是杜绝），需要收紧时切 `invite_only` / `closed`，或按产品需要叠加邮箱验证等流程。泄露 project_id 无需轮换——它不是凭证；需要保密与轮换的是 API key（§6）与 `security.jwt.secret`。

---

## 相关文档

- `authz-matrix.md` — 全方法授权矩阵（生成物，勿手改）
- `06-databases.md` — 文档面 `_acl` 权限模型、RLS 判定与 roles_sig 验签
- `09-api-guide.md` — 新增 RPC 时 authz 注解与 protovalidate 的写法
- `03-configuration.md` §6.2 — 会话 cookie 配置
