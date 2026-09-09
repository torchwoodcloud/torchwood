# Torchwood 安全审查报告与修复方案

> [!WARNING] 已作废归档（ARCHIVED）
> 归档日期：2026-08-09
> 归档原因：状态「待修复」已不成立——P0-1/P0-2/P0-3、P1-1~P1-3 均已修复落地（系统集合黑名单与 docDB 写保护、inline MIME 白名单 + nosniff、Teams 成员/owner 校验 + 邀请接受邮箱验证、migration 000007/000008、`oauthproviders` 独立 scope）；且本文档与 `security-review-permissions.md` 存在结论冲突（P0-1 关于 Server API 不受影响的结论不成立），已被后者取代。
> 截止归档仍**未修复**的项：P1-4 gRPC TLS 配置支持（默认绑定地址已按建议改为 `127.0.0.1:8088`，TLS 证书配置未实现）；P1-5 改邮箱无验证流程（`UpdateAccount` 仍直接落 email + 置 `email_verified=false`，无 `pending_email` 流程）。
> 后续信息源：`docs/security-review-permissions.md`（已归档）中的实施记录、`docs/roadmap.md`。

> 审查日期：2026-08-07
> 审查范围：Client / Server / Console 三套 API 的划分、认证与授权链路、文档权限模型、Storage、Teams、SDK、Console UI
> 状态：待修复（修复顺序见文末）

---

## 1. 整体评价

### 1.1 划分合理性（结论：总体合理）

| 层 | 结论 | 说明 |
|----|------|------|
| **Client API**（`proto/client`） | ✅ 合理 | 账号全生命周期 + 动态文档 + 团队；`ListDocuments` 走 `ACCESS_PUBLIC` + 文档级 `_perms` 匿名读控制，设计正确 |
| **Server API**（`proto/server`） | ✅ 合理 | 全部 `ACCESS_API_KEY`；所有 handler 的 projectID 一律从 principal 解析（请求体 project_id 无效），杜绝跨项目伪造；API key 禁止调 APIKeys 服务防自铸提权 |
| **Console API**（`proto/console`） | ✅ 合理 | 仅 auth 三件套，其余复用 Server API（`X-Torchwood-Project` 头 + `ValidateAdminProjectAccess`） |
| **TS SDK** | ⚠️ 有缺陷 | client/server 两套 + 3 种认证模式与 proto 对齐，但包入口损坏（见 P2-3） |
| **Console UI** | ✅ 合理 | 无 token 前端（HttpOnly cookie + SameSite=Lax + 单飞 refresh），抗 XSS；服务端才是真鉴权 |

### 1.2 验证无误的安全设计（保留）

- authz 注解 fail-closed + 启动期断言全部注册方法有策略（`internal/infra/server/grpc.go:103-158`）
- JWT 按用途派生密钥（admin / end_user / session-cookie）、强制 HS256、secret 为空拒绝启动（`cmd/server/provides.go:39`）
- Argon2id 密码哈希、API key SHA-256 存储、OTP HMAC 存储（Redis 泄露不可离线爆破）
- refresh token 轮换 + 重用检测 + 会话全量撤销；admin token 撤销机制
- OAuth2 `state` + PKCE + 回调 URL 项目白名单（`internal/app/client/oauth2.go:453-471`）
- OAuth 身份绑定冲突拒绝静默覆盖（`internal/app/client/identity.go:111-122, 162-173`）
- trusted-proxy 严格校验 XFF（`pkg/grpc/interceptor/trusted_proxy.go`）
- 查询 DSL 字段白名单 + 全参数化（`internal/infra/documentdb/postgres.go:1096-1185`）
- 上传 100MiB 限制、文件名头注入防护、SQL 列名白名单 + `_` 前缀字段禁止更新

---

## 2. 漏洞与修复方案

> 严重级别：**P0 高危** / P1 中危 / P2 低危
> 修复方案均给出具体文件与改动要点，验收标准为可测试断言。

---

### P0-1 客户端文档 API 可直写系统集合（认证边界失效）

**级别**：P0 高危（最高优先级）

