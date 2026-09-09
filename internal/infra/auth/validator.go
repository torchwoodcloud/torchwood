package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/infra/auth/principalcache"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Validator struct {
	cfg              *config.AppConfig
	apiKeyRepo       projects.APIKeyRepository
	projectRepo      projects.Repository
	adminRepo        projects.AdminRepository
	adminProjectRepo projects.AdminProjectRepository
	adminRevokeStore domainauth.AdminTokenRevokeStore
	sessions         domainauth.SessionRepository
	users            users.Repository
	roleResolver     domainauth.UserRoleResolver
	sessionCodec     *SessionCookieCodec
	oneTimeTokens    domainauth.OneTimeTokenStore
	// execTokens 是函数执行 token 校验端口（P0 执行身份；nil = 未装配，
	// twx_ token 一律拒绝——fail-closed）。
	execTokens domainfunctions.ExecutionTokenService
	// principalCache 是端用户 principal 短 TTL 缓存（P0.5 热路径清账；nil =
	// 直连 DB 实时校验，语义不变）。最坏吊销延迟 = TTL 30s（失效标记主动
	// 失效 + TTL 兜底），取舍见 principalcache 包注释。
	principalCache *principalcache.Cache
}

// SetPrincipalCache 注入端用户 principal 缓存（组合根装配；nil 安全）。
func (v *Validator) SetPrincipalCache(c *principalcache.Cache) {
	v.principalCache = c
}

func NewValidator(
	cfg *config.AppConfig,
	apiKeyRepo projects.APIKeyRepository,
	projectRepo projects.Repository,
	adminRepo projects.AdminRepository,
	adminProjectRepo projects.AdminProjectRepository,
	adminRevokeStore domainauth.AdminTokenRevokeStore,
	sessions domainauth.SessionRepository,
	usersRepo users.Repository,
	roleResolver domainauth.UserRoleResolver,
) *Validator {
	return NewValidatorWithOneTimeTokens(cfg, apiKeyRepo, projectRepo, adminRepo, adminProjectRepo, adminRevokeStore, sessions, usersRepo, roleResolver, nil, nil)
}

// NewValidatorWithOneTimeTokens 额外装配一次性 token 消费存储（CreateJWT
// 签发的一次性 JWT 验证时必须原子消费，防重放；未装配时此类 token 一律拒绝）。
// execTokens 为函数执行 token 服务（P0 执行身份；nil 时 twx_ token 一律拒绝）。
func NewValidatorWithOneTimeTokens(
	cfg *config.AppConfig,
	apiKeyRepo projects.APIKeyRepository,
	projectRepo projects.Repository,
	adminRepo projects.AdminRepository,
	adminProjectRepo projects.AdminProjectRepository,
	adminRevokeStore domainauth.AdminTokenRevokeStore,
	sessions domainauth.SessionRepository,
	usersRepo users.Repository,
	roleResolver domainauth.UserRoleResolver,
	oneTimeTokens domainauth.OneTimeTokenStore,
	execTokens domainfunctions.ExecutionTokenService,
) *Validator {
	return &Validator{
		cfg:              cfg,
		apiKeyRepo:       apiKeyRepo,
		projectRepo:      projectRepo,
		adminRepo:        adminRepo,
		adminProjectRepo: adminProjectRepo,
		adminRevokeStore: adminRevokeStore,
		sessions:         sessions,
		users:            usersRepo,
		roleResolver:     roleResolver,
		sessionCodec:     NewSessionCookieCodec(string(jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeSessionCookie))),
		oneTimeTokens:    oneTimeTokens,
		execTokens:       execTokens,
	}
}

func (v *Validator) ValidateToken(ctx context.Context, token string) (*shared.Principal, error) {
	return v.ValidateCredential(ctx, token, shared.CredentialTypeToken)
}

func (v *Validator) ValidateCredential(ctx context.Context, raw string, credentialType shared.CredentialType) (*shared.Principal, error) {
	switch credentialType {
	case shared.CredentialTypeAPIKey:
		return v.validateAPIKey(ctx, raw)
	case shared.CredentialTypeExecution:
		return v.validateExecutionToken(ctx, raw)
	case shared.CredentialTypeToken:
		claims, ok := v.parseJWT(raw)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
		}
		return v.principalFromJWT(ctx, claims)
	case shared.CredentialTypeSession:
		// Try JWT first (console or token-style session).
		if claims, ok := v.parseJWT(raw); ok {
			return v.principalFromJWT(ctx, claims)
		}
		projectID, sessionID, err := v.sessionCodec.Verify(raw)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid session")
		}
		return v.principalFromSession(ctx, projectID, sessionID)
	}
	return nil, status.Error(codes.Unauthenticated, "unsupported credential type")
}

