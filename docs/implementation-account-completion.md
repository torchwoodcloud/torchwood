# Torchwood Client Account 补齐实现方案

> 状态：**已实现**（2026-08-09 验收通过：4 项缺口全部落地，全量测试/gofmt/vet/wire 全绿；
> 5 项实现偏差经裁决全部接受）
> 目标读者：维护者与后续扩展
> 关联：`docs/roadmap.md` §2.1（Client Account / Auth）、`AGENTS.md`（开发约定，必读）
> 参考：`docs/implementation-functions-executor.md`（上一轮同类方案的流程与格式）

---

## 1. 目标与验收标准

补齐 Client Account 的 4 项缺口：**Magic URL 登录、一次性 JWT 签发、账号日志查询、
完整 MFA（TOTP 因子管理 + 登录二次验证挑战流）**，并同步 roadmap 状态（匿名登录、
邮箱验证、密码找回经核实**已实现**，仅文档滞后）。

**验收标准（roadmap §2.1 沿用 + 本方案新增）**：

1. `POST /v1/account/sessions/magic-url` 发送带 secret 的登录链接邮件；`PUT .../magic-url`
   验证后签发会话；链接一次性、1h 过期；用户不存在时防枚举（不报错、不发信差异）。
2. `POST /v1/account/jwt` 用当前会话换取 5 分钟 TTL 的一次性 JWT；未登录 401。
3. `GET /v1/account/logs` 返回当前用户最近操作日志（action/status/ip/user_agent/时间，
   按时间倒序，limit 默认 50 上限 100）。
4. MFA 完整流程：
   - `POST /v1/account/mfa/totp` 创建 TOTP 因子（返回 secret + otpauth URL，状态 pending）；
   - `PUT /v1/account/mfa/totp` 提交验证码激活因子（status → verified）；
   - `GET /v1/account/mfa` 列出因子（**不含 secret**）；
   - `DELETE /v1/account/mfa/{factor_id}` 删除因子；
   - **登录钩子**：用户存在 verified 因子时，所有登录方式（SignUp/SignIn/EmailOTP/
     PhoneOTP/Anonymous/OAuth/WeChat/MagicURL）不直接签发会话，登录响应返回
     `mfa_required=true` + `challenge_token` + 因子列表；客户端调
     `POST /v1/account/mfa/challenge` 提交 {challenge_token, factor_id, code} 验证通过后
     才获得会话；challenge_token 5min 一次性，验证失败返回 `Unauthenticated`。
5. 所有新增端点均有 proto authz 注解、gRPC handler、use-case 单元测试；
   `go test ./...`、`task lint`、`task build` 全绿。

---

## 2. 现状盘点（调研结论）

### 2.1 已实现（roadmap 标记滞后，仅需更新文档）

| 功能 | 证据 |
|---|---|
| 匿名登录 | proto `CreateAnonymousSession`（account.proto:106-109）+ use-case `anonymous.go:27`（IP 频控 20 次/h）+ 测试 |
| 邮箱验证 | proto `CreateVerification/UpdateVerification`（:121-129）+ `verification.go:31/92`（24h token）+ 集成测试 |
| 密码找回 | proto `CreateRecovery/UpdateRecovery`（:131-139）+ `recovery.go:29/88`（1h token、防枚举、改密删会话）+ 测试 |
| OAuth2/微信/EmailOTP/PhoneOTP/会话/prefs | 全部完整（25 个 RPC 与 handler 100% 对齐） |

### 2.2 四项缺口与现有资产