**位置**：
- `internal/api/clientgrpc/databases.go:27-41`（CreateDocument/UpdateDocument 入口）
- `internal/app/client/databases.go:76-104`（`ensureCollection` 仅校验标识符 + 集合存在 + Disabled）
- `internal/infra/documentdb/system_collection_specs.go:33-44`（users 集合 `create:any`）

**问题**：客户端 `CreateDocument` / `UpdateDocument` 的 `database_id` / `collection_id` 完全由调用者指定，**无系统集合黑名单**。`users` 集合 collection 级权限为 `create:any`，用户还能更新自己 users 文档的任意字段（`buildUpdateParts` 只拦截 `_` 前缀字段，`password_hash` / `email_verified` / `labels` / `status` / `email` 均为合法字段名）。

**影响**：
| 攻击链 | 后果 |
|--------|------|
| `CreateDocument(users)` 插入 `{email: victim@x.com, password_hash: <自算>, email_verified: true}` | 邮箱预注册抢占：受害者无法 SignUp，攻击者可随时以该身份登录 |
| 更新自己文档 `email_verified: true`、`labels: ["..."]` | 自举 `user:xxx/verified`、`label:*` 角色 → 绕过基于 verified/label 的文档权限 |
| 更新自己文档 `password_hash` | 绕过 `UpdateAccount` 的 `old_password` 校验 |
| 把自己 email 改为目标邮箱 + `email_verified: true` | 身份接管：SignIn(目标邮箱) 即获得该身份 |

任意注册用户（**含匿名账号**）可利用，覆盖项目内全部用户数据与认证边界。

**修复方案**（已按独立审查修订，2026-08-07）：

1. **客户端 API 增加系统集合黑名单**（核心修复，第一层）：

   `internal/app/client/databases.go` 新增集合校验，在 `ensureCollection` / `ensureCollectionForProject` 中拒绝系统集合：

   ```go
   // clientSystemCollections 是客户端 API 禁止直接读写的系统集合。
   var clientSystemCollections = map[string]struct{}{
       "users": {}, "sessions": {}, "identities": {},
       "teams": {}, "memberships": {},
       "buckets": {}, "files": {},
   }

   func isClientSystemCollection(collectionID string) bool {
       _, ok := clientSystemCollections[collectionID]
       return ok
   }
   ```

   `ensureCollectionForProject` 在 `GetCollection` 之前增加：

   ```go
   if isClientSystemCollection(collectionID) {
       return "", databases.Principal{}, shared.MapDocumentDBError(databases.ErrPermissionDenied)
   }
   ```

   > 注意：`ensureCollectionForRead`（List/Get/Count）也应同步拒绝系统集合，避免经客户端 API 枚举/读取系统数据。
   >
   > 边界：本黑名单只覆盖 `client.Databases` 一个入口；`server.Databases`（API key）、`storage`、`teams` 等 app 走独立调用链，不受影响（已验证）。

2. **docDB adapter 层系统集合写保护**（纵深防御，第二层，**关键**）：

   黑名单只在 app 层，将来新增任何 app 代码直调 docDB 写 users 即复发。在 `internal/infra/documentdb/postgres.go` 的 `CreateDocument` / `DeleteDocument`（及 `UpdateDocument` 的敏感字段）增加：非 `SystemPrincipal` 时，对 `users` / `sessions` / `identities` 的 create/delete 一律拒绝：

   ```go
   var systemCollectionsWriteProtected = map[string]struct{}{
       "users": {}, "sessions": {}, "identities": {},
   }

   func isWriteProtectedSystemCollection(collectionID string) bool {
       _, ok := systemCollectionsWriteProtected[collectionID]
       return ok
   }
   ```

   `CreateDocument` 在 collection 级 create 检查之前：

   ```go
   if !principal.IsSystem() && isWriteProtectedSystemCollection(collectionID) {
       return doc, ErrPermissionDenied
   }
   ```

   `DeleteDocument` 同理（`checkDocumentPermission` 之前）。

   > 影响面核验：SignUp / OTP 自动注册 / 匿名 / session 创建均以 `SystemPrincipal` 写入（`account.go:193`、`email_otp.go:163`、`anonymous.go:57`、`session_service.go:66`），不受影响。