// parseJWT verifies a token against the purpose-derived sub-keys. A token only
// verifies under the key it was signed with, so trying both domains is safe;
// principalFromJWT then dispatches on the signed ActorKind claim.
func (v *Validator) parseJWT(raw string) (*jwtparser.Claims, bool) {
	secret := v.cfg.GetSecurity().GetJwt().GetSecret()
	if claims, ok := jwtparser.Parse(jwtparser.DeriveKey(secret, jwtparser.PurposeAdminJWT), raw); ok {
		return claims, true
	}
	return jwtparser.Parse(jwtparser.DeriveKey(secret, jwtparser.PurposeEndUserJWT), raw)
}

// ParseClaims 解析并验签 JWT（复用于 Realtime 握手读取 ttp/exp 元数据；
// 校验语义仍以 ValidateCredential 为准）。
func (v *Validator) ParseClaims(raw string) (*jwtparser.Claims, bool) {
	return v.parseJWT(raw)
}

func (v *Validator) validateAPIKey(ctx context.Context, raw string) (*shared.Principal, error) {
	hash := sha256.Sum256([]byte(raw))
	hashStr := hex.EncodeToString(hash[:])
	key, err := v.apiKeyRepo.GetAPIKeyBySecretHash(ctx, hashStr)
	if err != nil {
		return nil, status.Error(codes.Internal, "api key validation failed")
	}
	if key == nil || !key.Enabled {
		return nil, status.Error(codes.Unauthenticated, "invalid or disabled api key")
	}
	if key.ExpireAt != nil && key.ExpireAt.Before(time.Now()) {
		return nil, status.Error(codes.Unauthenticated, "api key expired")
	}
	if v.projectRepo != nil {
		proj, err := v.projectRepo.GetProject(ctx, key.ProjectID)
		if err != nil {
			return nil, status.Error(codes.Internal, "project lookup failed")
		}
		if proj == nil || proj.Status != "active" {
			return nil, status.Error(codes.Unauthenticated, "project is not active")
		}
	}
	return &shared.Principal{
		ActorID:        idgen.ID(key.ID),
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		ProjectID:      key.ProjectID,
		APIKeyID:       key.ID,
		// B14 per-key 角色（C6 决议）：keys 承载 scope/API 面（集合默认权限、
		// 特权授予判定不受影响），key:<id> 承载数据隔离身份——RLS 谓词可见、
		// 可作文档 ACE 授予目标（跨 key 协作需显式授予 key:<id> ACE）。
		// 角色形态取自 DocRole 词表（M6.1 主权），禁止裸串拼接。
		Roles:       []string{databases.RoleKeys, databases.RoleKey(key.ID)},
		Permissions: key.Scopes,
	}, nil
}

// validateExecutionToken 校验函数执行 token 并构造 execution principal
// （P0 执行身份，设计 §1）。Permissions 按 declared_scopes 投影为 API key
// 同款权限串（"<res>.<op>"），scope 门（PolicySet.AllowsAPIKeyTargets）原样
// 生效；Roles 复用 B14 key 族模型（keys + key:function:<id>）——数据面可见
// 性由开发者把集合/桶授予 key:function:<id> 角色决定（scope 过门但角色未
// 授予 = 数据不可见，fail-closed）。
func (v *Validator) validateExecutionToken(ctx context.Context, raw string) (*shared.Principal, error) {
	if v.execTokens == nil {
		return nil, status.Error(codes.Unauthenticated, "execution token validation unavailable")
	}
	info, err := v.execTokens.Validate(ctx, raw)
	if err != nil {
		// Redis 不可用 = 拒绝（fail-closed，设计 Q1 拍板）；
		// Internal 与 401 区分，便于观测是基础设施故障还是凭证无效。
		return nil, status.Error(codes.Internal, "execution token validation failed")
	}
	if info == nil {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired execution token")
	}
	functionID := info.FunctionID
	// 数据面角色形态取自 DocRole 词表（key:function:<id> 经 RoleKey 构造，
	// 禁止裸串拼接——key 段身份为 "function:<function_id>"）。
	roles := []string{databases.RoleKeys, databases.RoleKey("function:" + functionID)}
	return &shared.Principal{
		ActorID:        idgen.ID(functionID),
		ActorKind:      shared.ActorKindExecution,
		CredentialType: shared.CredentialTypeExecution,
		ProjectID:      info.ProjectID,
		FunctionID:     functionID,
		ExecutionID:    info.ExecutionID,
		InvokingUserID: info.InvokingUserID,
		Roles:          roles,
		Permissions:    domainfunctions.DeclaredScopePermissions(info.Scopes),
	}, nil
}

