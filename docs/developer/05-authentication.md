# Torchwood 认证与授权

> 四凭证、Principal 注入、方法级 authz 双表与纵深防御。以代码为准：`internal/infra/auth/validator.go:21`、`internal/api/interceptor/`、`proto/shared/v1/authz.proto`、`internal/infra/server/grpc.go:217`。
> 最新更新：2026-08-23

---

## 1. 四凭证与优先级

`internal/domain/shared/principal.go` 定义两正交维度：

| 维度 | 取值 |
|------|------|
| `CredentialType` | `token`（JWT Bearer）· `session`（cookie 不透明/HMAC）· `api_key` |
| `ActorKind` | `end_user`（终端用户）· `admin`（Console 管理员）· `service`（API Key 自动化） |

`internal/api/interceptor/jwt.go:77` 的 `Authenticate` 按以下优先级解析 `metadata`（`shared.ParseAuthnRequest`）：

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
| API Key | Server API | `secret → sha256 hex` 存库，细粒度 scope（§4），以 `keys` + `key:<自身id>` 双角色参与 `_acl`（B14 per-key 私有） |

---

## 2. Validator（`internal/infra/auth/validator.go:21`）

`Validator` 实现 `interceptor.Validator`：

| 凭证 | 校验 |
|------|------|
| `api_key` | `sha256(raw)` → `GetAPIKeyBySecretHash`；查 `Enabled`/`ExpireAt`；**查 `project Status==active`**（`validator.go:136`）否则 `Unauthenticated: project is not active`；成功 `ActorKind=service`、`Roles=["keys", "key:<APIKeyID>"]`（B14：keys 承载 scope/API 面，`key:<id>` 承载数据隔离身份）、`Permissions=Scopes`、`ProjectID=key.ProjectID` |
| `token` | 先 `admin-jwt` 域验签，失配再试 `end-user-jwt`（`parseJWT:109`，域分离见 §6）；分发到 `principalFromJWT` |
| `session` | 先当 JWT 试解（console JWT），否则 `SessionCookieCodec.Verify` 得 `projectID:sessionID` → `principalFromSession` 查 `sessions` 集合 |

`principalFromJWT` 分支：

- `akd=admin`：校验 `ttp==access`、查 `adminRepo`、校验 `RevokeBefore`（`checkAdminTokenRevoked:334`），`IsPlatformAdmin = role∈{owner,admin}`；
- `akd=end_user`（含一次性 JWT：`oneTimeTokens.Consume` 原子消费防重放）、校验绑定 `sessionID` 的会话仍有效（`validateEndUserSession:257`）、校验 `ensureUserCanAuthenticate`（用户存在且 `CanAuthenticate`）、**实时 `resolveEndUserRoles:279`（`UserRoleResolver`）fail-closed 拒绝，防 JWT 旧角色残留**。

`ValidateAdminProjectAccess:310`：非平台 admin 且 `principal.ProjectID` 非空时，校验 `adminProjectRepo.HasProjectAccess`。

---

## 3. Principal 注入

`AuthInterceptor.UnaryAuthMiddleware`（`jwt.go:77`）：

```
ACCESS_PUBLIC → 尽力解析凭证 → 直接放行
无凭证 / ValidateCredential 失败 → 401
ACCESS_API_KEY：
  ├─ 要求 token=api_key 或 admin 会话（所以 admin 可经 X-Torchwood-Project 调 Server API）
  ├─ api_key 禁调 APIKeysService（IsAPIKeysServiceMethod → 403）
  └─ APIKeyScopeAllowed(scope) → 403
admin 主体：查 adminRoleMethodRules → 403（viewer 细粒度）；读 X-Torchwood-Project → ValidateAdminProjectAccess
permissionMethods：HasAnyRole(perms) → 403（且 api_key 一律 403）
→ contexts.WithPrincipal(ctx, principal) → handler（handler 经 contexts.Principal 读取）
```

`Principal`（`domain/shared/principal.go`）：`ActorID`/`ActorKind`/`CredentialType`/`IsPlatformAdmin`/`ProjectID`/`UserID`/`SessionID`/`APIKeyID`/`Roles`/`Permissions`（API Key 的 scopes 在 `Permissions`）。

Console 多项目：仅 `admin` 会话读 `X-Torchwood-Project` 写入 `ProjectID` 再校验项目访问权。

---

## 4. API Key 与 scope（`apikey_scope.go:25`）