3. **收紧 users 集合 collection 级权限**（纵深防御，第三层）：

   `internal/infra/documentdb/system_collection_specs.go:33-44` 将 `{Type: "create", Role: "any"}` 改为 `{Type: "create", Role: "keys"}`（与 `sessions` 对齐）。

   > 该集合权限随 `EnsureSystemCollections` 幂等创建（`coll != nil` 即跳过），**已存在的项目不会自动迁移**，须执行迁移脚本（见第 5 步）。

4. **users 文档敏感字段写保护**（纵深防御，第四层）：

   在客户端 `UpdateDocument` 应用层（`internal/app/client/databases.go`）过滤 `_` 前缀之外，再拒绝 `password_hash` / `email_verified` / `labels` / `status` 字段（与 `internal/app/server/users.go:57-61` 的 `userUpdateProtectedFields` 一致）。

   > `increment` map 无需处理：`buildIncrementParts` 只接受数值类型字段，对 `email_verified` / `status` 等布尔/字符串字段会 SQL 类型报错，天然不可用。

5. **存量项目迁移**（随第 3 步落地）：

   先核对 `internal/infra/bun/model` 中集合元数据的实际表名与字段（`DocumentCollection` / `DocumentCollectionPermission`），再按以下模板编写可执行迁移：

   ```sql
   -- 模板：将存量项目 users 集合的 create 权限收窄为 keys
   UPDATE document_collection_permissions
   SET permission_role = 'keys'
   WHERE collection_id = 'users' AND permission_type = 'create' AND permission_role = 'any';
   ```

   验收须包含存量项目回放验证（迁移前先构造一个非 keys 账号确认可插入 users，迁移后确认被拒）。

**验收标准**：
- [ ] 客户端 `POST /v1/databases/default/collections/users/documents` 返回 403/400
- [ ] 客户端对 users/sessions/identities/teams/memberships/buckets/files 的 CRUD 全部被拒
- [ ] 非 `SystemPrincipal` 直调 docDB 对 users/sessions/identities 的 create/delete 被拒（适配器层测试）
- [ ] 已有项目迁移后 users 集合 `create` 权限生效
- [ ] 回归：Server API（API key 路径）、SignUp / OTP / 匿名注册、session 创建不受影响

---

### P0-2 Storage `/view` 同源存储型 XSS

**级别**：P0 高危

**位置**：
- `internal/api/serverhttp/file_handler.go:197-204`（`Content-Type` 客户端可控 + `Content-Disposition: inline`）
- `internal/app/storage/storage.go:162`（MimeType 原样入库）
- `internal/app/storage/storage.go:287-295`（默认文件权限 `read:any`）
- `/v1/*` 响应无 CSP / `X-Content-Type-Options`（CSP 仅作用于 `/console/`，见 `internal/infra/server/console.go:46`）

**问题**：上传时 Content-Type 完全由客户端 multipart 头指定，`/view` 端点对 `text/html`、`image/svg+xml` 以 `inline` 同源输出。攻击者上传恶意 HTML → 诱导已登录 admin 打开链接 → JS 在 admin 会话上下文执行（cookie 自动附带；HttpOnly 只防 token 读取，不防请求代发）→ 可建全 scope API key、删用户、读项目数据。

**修复方案**（已按独立审查修订，2026-08-07；三者取其一，建议全部）：

1. **inline 白名单**：`file_handler.go` 的 `download` 中对 `/view`（inline）只允许安全类型：

   ```go
   var inlineSafeMimeTypes = map[string]struct{}{
       "image/png": {}, "image/jpeg": {}, "image/gif": {}, "image/webp": {},
       "image/avif": {}, "image/svg+xml": {}, // 如保留 svg 需配合 sanitize，见下
       "text/plain": {}, "application/pdf": {},
   }
   ```

   inline 时：`mime` 不在白名单 → 一律 `attachment`；`image/svg+xml` 建议直接降级为 attachment（SVG 可内嵌脚本）。

   > 独立审查补充：白名单**必须包含 `video/*`、`audio/*`**（按前缀匹配，如 `strings.HasPrefix(mime, "video/")`）——客户端 App 依赖 inline 预览视频/音频，且音视频格式不可承载脚本，不加入会造成功能回归。