func (v *Validator) principalFromJWT(ctx context.Context, claims *jwtparser.Claims) (*shared.Principal, error) {
	switch claims.ActorKind {
	case "admin":
		if claims.TokenType != "" && claims.TokenType != jwtparser.TokenTypeAccess {
			return nil, status.Error(codes.Unauthenticated, "invalid token type")
		}
		// 撤销判定随行读出（M5 C1）：每请求本来就要取 admins 行读角色，
		// revoked_at 同行带出零额外查询——DB 为事实源，Redis 保留为登出
		// 快路径，判定取两者 max。
		admin, err := v.adminRepo.GetAdmin(ctx, claims.UserID)
		if err != nil {
			return nil, status.Error(codes.Internal, "admin lookup failed")
		}
		if admin == nil {
			return nil, status.Error(codes.Unauthenticated, "admin not found")
		}
		if err := v.checkAdminTokenRevoked(ctx, claims, admin.RevokedAt); err != nil {
			return nil, err
		}
		return &shared.Principal{
			ActorID:         idgen.ID(admin.ID),
			ActorKind:       shared.ActorKindAdmin,
			CredentialType:  shared.CredentialTypeToken,
			IsPlatformAdmin: admin.Role == "owner" || admin.Role == "admin",
			AdminID:         admin.ID,
			Email:           admin.Email,
			Roles:           []string{admin.Role, shared.RoleConsole},
		}, nil
	default:
		if claims.OneTime {
			// 一次性 JWT（CreateJWT 签发）：验证侧原子消费，二次使用或
			// 未装配消费存储一律拒绝（fail-closed），普通 access token 不受影响。
			if v.oneTimeTokens == nil || claims.TokenID == "" {
				return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
			}
			consumed, err := v.oneTimeTokens.Consume(ctx, domainauth.OneTimeJWTKeyPrefix+claims.TokenID)
			if err != nil || consumed == "" {
				return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
			}
		}
		// P0.5 principal 短 TTL 缓存（热路径清账）：命中时一次 Redis 失效
		// 标记检查替代 4 次 DB 往返（session 校验 + users.GetByID ×2 +
		// memberships）。一次性 JWT 不缓存（消费即失效的语义不走本通道）。
		cacheKey := principalcache.Key{}
		if !claims.OneTime && claims.SessionID != "" && claims.ProjectID != "" && v.principalCache != nil {
			cacheKey = principalcache.Key{ProjectID: claims.ProjectID, SessionID: claims.SessionID, IAT: claims.IssuedAt}
			if p := v.principalCache.Get(ctx, cacheKey); p != nil && p.UserID == claims.UserID {
				return p, nil
			}
		}
		if claims.SessionID != "" && claims.ProjectID != "" {
			if err := v.validateEndUserSession(ctx, claims.ProjectID, claims.SessionID, claims.UserID); err != nil {
				return nil, err
			}
		}
		if claims.TokenType != "" && claims.TokenType != jwtparser.TokenTypeAccess {
			return nil, status.Error(codes.Unauthenticated, "invalid token type")
		}
		// ensureUserCanAuthenticate 取回的 user 原样传入角色解析
		// （P0.5：删掉 LoadUserRoles 内的第二次 users.GetByID）。
		user, err := v.ensureUserCanAuthenticate(ctx, claims.ProjectID, claims.UserID)
		if err != nil {
			return nil, err
		}
		roles, err := v.resolveEndUserRoles(ctx, claims.ProjectID, claims.UserID, user)
		if err != nil {
			return nil, err
		}
		p := &shared.Principal{
			ActorID:        idgen.ID(claims.UserID),
			ActorKind:      shared.ActorKindEndUser,
			CredentialType: shared.CredentialTypeToken,
			ProjectID:      claims.ProjectID,
			UserID:         claims.UserID,
			SessionID:      claims.SessionID,
			Email:          claims.Username,
			Roles:          roles,
		}
		if cacheKey.SessionID != "" && v.principalCache != nil {
			v.principalCache.Put(cacheKey, p)
		}
		return p, nil
	}
}