| 缺口 | 现有资产 | 缺口 |
|---|---|---|
| **Magic URL** | `AccountTokenStore`（Redis 一次性 token：verification 24h / recovery 1h，32 字节 secret + SHA-256 哈希）、`Mailer`（真实 SMTP，未配置时 dev 日志）、`buildAccountActionURL`（verification.go:132）、OAuth redirect 白名单 `validateProjectOAuthRedirectURLs`（oauth2.go:453） | 无 proto 端点、无 token purpose、无 use-case |
| **JWT 签发** | `jwtparser.Generate/Parse` + `PurposeEndUserJWT` 派生 key（keys.go:10-14）、`SessionService.IssueTokens`、refresh 轮换+重用检测 | 无 `POST /v1/account/jwt` 端点与 use-case |
| **账号日志** | 全局 `AuditInterceptor`（pkg/grpc/interceptor/audit.go:22-64）自动记录全部 /v1/account/* → `audit_logs` 表（000002/000004 迁移，含 project/actor 索引）+ `audit.Repository.Insert` | `audit.Repository` 无查询方法；无 `GET /v1/account/logs`；`clientgrpc/account.go` 各 handler 未设 `WithAuditResource` |
| **MFA** | `sessions` 集合 `factors` json 属性占位（system_collection_specs.go:53）、`pkg/secretbox`（AES-256-GCM 加密）、会话创建统一入口 `finishSignInWithProvider`（email_otp.go:181） | TOTP 库、proto/domain/infra/use-case 全无；users 无 factors 字段；登录钩子无 |

### 2.3 会话创建统一入口（MFA 钩子关键）

所有登录路径最终收敛到 `internal/app/client/email_otp.go:181` 的
`finishSignInWithProvider(ctx, projectID, user, provider)`（返回 user/tokens/cookie/err）：
SignUp 与 SignIn → `account.go:209 finishSignIn` → 此函数；OAuth → oauth2.go:325；
匿名 → anonymous.go:61；PhoneOTP → phone_otp.go:127；WeChat → wechat.go:59。
**MFA 登录钩子只需改造此一处**（含返回签名扩展），全部路径自动生效。

---

## 3. 数据模型

### 3.1 users 集合加 `factors` 字段（documentdb 系统集合）

`internal/infra/documentdb/system_collection_specs.go` 的 users spec（:16-43）增加：

```go
{ID: "users_factors", Key: "factors", Type: "json"},
```

factors 值为 JSON 数组，元素结构：

```json
{
  "id": "fac_<uuid>",
  "type": "totp",
  "secret": "enc:v1:<AES-256-GCM 密文>",   // secretbox.Encrypt(明文, jwt secret)
  "status": "pending",                      // pending → verified
  "created_at": "<RFC3339>"
}
```

- TOTP secret **必须加密存储**（`pkg/secretbox`，key 用 `cfg.Security.Jwt.Secret`；
  secret 为空时 `CreateTOTPFactor` 返回 `Internal` "mfa secret is not configured"）。
- 因子列表读写走现有 `docDB.GetDocument/UpdateDocument`（users 集合 update 权限
  `user:{id}` owner 已有，end-user 自助路径可用）。
- 不回显明文 secret：所有读路径（ListFactors）仅返回元数据。

### 3.2 无新增 SQL 表

- MFA challenge token 存 Redis（新增 `mfa_challenge_redis.go`）。
- 账号日志复用现有 `audit_logs` 表。

---

## 4. 端口设计

### 4.1 `internal/domain/auth/account_token.go`（扩展）

新增 purpose 常量与接口方法（复用现有 createToken/verifyToken 实现模式）：

```go
const AccountTokenPurposeMagicURL = "magic_url"

type AccountTokenStore interface {
    // ...现有 4 个方法不动...
    CreateMagicURLToken(ctx context.Context, projectID, userID, email string) (secret string, expireAt time.Time, err error)
    VerifyMagicURLToken(ctx context.Context, projectID, userID, secret string) error
}
```

TTL：**1h**（与 recovery 一致）。`internal/infra/auth/account_token_redis.go` 实现
（内部复用现有 `createToken/verifyToken` 帮助函数，purpose 传入 `magic_url`）。

### 4.2 `internal/domain/auth/mfa.go`（新建）

```go
package auth

// Factor 是用户的 MFA 因子（持久化在 users 文档 factors 字段）。
type Factor struct {
    ID        string
    Type      string // "totp"
    Secret    string // 加密后的密文（enc:v1:...）
    Status    string // "pending" | "verified"
    CreatedAt time.Time
}

// FactorStatus 常量。
const (
    FactorStatusPending  = "pending"
    FactorStatusVerified = "verified"
)

// MFAService 是 TOTP 因子管理 + 登录挑战端口。
type MFAService interface {
    // CreateTOTPFactor 生成 TOTP secret 与 otpauth URL（issuer=project name、account=email）。
    CreateTOTPFactor(ctx context.Context, projectID, userID, email string) (*Factor, string, string, error) // factor, plainSecret, otpauthURL
    // VerifyTOTPFactor 校验 code 并激活因子（防重放：同一 code 60s 内不可重用）。
    VerifyTOTPFactor(ctx context.Context, factor *Factor, code string) error
    // ValidateTOTP 校验 code（登录挑战用，不做状态变更）。
    ValidateTOTP(ctx context.Context, factor *Factor, code string) error
}

// MFAChallengeStore 存登录挑战 token（Redis，5min TTL，一次性消费）。
type MFAChallengeStore interface {
    Create(ctx context.Context, projectID, userID string) (token string, expireAt time.Time, err error)
    // Consume 一次性取出并删除；不存在/已用返回错误。
    Consume(ctx context.Context, token string) (projectID, userID string, err error)
}
```

### 4.3 `internal/domain/audit/audit.go`（扩展）

```go
type Repository interface {
    Insert(ctx context.Context, entry *Entry) error
    // ListByActor 返回某项目下指定 actor 的日志（created_at DESC，limit ≤ 100）。
    ListByActor(ctx context.Context, projectID, actorID string, limit int) ([]Entry, error)
}
```

`internal/infra/bun/bunrepo/audit_repo.go` 实现（bun Select，`al.project_id = ? AND
al.actor_id = ?`，`ORDER BY al.created_at DESC LIMIT ?`）。

### 4.4 `internal/domain/auth/session.go`（扩展）

```go
const ProviderMagicURL = "magic_url"
```

（Provider 常量集 :6-12 增加；匿名/邮箱等已有。）

---

## 5. proto 设计（`proto/client/v1/account.proto` 扩展）

service `AccountService`（现有 :13-140）追加 9 个 RPC；消息追加如下。**所有 RPC 必须带
`google.api.http` 与 `method_auth` 注解**（否则 `collectMethodsByAccess` fail-closed）。

```proto
  // MFA
  rpc ListFactors(ListFactorsRequest) returns (ListFactorsResponse) {
    option (google.api.http) = { get: "/v1/account/mfa" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PERMISSION, permissions: ["users"] };
  }
  rpc CreateTOTPFactor(CreateTOTPFactorRequest) returns (TOTPFactor) {
    option (google.api.http) = { post: "/v1/account/mfa/totp", body: "*" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PERMISSION, permissions: ["users"] };
  }
  rpc VerifyTOTPFactor(VerifyTOTPFactorRequest) returns (Factor) {
    option (google.api.http) = { put: "/v1/account/mfa/totp", body: "*" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PERMISSION, permissions: ["users"] };
  }
  rpc DeleteFactor(DeleteFactorRequest) returns (shared.v1.Empty) {
    option (google.api.http) = { delete: "/v1/account/mfa/{factor_id}" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PERMISSION, permissions: ["users"] };
  }
  rpc CreateMFASession(CreateMFASessionRequest) returns (SignInResponse) {
    option (google.api.http) = { post: "/v1/account/mfa/challenge", body: "*" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PUBLIC };
  }

  // 一次性 JWT
  rpc CreateJWT(CreateJWTRequest) returns (CreateJWTResponse) {
    option (google.api.http) = { post: "/v1/account/jwt", body: "*" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PERMISSION, permissions: ["users"] };
  }

  // Magic URL
  rpc CreateMagicURLSession(CreateMagicURLSessionRequest) returns (ChallengeResponse) {
    option (google.api.http) = { post: "/v1/account/sessions/magic-url", body: "*" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PUBLIC };
  }
  rpc UpdateMagicURLSession(UpdateMagicURLSessionRequest) returns (SignInResponse) {
    option (google.api.http) = { put: "/v1/account/sessions/magic-url", body: "*" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PUBLIC };
  }

  // 账号日志
  rpc ListLogs(ListLogsRequest) returns (ListLogsResponse) {
    option (google.api.http) = { get: "/v1/account/logs" };
    option (torchwood.shared.v1.method_auth) = { access: ACCESS_PERMISSION, permissions: ["users"] };
  }
```

消息定义（空请求消息参照现有 `ListSessionsRequest` 模式；`shared.v1.Empty` 需
`import "shared/v1/common.proto"`——确认现有 imports，若无则添加）：

```proto
// ---- MFA ----
message Factor {
  string id = 1;
  string type = 2;      // totp
  string status = 3;    // pending / verified
  google.protobuf.Timestamp created_at = 4;
}
message ListFactorsRequest {}
message ListFactorsResponse { repeated Factor factors = 1; }
message CreateTOTPFactorRequest {}
message TOTPFactor {
  Factor factor = 1;
  string secret = 2;        // 仅创建响应返回明文 secret
  string otpauth_url = 3;   // otpauth://totp/{issuer}:{email}?secret=...&issuer=...
}
message VerifyTOTPFactorRequest { string factor_id = 1; string code = 2; }
message DeleteFactorRequest { string factor_id = 1; }
message CreateMFASessionRequest {
  string project_id = 1;
  string challenge_token = 2;
  string factor_id = 3;
  string code = 4;
}

// 登录响应扩展（SignUpResponse / SignInResponse 同步追加；proto3 新增字段对
// 现有 SDK 向后兼容）
message SignInResponse {
  Account account = 1;
  TokenBundle tokens = 2;
  bool mfa_required = 3;        // 用户有 verified 因子且未完成挑战时 true
  string challenge_token = 4;   // mfa_required 时返回（5min 一次性）
  repeated Factor factors = 5;  // mfa_required 时返回（不含 secret）
}

// ---- JWT ----
message CreateJWTRequest {}
message CreateJWTResponse { string token = 1; }

// ---- Magic URL ----
message CreateMagicURLSessionRequest {
  string project_id = 1;
  string email = 2;
  string url = 3;   // 登录成功后的回调地址（白名单校验）
}
message UpdateMagicURLSessionRequest {
  string project_id = 1;
  string user_id = 2;
  string secret = 3;
}

// ---- Logs ----
message ListLogsRequest { int32 limit = 1; }
message LogEntry {
  string id = 1;
  string action = 2;        // gRPC 方法名（如 /torchwood.client.v1.AccountService/SignIn）
  string status = 3;
  string resource_id = 4;
  string ip = 5;
  string user_agent = 6;
  google.protobuf.Timestamp created_at = 7;
}
message ListLogsResponse { repeated LogEntry logs = 1; }
```

生成：`task generate-proto`（buf 自动纳入）。生成后如
`File_client_v1_account_proto` 已注册（现有 account 已注册则无需改
`grpc.go` 的 collectMethodsByAccess 列表）。

---

## 6. 分层实现规格

### 6.1 infra：TOTP 与挑战存储（新建）

- **依赖**：`go get github.com/pquerna/otp@latest`（totp 包）。
- `internal/infra/auth/totp.go`（新建，实现 `domainauth.MFAService`）：
  - `CreateTOTPFactor`：`totp.Generate(totp.GenerateOpts{Issuer: projectName, AccountName: email, Period: 30, Digits: 6, Algorithm: otp.AlgorithmSHA1})`
    → 返回 `*Factor{Secret: secretbox.Encrypt(secret, jwtSecret)}` + 明文 secret + otpauth URL。
  - `VerifyTOTPFactor`：`totp.ValidateCustom(code, plainSecret, time.Now(), totp.ValidateOpts{Period: 30, Skew: 1, Digits: 6, Algorithm: otp.AlgorithmSHA1})`
    → 防重放：Redis 记录 `mfa:used-code:{sha256(secret)}:{step}` 60s TTL，命中则拒绝
    （复用 `internal/infra/clients/database.go` 的 `NewRedis`；可内嵌于本文件）。
  - `ValidateTOTP`：同上但不写重放记录（challenge_token 一次性已防重放，见 6.3）。
  - 构造：`NewTOTPService(cfg, redis) domainauth.MFAService`。
- `internal/infra/auth/mfa_challenge_redis.go`（新建，实现 `domainauth.MFAChallengeStore`）：
  - `Create`：随机 32 字节 token（hex）→ `SET mfa:challenge:{token} {projectID}:{userID} EX 300`。
  - `Consume`：`GETDEL`（原子），不存在 → `Unauthenticated` "invalid or expired challenge"。

### 6.2 app：MFA 因子管理（`internal/app/client/mfa.go` 新建）

```go
func (a *Account) ListFactors(ctx, projectID, userID) ([]domainauth.Factor, error)
func (a *Account) CreateTOTPFactor(ctx, projectID, userID, email) (*Factor, plainSecret, otpauthURL, error)
func (a *Account) VerifyTOTPFactor(ctx, projectID, userID, factorID, code) (*Factor, error)
func (a *Account) DeleteFactor(ctx, projectID, userID, factorID) error
func (a *Account) CompleteMFASession(ctx, projectID, challengeToken, factorID, code) (*User, *TokenBundle, string, error)
```

- 因子读写：`requireUser`（account.go:538 现有）取 users 文档 → `doc.Data["factors"]`
  json 数组解析/更新 → `UpdateDocument`（owner 权限）。
- `CreateTOTPFactor`：pending 因子 **10 分钟内必须激活**（`created_at + 10min < now`
  时 `VerifyTOTPFactor` 拒绝并删除该因子）；`CreateTOTPFactor` 前删除同类型 pending
  过期因子。
- `VerifyTOTPFactor`：pending → 校验 code → `verified`；已 verified 因子不可重复激活
  （InvalidArgument）。
- `DeleteFactor`：删除后该用户无 verified 因子 → 登录恢复直通。
- 防枚举：`VerifyTOTPFactor` 失败 5 次锁定该因子 15min（Redis 计数，可复用
  `internal/infra/auth/ratelimit_redis.go` 的 RateLimiter 或独立实现）。

### 6.3 MFA 登录钩子（核心，改 `email_otp.go:181`）

`finishSignInWithProvider` 改造为：

```go
// 返回值新增 mfa *MFASignInChallenge（nil 表示无需挑战）
func (a *Account) finishSignInWithProvider(ctx, projectID string, user *User, provider string) (*User, *TokenBundle, string, *MFASignInChallenge, error)
```

逻辑：

1. 读 users 文档 factors；存在 `verified` 因子时：
2. 生成 challenge：`a.mfaChallenges.Create(ctx, projectID, user.ID)` → token；
3. 返回 `mfa = &MFASignInChallenge{Token: token, Factors: verifiedFactors 元数据}`，
   **不创建会话**（跳过 `CreateSessionAndTokens`）；
4. 无 verified 因子：原逻辑签发会话。

`MFASignInChallenge` 定义（`internal/app/client/mfa.go`）：

```go
type MFASignInChallenge struct {
    Token   string
    Factors []domainauth.Factor
}
```

调用方适配（SignUp/SignIn/OAuth/Anonymous/PhoneOTP/WeChat/MagicURL handler 或 use-case
返回处）：`finishSignInWithProvider` 返回 mfa 非 nil 时，handler 映射
`SignInResponse{mfa_required: true, challenge_token, factors}`（account/tokens 为空）。

**`CompleteMFASession`（CreateMFASession use-case）**：

1. `mfaChallenges.Consume(ctx, token)` → (projectID, userID)（一次性，防重放）；
2. `requireUser` → 校验 factorID 属于该用户且 verified；
3. `mfa.ValidateTOTP(ctx, factor, code)`（challenge 已一次性，此处不做 code 重放记录）；
4. 通过 → `sessions.CreateSessionAndTokens(ctx, projectID, userID, email, ProviderMFA)`；
5. 返回 user/tokens/cookie（映射 SignInResponse）。

`ProviderMFA = "mfa"` 加入 session.go Provider 常量。

### 6.4 Magic URL（`internal/app/client/magic_url.go` 新建）

```go
func (a *Account) CreateMagicURLSession(ctx, projectID, email, callbackURL string) (*ChallengeResponse, error)
func (a *Account) UpdateMagicURLSession(ctx, projectID, userID, secret string) (*User, *TokenBundle, string, *MFASignInChallenge, error)
```

- `CreateMagicURLSession`：
  1. `normalizeEmail`；项目就绪检查（`ensureProjectReady`，参照 anonymous.go:27）；
  2. 查用户：不存在或为匿名占位邮箱 → **返回 nil 错误 + 空 challenge_id 响应**（防枚举，
     与 recovery.go:66-73 一致）；无 tokens/mailer 配置 → `Unimplemented`（同 recovery:30）；
  3. `a.tokens.CreateMagicURLToken(ctx, projectID, user.ID, email)`（1h）；
  4. `buildAccountActionURL(ctx, projectID, callbackURL, user.ID, secret)`（复用
     verification.go:132；callbackURL 需白名单校验，复用 oauth2.go:453
     `validateProjectOAuthRedirectURLs` 语义）；
  5. `a.mailer.Send(ctx, email, "Sign in to Torchwood", 正文含完整 URL)`；
  6. 返回 `ChallengeResponse{challenge_id: secret, expire_at}`（与 email-otp 同构）。
- `UpdateMagicURLSession`：
  1. `VerifyMagicURLToken(ctx, projectID, userID, secret)`（失败 → `Unauthenticated`）；
  2. `requireUser`（按 userID 读文档；用户不存在 → `Unauthenticated`）；
  3. 走 `finishSignInWithProvider(ctx, projectID, user, ProviderMagicURL)`（MFA 钩子自动生效）。

### 6.5 一次性 JWT（`internal/app/client/jwt.go` 新建）

```go
const oneTimeJWTTTL = 5 * time.Minute

func (a *Account) CreateJWT(ctx, projectID, userID string) (string, error) {
    // 1. requireUser；2. roles := a.roles.LoadUserRoles(ctx, projectID, userID)
    // 3. claims := jwtparser.Claims{UserID, ProjectID, Username: email,
    //    ActorKind: "end_user", TokenType: jwtparser.TokenTypeAccess,
    //    Roles: roles, IssuedAt: now, ExpiresAt: now+5min}
    // 4. token, err := jwtparser.Generate(jwtparser.DeriveKey(jwtSecret, PurposeEndUserJWT), claims)
    // 5. 返回 token
}
```

- 密钥与现有 end-user token 同一派生 key（`PurposeEndUserJWT`），保证 validator 可验证。
- TTL 5 分钟；token 绑定项目与用户。
- 注意：`jwtparser.Generate` 默认 15min——传显式 `ExpiresAt` 覆盖。

### 6.6 账号日志（`internal/app/client/logs.go` 新建）

```go
const maxLogsLimit = 100

func (a *Account) ListLogs(ctx, projectID, userID string, limit int32) ([]audit.Entry, error)
```

- limit 归一化：<=0 → 50；>100 → 100。
- 调用 `a.auditRepo.ListByActor(ctx, projectID, userID, int(limit))`。
- `Account` 结构体新增 `auditRepo audit.Repository` 字段（构造注入，Wire 自动）。
- 说明：PUBLIC 登录方法（SignIn 等）的审计记录 ActorID 为空（Principal 未建立），
  ListLogs 只见有 actor 的记录；如需补记，可在登录 use-case 成功后由
  `finishSignInWithProvider` 内插一条 actor 化的审计（可选增强，不在 MVP 强制范围）。

### 6.7 gRPC handler（`internal/api/clientgrpc/account.go` 扩展）

- 新增 9 个 handler，全部先取 projectID（`contexts.Principal`）再调 use-case；
  PERMISSION["users"] 方法可加 `contexts.WithAuditResource`（顺手补齐现有 account
  handler 缺失的 resource_id，见 §7 问题 2）。
- 登录类 handler（SignUp/SignIn/EmailOTP/PhoneOTP/Anonymous/OAuth/WeChat/MagicURL）
  返回值映射：`finishSignInWithProvider` 的 mfa 挑战 → `SignInResponse{mfa_required,
  challenge_token, factors}`；无挑战 → 原逻辑。
- `mapFactor`（domainauth.Factor → proto Factor）。

### 6.8 Console

**不需要改动**（Client Account 是纯 Server API，由 SDK 消费；Console 是管理后台）。

---

## 7. 实现顺序（建议）

| 步骤 | 内容 | 验证 |
|------|------|------|
| 1 | proto 扩展 + `task generate-proto` + `go build ./...` | 编译通过 |
| 2 | domain 端口：account_token（magic_url）、mfa.go、session（ProviderMagicURL/ProviderMFA）、audit ListByActor | `go vet ./...` |
| 3 | infra：TOTP（含重放）、mfa_challenge_redis、account_token_redis 扩展、audit_repo ListByActor | infra 单测 |
| 4 | app：mfa.go（因子管理 + 钩子改造 + CompleteMFASession）、magic_url.go、jwt.go、logs.go；`finishSignInWithProvider` 签名扩展 + 全部调用点适配 | `go test ./internal/app/client/...` |
| 5 | handler 9 个 + 登录响应映射 | `go build ./...` |
| 6 | wire-all（Account 构造新增 mfa/auditRepo 依赖） | wire_gen 检查 |
| 7 | 测试补齐 + 全量验证 | 见 §8 |

每步完成跑 `gofmt -l .`（必须空）+ `go vet ./...`。

---

## 8. 测试与验证

- **app 单测**（mock docDB/sessions/tokens/mailer/rateLimiter，参照现有
  `internal/app/client/*_test.go` 的 `NewTestAccount` + miniredis 模式）：
  - magic_url：创建（用户存在/不存在防枚举/占位邮箱拒绝/白名单 URL 拒绝/无 mailer
    Unimplemented）、确认（有效 secret 签发/MFA 钩子触发/错误 secret 拒绝/过期拒绝）。
  - jwt：签发格式（可被 `jwtparser.Parse` 验证、TTL 5min、roles 附带）、未登录拒绝。
  - logs：按 actor 查询、limit 归一化、仅本用户数据。
  - mfa：创建因子（secret 加密落库/otpauth URL 格式）、pending 激活、错误 code 拒绝、
    失败 5 次锁定、删除、List 不含明文 secret。
  - **钩子**：用户有 verified 因子时 SignIn 返回 mfa_required 且不产生会话文档；
    `CompleteMFASession` 成功后签发会话；challenge_token 二次使用拒绝；错误 code 拒绝。
- **infra 单测**：TOTP（生成/验证/重放拒绝，miniredis）、mfa_challenge（创建/消费
  一次/过期）、account_token_redis magic_url purpose、audit_repo ListByActor（集成）。
- **集成测试**：`internal/app/client/mfa_integration_test.go`（真实 Postgres + miniredis）：
  注册 → 创建因子 → 激活 → 登出 → 登录（mfa_required）→ challenge 完成 → 会话可用。
- **全量验证**：`go test ./...`（.env 提供 TORCHWOOD_TEST_DATABASE_SOURCE）、
  `task lint`、`task build`。
- CI 无需改动（无 Docker 依赖）。

---

## 9. 范围外（明确不做）

- TOTP 之外的其他因子（SMS/Email OTP 作为 MFA 因子、WebAuthn/Passkey）。
- 会话级 MFA 状态标记（sessions.factors 字段暂不使用——因子归属用户而非会话）。
- 账号日志的自定义过滤（按 action/时间范围过滤仅做 limit 分页）。
- MFA 恢复码 / 一次性备用码。
- 二维码图片生成（SDK 端根据 otpauth_url 自渲染）。
- 匿名账号升级为正式账号（邮箱绑定）。
- 登录审计补记 actor（PUBLIC 方法审计无 actor，见 §6.6 可选增强）。

---

## 10. 关键坑（实现时必须注意）

1. **authz fail-closed**：9 个新 RPC 必须带 `method_auth` 注解（MFA 管理类
   `ACCESS_PERMISSION["users"]`、挑战/Magic URL `ACCESS_PUBLIC`），否则
   `assertRegisteredMethodsHaveAuthz` 启动失败。
2. **`finishSignInWithProvider` 签名变更**：全部调用点（account.go:209/264、
   oauth2.go:325、anonymous.go:61、phone_otp.go:127、wechat.go:59）必须同步适配，
   编译期会强制发现，但别漏 handler 层的 mfa 响应映射。
3. **TOTP secret 加密**：明文绝不落库/回显；`jwt secret` 未配置时 CreateTOTPFactor
   返回 Internal（fail-closed）。
4. **challenge_token 一次性**：`GETDEL` 原子消费；Consume 失败一律 `Unauthenticated`
   （不区分无效/过期/已用，防探测）。
5. **防枚举**：Magic URL 创建与 recovery 一致（用户不存在返回空成功）；
   VerifyTOTPFactor 失败锁定（5 次/15min）。
6. **验证码重放**：VerifyTOTPFactor 用 Redis 记录 `used-code`（60s）；
   CompleteMFASession 依赖 challenge 一次性（不额外记录）。
7. **users 文档并发写**：factors 更新为读-改-写（GetDocument → 修改 → UpdateDocument），
   两个并发请求可能互相覆盖；MVP 接受（文档级事务超出范围），但 UpdateDocument 失败
   必须回滚内存状态。
8. **响应向后兼容**：`SignInResponse` 新增字段是 proto3 追加，现有 SDK 不受影响；
   `mfa_required=false` 时 challenge_token/factors 必须为空。
9. **JWT TTL**：`jwtparser.Generate` 默认 15min，CreateJWT 必须显式传 5min
   `ExpiresAt`（§6.5）。
10. **limit 上限**：ListLogs limit ≤ 100（数据库层也加 LIMIT 兜底）。
11. **pquerna/otp 版本**：`go get github.com/pquerna/otp@latest` 后 `go mod tidy`；
    ValidateOpts 的 Period/Digits/Algorithm 必须与 GenerateOpts 一致。
12. **provider 常量**：新增 `ProviderMagicURL`/`ProviderMFA` 写入 sessions 文档
    provider 字段；ListSessions 展示时按原样透传（无需改映射）。