2. **上传时校验/剥离危险 MIME**：`internal/app/storage/storage.go` `CreateFile` 中对客户端传入的 Content-Type 做白名单归一（或至少对 `text/html`、`application/xhtml+xml`、`application/javascript` 等强制改判为 `application/octet-stream`）。

3. **响应加固**：storage 下载端点（`file_handler.go`）响应头增加：

   ```go
   w.Header().Set("X-Content-Type-Options", "nosniff")
   w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
   ```

   `sandbox` 可使 inline 内容在无脚本环境渲染（合法图片/PDF 渲染不受影响）。

   > 独立审查补充：`X-Content-Type-Options: nosniff` 建议升级为**全局 HTTP 中间件**（`internal/infra/server/grpc_gateway.go` 的 combined handler 外层），覆盖所有 `/v1/*` 响应，而非仅下载端点。

4. **服务端 MIME 嗅探**（可选纵深）：读取文件头若干字节与声明 Content-Type 比对，不一致时按嗅探结果归一。

**验收标准**：
- [ ] 上传 `text/html` 文件，`/view` 返回 `Content-Disposition: attachment` 或渲染被 CSP 沙箱阻断
- [ ] 图片/PDF/视频/音频仍可 inline 预览
- [ ] 下载端点响应含 `X-Content-Type-Options: nosniff`；全局 `/v1/*` 响应含 nosniff

---

### P0-3 Client Teams 授权模型缺陷

**级别**：P0 高危（团队数据完整性 + 垃圾邀请）

**位置**：
- `internal/app/client/teams.go:63-75`（`CreateMembership` 无团队归属校验）
- `internal/app/client/teams.go:103-108`（`DeleteMembership` 无归属/身份校验）
- `internal/app/client/teams.go:55-61`（`DeleteTeam` 无 owner 校验）
- `internal/infra/documentdb/system_collection_specs.go:157-217`（teams `read:any` / update·delete `team:{id}`；memberships `create:users` / update·delete `team:{id}`）
- `internal/app/client/user_roles.go:43-68`（任何 accepted 成员自动获得 `team:<id>` 角色）

**问题**：
- 任何认证用户可向**任意团队**创建成员邀请（teams `read:any` + memberships `create:users`，无团队归属校验）→ 垃圾邀请/邮箱轰炸
- 任何团队成员（持有 `team:<id>` 角色）可删除团队内**任意**成员（含 owner）——`DeleteMembership` 无自校验
- 任何团队成员可删除**整个团队**（teams `delete:team:{id}`）
- 客户端 `CreateMembership` 可直接指定 `roles`（如 `owner`），目前仅为标签，未来扩展角色权限时是隐患
- 🆕 **邀请接受链无邮箱验证**（独立审查新发现）：`SignUp` 无需验证邮箱即可注册成功（`account.go:127`）→ 攻击者 SignUp `victim@x.com`（未验证）→ `UpdateMembershipStatus` 仅比对 `memEmail == 调用者 email` 字符串（`client/teams.go:97`）→ **攻击者可接受发往该邮箱的任何团队邀请**，以 victim 身份进入团队

**修复方案**（已按独立审查修订，2026-08-07）：

1. **客户端路径的团队管理操作要求成员身份**：`CreateMembership` / `DeleteMembership` / `DeleteTeam` 在 `internal/app/client/teams.go` 增加调用 `t.teams.ListAcceptedTeamRoles`（或新端口 `TeamMembershipResolver`），校验调用者是否是该团队成员：

   ```go
   roles, err := t.teams.ListAcceptedTeamRoles(ctx, projectID, userID)
   if err != nil { return err }
   teamRole := fmt.Sprintf("team:%s", teamID)
   if !slices.Contains(roles, teamRole) {
       return status.Error(codes.PermissionDenied, "not a member of this team")
   }
   ```

   > 注：每次操作多一次全量 membership 查询（按 user_id + status 过滤，有索引），性能代价可接受；如高频可后续加缓存。