**存储**：`secret = uuid()+uuid()`，库中仅 `sha256(secret)` hex（`internal/app/server/apikeys.go`），明文只在创建响应出现一次。

**scope 规则**：`apiKeyScopeRules`（`apikey_scope.go:25`）显式映射全部 `ACCESS_API_KEY` 方法为 `{resource, op}`，为单一事实源；`apiKeyScopeAllowed` 匹配、创建时 `ValidAPIKeyScope` 校验（≤32 项、每项 ≤64 字符）。

| 资源 | scope | 服务 | 读 | 写 |
|------|-------|------|----|----|
| `databases` | `databases.read/write` | DatabasesService | List/Get/Count | Create/Update/Delete/Upsert/Bulk |
| `users` | `users.read/write` | UsersService | List/Get | Create/Update/Delete |
| `groups` | `groups.read/write` | GroupsService | List/Get | Create/Update/Delete |
| `storage` | `storage.read/write` | StorageService | List/Get/Usage | Create/Update/Delete |
| `projects` | `projects.read/write` | ProjectsService | List/Get | Create/Update/Delete |
| `oauthproviders` | `oauthproviders.read/write` | OAuthProvidersService | List | Upsert/Delete |
| `apikeys` | `apikeys.read/write` | APIKeysService | List/Get | Create/Delete（但 API key 凭证被上游禁调） |
| `functions` | `functions.read/write` | FunctionsService | List/Get | Create/Update/Delete/SetVariables |
| `payments` | `payments.read/write` | PaymentsService | List/Get | Refund/ManualFulfill |
| `economy` | `economy.read/write` | AssetsService | List/Get | Create/Update/Delete/Grant/Consume/... |
| `subscriptions` | `subscriptions.read/write` | SubscriptionsService | List/Get | Create/Update/Delete/Cancel |
| `billing` | `billing.read` | BillingService | Get/List | — |
| `outbox` | `outbox.read/write` | **OutboxService** | `ListDeadLetters` | `ReplayDeadLetter`（W-J 死信，`proto/server/v1/outbox.proto:43`） |

匹配：裸资源名=`*`/`all` 全量放行；`*.read` 仅读方法；`*.write` 仅写方法；**未登记方法即使 `*` 也 fail-closed**。

**防护**：`IsAPIKeysServiceMethod` 拒绝 API key 调 `APIKeysService`（防自铸提权）；API Key 以 `Roles=["keys", "key:<自身id>"]` 参与用户 collection `_acl`（不默认 bypass；仅 `SystemPrincipal`/平台 admin 绕过）。**默认私有（B14，C6 决议）**：key 创建文档的空 ACE 种子绑 `read/update/delete:key:<自身id>`——其他 key 不可见（Get = NotFound）；跨 key 协作需显式授予对方 `key:<id>` ACE；遗留的 `read:keys`/`write:keys` 显式授予仍共享（存量 keys ACE 保留有效，默认种子不再产生）。详见 `06-databases.md §7`。

---

## 5. gRPC 方法级 authz（`proto/shared/v1/authz.proto:9`）

```proto
enum AccessLevel { ACCESS_PUBLIC=1; ACCESS_AUTHENTICATED=2; ACCESS_PERMISSION=3; ACCESS_API_KEY=4; }
extend MethodOptions  { MethodAuth  method_auth  = 52001; }
extend ServiceOptions { ServiceAuth service_auth = 52002; }
```

- 服务可 `service_auth.default_access` 提供默认，方法未标时回退（`resolveMethodAccess:267`）；
- `ACCESS_AUTHENTICATED` 缺省 `["users"]`，`ACCESS_PERMISSION` 缺 `permissions` 直接报错。

`internal/infra/server/grpc.go:65` 启动期 `collectMethodsByAccess` 聚合四类：

| access | 归集 | 拦截器语义 |
|--------|------|------------|
| `ACCESS_PUBLIC` | `publicMethods` | 可选凭证，必放行 |
| `ACCESS_API_KEY` | `apiKeyMethods` | 须 `api_key` 或 `admin`，走 scope 门禁 |
| `ACCESS_AUTHENTICATED` | `permissionMethods[method]=["users"]` | 已认证 + 默认 users |
| `ACCESS_PERMISSION` | `permissionMethods[method]=perms` | 已认证 + 显式 perms（空则启动失败） |

**守门**：