func (v *Validator) principalFromSession(ctx context.Context, projectID, sessionID string) (*shared.Principal, error) {
	if v.sessions == nil {
		return nil, status.Error(codes.Internal, "session lookup failed")
	}
	// P0.5 principal 短 TTL 缓存（session-cookie 路径；无 iat，键 IAT=0）。
	cacheKey := principalcache.Key{ProjectID: projectID, SessionID: sessionID}
	if v.principalCache != nil {
		if p := v.principalCache.Get(ctx, cacheKey); p != nil {
			return p, nil
		}
	}
	sess, err := v.sessions.GetByID(ctx, projectID, sessionID)
	if err != nil {
		return nil, status.Error(codes.Internal, "session lookup failed")
	}
	if sess == nil {
		return nil, status.Error(codes.Unauthenticated, "session not found")
	}
	if sess.ExpireAt.IsZero() || sess.ExpireAt.Before(time.Now()) {
		return nil, status.Error(codes.Unauthenticated, "session expired")
	}
	userID := sess.UserID
	if userID == "" {
		return nil, status.Error(codes.Unauthenticated, "invalid session")
	}
	user, err := v.ensureUserCanAuthenticate(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	roles, err := v.resolveEndUserRoles(ctx, projectID, userID, user)
	if err != nil {
		return nil, err
	}
	p := &shared.Principal{
		ActorID:        idgen.ID(userID),
		ActorKind:      shared.ActorKindEndUser,
		CredentialType: shared.CredentialTypeSession,
		ProjectID:      projectID,
		UserID:         userID,
		SessionID:      sessionID,
		Roles:          roles,
	}
	if v.principalCache != nil {
		v.principalCache.Put(cacheKey, p)
	}
	return p, nil
}

func (v *Validator) validateEndUserSession(ctx context.Context, projectID, sessionID, userID string) error {
	if v.sessions == nil {
		return status.Error(codes.Unauthenticated, "session lookup failed")
	}
	sess, err := v.sessions.GetByID(ctx, projectID, sessionID)
	if err != nil {
		return status.Error(codes.Unauthenticated, "session lookup failed")
	}
	if sess == nil {
		return status.Error(codes.Unauthenticated, "session not found or revoked")
	}
	if sess.UserID != userID {
		return status.Error(codes.Unauthenticated, "invalid session")
	}
	if sess.ExpireAt.IsZero() || sess.ExpireAt.Before(time.Now()) {
		return status.Error(codes.Unauthenticated, "session expired")
	}
	return nil
}

// resolveEndUserRoles 实时解析用户角色；解析失败按拒绝处理（fail-closed），
// 避免 JWT claims 中的旧角色残留。user 是调用方已取回的行（P0.5 清账：
// 免去解析器内的第二次 users.GetByID）。
func (v *Validator) resolveEndUserRoles(ctx context.Context, projectID, userID string, user *users.User) ([]string, error) {
	if v.roleResolver == nil {
		return []string{databases.RoleUsers, databases.RoleUser(userID)}, nil
	}
	resolved, err := v.roleResolver.LoadUserRoles(ctx, projectID, userID, user)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "role resolution failed")
	}
	return resolved, nil
}

// ensureUserCanAuthenticate 校验用户可认证并返回行（P0.5：取回的行供角色
// 解析复用）。
func (v *Validator) ensureUserCanAuthenticate(ctx context.Context, projectID, userID string) (*users.User, error) {
	if projectID == "" || userID == "" {
		return nil, nil
	}
	if v.users == nil {
		return nil, status.Error(codes.Unauthenticated, "user lookup failed")
	}
	found, err := v.users.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "user lookup failed")
	}
	if found == nil {
		return nil, status.Error(codes.Unauthenticated, "user not found")
	}
	if !found.CanAuthenticate() {
		return nil, status.Error(codes.Unauthenticated, "user account is not active")
	}
	return found, nil
}

func (v *Validator) ValidateAdminProjectAccess(ctx context.Context, principal *shared.Principal) error {
	if principal == nil || principal.ActorKind != shared.ActorKindAdmin {
		return nil
	}
	if principal.ProjectID == "" {
		return nil
	}
	if principal.IsPlatformAdmin {
		return nil
	}
	adminID := principal.AdminLookupID()
	if adminID == "" {
		return status.Error(codes.Unauthenticated, "admin context missing")
	}
	has, err := v.adminProjectRepo.HasProjectAccess(ctx, adminID, principal.ProjectID)
	if err != nil {
		return status.Error(codes.Internal, "admin project access check failed")
	}
	if !has {
		return status.Error(codes.PermissionDenied, "admin has no access to project")
	}
	return nil
}

// checkAdminTokenRevoked 判定 admin token 撤销（M5 C1）：DB revoked_at 为
// 事实源（改密/删除等管理动作同事务落库），Redis RevokedBefore 为登出快
// 路径；取两者 max，iat <= max 即拒（与 Redis 原判定同构）。
func (v *Validator) checkAdminTokenRevoked(ctx context.Context, claims *jwtparser.Claims, dbRevokedAt time.Time) error {
	if claims == nil || claims.UserID == "" {
		return nil
	}
	revokedAt := dbRevokedAt
	if v.adminRevokeStore != nil {
		redisRevoked, err := v.adminRevokeStore.RevokedBefore(ctx, claims.UserID)
		if err != nil {
			return err
		}
		if redisRevoked.After(revokedAt) {
			revokedAt = redisRevoked
		}
	}
	if !revokedAt.IsZero() && claims.IssuedAt <= revokedAt.Unix() {
		return status.Error(codes.Unauthenticated, "token revoked")
	}
	return nil
}

//nolint:unused
func parseTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		return time.Parse(time.RFC3339Nano, t)
	}
	return time.Time{}, fmt.Errorf("unsupported time type")
}