2. **owner 语义落地**：成员删除/团队删除仅允许 `team:<id>/owner` 角色。**注意**：`ListAcceptedTeamRoles`（`app/server/teams.go:332-354`）当前只返回 `team:<id>` 与 `member:<id>`，**不含** `team:<id>/owner`（该角色仅在 `user_roles.go:62-66` 的 JWT 载荷中生成）——需扩展该方法，额外返回原始 membership roles（`team:<id>/<role>`），方案 1 与 2 共用同一数据源。

3. **`DeleteMembership` 增加身份校验**：与 `UpdateMembershipStatus` 一致，`memUserID != userID && memEmail != email` 时拒绝（`internal/app/client/teams.go:95-99` 已有范本）。

4. **`CreateMembership` 拒绝授予 owner 角色**（客户端路径）：`roles` 仅允许 `member`。

5. **🆕 邀请接受要求邮箱已验证**（堵住 SignUp 未验证注册 + 邀请劫持链）：

   `UpdateMembershipStatus`（`internal/app/client/teams.go:86-101`）在放行前读取调用者 users 文档的 `email_verified`（`SystemPrincipal` 查询），未验证则拒绝接受邀请（`codes.FailedPrecondition`）；或要求 membership 的 `user_id` 已与调用者绑定（即用户须先走验证流程）。

   > 该修复同时缓解 SignUp 邮箱抢占的放大面（P0-1 修复后仅剩此入口可被未验证邮箱利用）。

**验收标准**：
- [ ] 非团队成员调用 CreateMembership/DeleteMembership/DeleteTeam 被拒
- [ ] 普通成员删除他人 membership / 删除团队被拒；owner 放行
- [ ] 客户端邀请无法授予 `owner` 角色
- [ ] 🆕 未验证邮箱用户接受团队邀请被拒；验证后放行

---

### P1-1 系统集合权限迁移脚本

**级别**：P1（随 P0-1 落地）

**位置**：`db/migrations/`（新增）

**问题**：P0-1 中 users 集合 `create:any` 的收窄只对新建项目生效，存量项目需显式迁移。

**修复方案**：见 P0-1 修复方案第 5 步（已合并：核对 bun model 元数据表结构 → 按模板编写迁移 → 存量项目回放验证）。

**验收标准**：
- [ ] 存量项目执行迁移后，匿名/客户端用户对 users 集合的 create 被拒
- [ ] Server API（keys/admin）不受影响

---

### P1-2 OAuth 端用户会话 cookie 的 Secure 标志依赖 r.TLS

**级别**：P1 中危

**位置**：`internal/api/serverhttp/oauth_handler.go:63`

**问题**：`Secure: r.TLS != nil` 在反向代理 TLS 终结部署下恒为 false → cookie 无 Secure 标志，HTTP 明文环境可被中间人窃取。与 console cookie 基于 `public_url` 配置判断不一致（`internal/app/console/auth.go:158-160`）。

**修复方案**：与 console 对齐，改为基于配置：

```go
// OAuthHandler 构造时注入 secureCookies 判断
secure := strings.HasPrefix(cfg.GetServer().GetHttp().GetPublicUrl(), "https://")
http.SetCookie(w, &http.Cookie{
    ...
    Secure:   secure,
})
```

> 独立审查补充：`console.Auth.SecureCookies()`（`internal/app/console/auth.go:158-160`）与本判断逻辑重复，建议抽为公共 helper（如 `pkg/securecookies` 或 `config` 上的方法），两端共用，避免今后两处不一致。

**验收标准**：
- [ ] `public_url` 为 https 时（即使代理后 r.TLS 为 nil）cookie 带 Secure
- [ ] 单元测试覆盖两种配置

---

### P1-3 OAuth Providers 归入 `projects` scope（scope 粒度过粗）