1. 方法未标且服务无默认 → `missing auth policy` 启动失败；
2. `AssertAPIKeyScopeCoverage(apiKeyMethods)`（`apikey_scope.go:237`）：`apiKeyScopeRules` 必须与 `ACCESS_API_KEY` 方法集完全一致，否则 panic（漏登记写方法会被 `*` 误放）；
3. `AssertAdminRoleWriteCoverage()`（`apikey_scope.go:302`）：`adminRoleMethodRules` 必须覆盖 `apiKeyScopeRules` 全部 `op==write` 方法，且不得含 `read`/未映射方法，否则 viewer 可越权写；
4. `assertRegisteredMethodsHaveAuthz`（`grpc.go:179`）：已注册 gRPC 方法（除 `grpc.health.v1`/`grpc.reflection` 白名单）必须落在三集合之一，否则 `registered grpc methods missing authz annotation`。

---

## 6. adminRoleMethodRules 与纵深防御

`internal/api/interceptor/admin_roles.go:16` 登记 Server API **全部写方法**的允许角色，拦截器 `adminRoleMethodRules[method]` 非空时 `HasAnyRole(perms)`（`jwt.go:131`）：

- `owner,admin`：`APIKeysService`、用户接管面（`UpdateUser/DeleteUser` 等）、Databases schema DDL（`CreateDatabase/Collection/Attribute/Index`）、Functions、`OAuthProviders`、`Projects Create/Delete`、`Payments Refund/ManualFulfill`、`Assets Grant/Consume...`、`OutboxService ReplayDeadLetter`；
- `member,owner,admin`：用户文档 CRUD、Storage 桶/文件、Groups、Projects Update、`Assets` 目录 CRUD 等业务写。

纵深防御四守卫（`internal/app/shared/authz.go`，机制 v8 评审 B1 改名归组；双面共享 use-case 用 `RequireAnyOf` 组合）：

- `RequireServerPrincipal:61`（Databases DDL 等业务写）：放行 `admin` 或 `service`（API Key），匿名/端用户 `PermissionDenied`——绕过拦截器直调 use-case 时仍不可 `SystemPrincipal` 写；
- `RequirePlatformPrincipal:34`（Functions 写、API Key 管理、用户密码/令牌等平台级）：仅 `admin.IsPlatformAdmin`，API Key / 受限 admin 一律拒绝；
- `RequireConsolePrincipal:48`（Console 专属）；
- `RequireEndUser:17`（纯端用户 actor 语义，带项目绑定的判断留在调用面）。

Functions DDL 与 Storage 已对齐 `RequireServerPrincipal` 口径（Databases 组自 Round3 起与 Functions 同口径，API Key 持 `databases.write` 可做 DDL）。

---

## 7. JWT / cookie / 加密工具

| 工具 | 位置 | 要点 |
|------|------|------|
| `jwtparser` | `pkg/jwtparser/jwt.go` + `keys.go` | HS256，`tid/uid/usn/akd/pid/sid/ttp/rls/scp/exp/iat` 短名；`exp`+`iat` 必校验，仅 `HS256`；`PurposeEndUserJWT/AdminJWT/SessionCookie` 三域 `HMAC-SHA256(master,purpose)` 派生，跨域不通用（改 master 即全域失效） |
| Console cookie | `internal/api/consolegrpc/cookies.go` | `TORCHWOOD_session_console`（`Path /`）+ `TORCHWOOD_console_refresh`（`Path /v1/console/auth`），`HttpOnly`+`SameSite=Lax`+`Secure(https)`；refresh 带 rotation + 重用 `RotateMismatch` 撤销；登出 `Max-Age=0` |
| SessionCodec | `internal/infra/auth/session_cookie.go` | `base64url(projectID:sessionID):HMAC-SHA256`，验签后查 `sessions` 集合 |
| `password` | `pkg/password/password.go` | Argon2id `t=3 m=65536 p=4`，`$argon2id$v=19$...`，`ConstantTimeCompare` |
| `secretbox` | `pkg/secretbox/secretbox.go` | `sha256("torchwood-secretbox:"+secret)` → AES-256-GCM，`enc:v1:` 前缀，空透传兼容旧明文；OAuth `client_secret`（`bunrepo/oauth_provider_repo.go:24`）、TOTP `factor.Secret`（`infra/auth/totp.go:52`） |

> 详见 `docs/developer/06-databases.md §3`（`_perms` 历史）与 `§7`（`_acl` 与 `keys`/`key:{id}` 角色）、`03-configuration.md §6.2`（会话 cookie）、`pkg/jwtparser` 源码。