**级别**：P1 中危

**位置**：`pkg/grpc/interceptor/apikey_scope.go:39-42`

**问题**：`OAuthProviders` 服务被映射到 `projects` scope → 持有 `projects` scope 的 API key 可改写项目 OAuth client_id/secret，把该项目全部第三方登录引向攻击者自己的 client（劫持登录）。

**修复方案**：

1. 映射独立化：

   ```go
   case strings.Contains(svc, "OAuthProviders"):
       return "oauthproviders"
   ```

   > 独立审查补充：`apiKeyScopeResource` 目前用**子串匹配**（`strings.Contains`），建议一并改为精确服务名匹配（`strings.HasSuffix` 或显式 switch），当前 `"OAuthProviders"` 与 `"Projects"` 子串虽无冲突，但该写法脆弱，新增服务名时易误配。

2. 服务端将 OAuth provider 写操作从「projects scope 可管理」改为「仅 admin 或专用 scope」：`proto/server/v1/oauth_providers.proto` 的 `UpsertOAuthProvider` / `DeleteOAuthProvider` 增加方法级注解或独立 scope 常量，并在 `internal/app/server/oauth_providers.go` 校验。

3. 存量 API key 如需保留，seed 中为 default key 补 `oauthproviders` scope（`cmd/seed/main.go:88`）。

   > 影响面核验：Console UI 经 admin session 调 OAuth provider 配置（`console/src/api/oauthProviders.ts`），不受 scope 收紧影响。

**验收标准**：
- [ ] `projects` scope 的 key 无法读写 OAuth provider 配置
- [ ] 新增 `oauthproviders` scope 的 key 可以
- [ ] 回归：projects CRUD 不受影响

---

### P1-4 gRPC 端口明文无 TLS

**级别**：P1 中危（部署配置）

**位置**：`internal/infra/server/grpc.go:78-87`（lynxgrpc.NewServer 无凭据）；`internal/infra/server/grpc_gateway.go:48`（内环 insecure）

**问题**：`server.grpc.addr: :8088` 默认绑定所有接口且无 TLS → 生产若暴露该端口，Bearer token / x-api-key 明文可被嗅探。

**修复方案**（已按独立审查核验可行性，2026-08-07）：
- **可行**：lynx v1.2.0 的 `lynxgrpc` 提供 `WithTLSConfig(*tls.Config)`（`server/grpc/server.go:116-122`，内部经 `credentials.NewTLS` 装配），`NewGRPCServer` 中当配置了证书时使用之
- 配置项：`server.grpc.tls_cert_file` / `tls_key_file`；有证书时：

  ```go
  cert, err := tls.LoadX509KeyPair(certFile, keyFile)
  grpcOpts = append(grpcOpts, lynxgrpc.WithTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}}))
  ```

- 内环 gateway → gRPC 的 dial（`grpc_gateway.go:48`）在有 TLS 配置时改用 `credentials.NewTLS`，否则维持 `insecure`（仅限 127.0.0.1 回环）
- **至少（推荐默认）**：`configs/config.yaml` 将 `server.grpc.addr` 默认改为 `127.0.0.1:8088`（网关同机回环已足够），并由外层反代暴露 HTTP；文档明确「gRPC 端口必须内网隔离或前置 TLS」

**验收标准**：
- [ ] 配置 TLS 后 gRPC 握手为 TLS；内环 dial 同步启用
- [ ] 无 TLS 配置时默认绑定 127.0.0.1，文档有明确部署警告

---

### P1-5 UpdateAccount 改邮箱无验证（邮箱抢占）

**级别**：P1 中危

**位置**：`internal/app/client/account.go:380-393`

**问题**：已登录用户可把 email 改为任意未注册邮箱（不校验新邮箱所有权，仅置 `email_verified=false`）→ 抢占任意未注册邮箱。

**修复方案**：
- 新邮箱变更走验证流程：`email` 变更时置 `pending_email` 字段 + 发送验证邮件（复用 `internal/app/client/verification.go` 的 token 机制），验证通过后才落 `email`
- 或最低限度：新邮箱与旧邮箱同为已验证状态才允许直接变更

> 独立审查补充：`pending_email` 生效期间，SignIn / OTP / recovery 仍以**旧邮箱**为准，验证链接须绑定 `project_id + user_id + pending_email` 三要素，防止交叉会话误验。

**验收标准**：
- [ ] 改邮箱后需通过新邮箱验证链接才能生效
- [ ] 验证前原邮箱登录不受影响
- [ ] 验证链接绑定 user_id + pending_email，其他用户/邮箱不可复用

---

### P2 低危 / 注意项

| # | 项目 | 位置 | 说明与处理 |
|---|------|------|-----------|
| P2-1 | 默认集合权限开放 | `internal/domain/databases/permissions.go:14-29` | 新建集合默认 `read:any`（匿名可读全部）+ `update:users/delete:users`（docSecurity=false 时任意认证用户可改删任意文档）。产品决策：保持 Appwrite 兼容，但须在文档与 Console 新建集合 UI 中明示风险，并建议默认 `documentSecurity: true` |
| P2-2 | OAuth fragment 携带 access_token | `internal/app/client/oauth2.go:426-440` | token 进浏览器历史、可能经 Referer 外泄。已有 HttpOnly session cookie，建议移除 fragment token（SDK 无 cookie 场景改走 token 端点） |
| P2-3 | TS SDK 包入口损坏 | `sdk/typescript/src/index.ts:1` | 导出不存在的 `./torchwood.js`（实现在门面文件，当时文件名仍沿用项目旧名），无法编译发布。修正为门面实际路径并补构建验证 |
| P2-4 | SDK demo localStorage 存 token | `sdk/demo/src/lib/storage.ts` | 官方示例引导不安全实践；建议改内存/sessionStorage 并注释风险 |
| P2-5 | seed 硬编码凭据 | `cmd/seed/main.go:48,81` | admin 密码 `Admin@123` + 明文打印 API key；生产误跑即失守。加环境变量覆盖 + 明确"仅限开发"输出 |
| P2-6 | SignUp 无速率限制 | `internal/app/client/account.go:127` | 注册风暴 + 放大 P0-1。按 IP 增加与匿名会话一致的限流（`anonymous.go:17-21` 范本） |
| P2-7 | 分页 token 无签名/TTL | `pkg/crud/pagination.go:48-57` | 可篡改 offset，但因每次查询重新套 perms filter，无越权影响；后续改用 cursor 模式（`EncodeCursorPageToken`） |
| P2-8 | metrics 端口无鉴权 | `configs/config.yaml:15` | `:9100` 无鉴权；部署时内网隔离或加鉴权 |
| P2-9 | 项目 ID 由名称派生 | `internal/app/server/projects.go:38` | 仅空格替换，特殊字符进入 S3 key；加 `^[a-z0-9-]+$` 白名单校验 |
| P2-10 | 匿名会话可用 P0-1 | `internal/app/client/anonymous.go` | 匿名用户即认证用户，P0-1 修复后自动消除该放大面 |

---

## 3. 影响范围汇总

| 漏洞 | 利用者 | 前置条件 | 影响面 |
|------|--------|----------|--------|
| **P0-1 系统集合可写** | 任何注册/匿名用户 | 项目开启客户端 DB API（默认） | **全用户体系**：身份伪造、特权自举、邮箱抢占、绕过密码策略 |
| **P0-2 Storage 同源 XSS** | 任何注册用户 | 受害者点击 `/view` 链接 | admin 会话上下文任意操作 |
| **P0-3 Teams 授权缺陷** | 任何注册用户（含未验证邮箱） | 无 | 任意团队垃圾邀请；成员可删任意成员/整队；未验证邮箱可接受他人邀请（🆕） |
| **P1-2 OAuth cookie Secure** | 网络攻击者 | TLS 终结代理部署 + `public_url` 非 https | 会话 cookie 明文可窃取 |
| **P1-3 OAuthProviders scope** | 持有 `projects` scope 的 API key | 无 | 改写 client_secret → 劫持项目全部第三方登录 |
| **P1-4 gRPC 明文** | 网络攻击者 | gRPC 端口暴露公网 | 全部凭据可嗅探 |
| **P1-5 改邮箱无验证** | 已登录用户 | 无 | 邮箱抢占 |
| **P2-1 默认集合权限** | 匿名（读）/ 任何认证用户（写） | 新建集合未显式收紧 | 新集合数据匿名可读、认证用户可改删 |

> 注：P0-3 影响范围已按独立审查补充「邀请接受链无邮箱验证」；P0-1/P0-2/P0-3/P1 各项的修复方案均已按独立审查修订（见对应章节标注）。

## 4. 修复顺序建议

1. **P0-1 系统集合隔离**（认证边界，一票否决级）→ 2. **P0-2 存储 XSS** → 3. **P0-3 Teams 授权**（含邀请接受链修复）→ 4. **P1-1 存量迁移**（随 P0-1）→ 5. **P1-2/P1-3/P1-4/P1-5** → 6. **P2 各项**（可与上述并行，SDK 包入口 P2-3 建议尽快顺手修）

---

## 5. 修复方案独立审查（2026-08-07）

> 对本文档第 2 节修复方案的第二轮独立审查，结论已回写进对应方案。本节记录审查结论与新增发现。

### 5.1 审查结论摘要

| 方案 | 结论 | 修正内容 |
|------|------|----------|
| P0-1 系统集合隔离 | ✅ 成立，需补纵深 | 新增 docDB 层写保护（第二层）、迁移模板细节、increment 无需处理说明；已验证 SignUp/OTP/匿名/session 均走 `SystemPrincipal` 无回归 |
| P0-2 Storage XSS | ✅ 成立，需补白名单 | 白名单补 `video/*`、`audio/*`（防功能回归）；nosniff 升级为全局中间件 |
| P0-3 Teams 授权 | ⚠️ 需修订后执行 | 补「邀请接受链无邮箱验证」攻击链（🆕）；`ListAcceptedTeamRoles` 需扩展返回原始 membership roles 才能落地 owner 校验 |
| P1-1 迁移脚本 | ✅ 成立 | 与 P0-1 第 5 步合并去重 |
| P1-2 Secure cookie | ✅ 成立 | 建议抽公共 helper 避免逻辑重复 |
| P1-3 scope 独立 | ✅ 成立 | 建议子串匹配改精确匹配；已核验 Console UI 走 admin session 不受影响 |
| P1-4 gRPC TLS | ✅ 成立 | 已验证 lynx v1.2.0 `lynxgrpc.WithTLSConfig` 可行；补充内环 dial 与默认 127.0.0.1 |
| P1-5 改邮箱验证 | ✅ 成立 | 补充 pending_email 期间旧邮箱语义与验证链接绑定要素 |
| P2 各项 | ✅ 成立 | P2-7 已确认无越权（每查询重套 perms filter） |

### 5.2 独立审查新增发现（原报告未收录）

**审计盲区：认证失败请求无审计记录**（低-中危，合规视角）

- **位置**：`internal/infra/server/grpc.go:82-87`，拦截器顺序为 clientInfo → **auth** → audit
- **问题**：认证失败（如暴力破解尝试）在 `UnaryAuthMiddleware` 直接 `return`（`pkg/grpc/interceptor/jwt.go:76-148`），**不产生 audit 条目**，仅有 slog 告警日志。登录审计是常见合规要求（记录谁何时尝试登录、失败多少次）。
- **修复方案**（任选其一）：
  1. 将 audit interceptor 前移到 auth interceptor 之前（审计全量请求含认证失败）
  2. 在 `UnaryAuthMiddleware` 拒绝路径同步写审计条目（复用 `internal/domain/audit` 端口，Action = fullMethod，Status = `Unauthenticated`/`PermissionDenied`）
- **验收标准**：
  - [ ] 错误口令登录产生 audit 记录（含 IP、UA、actor 信息为空）
  - [ ] 成功请求审计不受影响
