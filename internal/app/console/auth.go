package console

import (
	"context"
	"strings"
	"sync"
	"time"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"github.com/torchwooddev/torchwood/pkg/password"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type Auth struct {
	cfg              *config.AppConfig
	adminRepo        projects.AdminRepository
	adminRevokeStore domainauth.AdminTokenRevokeStore
	loginThrottle    domainauth.LoginThrottle
	rotation         domainauth.RefreshRotationStore
}

func NewAuth(cfg *config.AppConfig, adminRepo projects.AdminRepository, adminRevokeStore domainauth.AdminTokenRevokeStore, loginThrottle domainauth.LoginThrottle, rotation domainauth.RefreshRotationStore) *Auth {
	return &Auth{cfg: cfg, adminRepo: adminRepo, adminRevokeStore: adminRevokeStore, loginThrottle: loginThrottle, rotation: rotation}
}

type SignInCommand struct {
	Email    string
	Password string
}

type RefreshTokenCommand struct {
	RefreshToken string
}

type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	// RefreshTokenID is the jti of RefreshToken; used for rotation bookkeeping
	// and never mapped to proto responses.
	RefreshTokenID string
}

func (a *Auth) SignIn(ctx context.Context, cmd SignInCommand) (*TokenPair, error) {
	clientInfo := contexts.ClientInfoFrom(ctx)
	throttleEmail := strings.ToLower(strings.TrimSpace(cmd.Email))
	if err := a.checkLoginThrottle(ctx, throttleEmail, clientInfo.IP); err != nil {
		return nil, err
	}
	invalidCredentials := func() (*TokenPair, error) {
		a.recordLoginFailure(ctx, throttleEmail, clientInfo.IP)
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	admin, err := a.adminRepo.GetAdminByEmail(ctx, cmd.Email)
	if err != nil {
		return nil, status.Error(codes.Internal, "admin lookup failed")
	}
	if admin == nil {
		// 哑哈希时序均衡（P2-10，对齐 client SignIn）：admin 不存在时也执行
		// 一次同价 Argon2 校验，消除按响应时延枚举已注册 admin 邮箱的侧信道。
		_, _ = password.Verify(cmd.Password, consoleDummyPasswordHash())
		return invalidCredentials()
	}
	if ok, _ := password.Verify(cmd.Password, admin.PasswordHash); !ok {
		return invalidCredentials()
	}
	a.resetLoginThrottle(ctx, throttleEmail, clientInfo.IP)
	return a.issueAdminTokens(ctx, admin.ID, admin.Email, admin.Role)
}

// consoleDummyPasswordHash 是 console 登录的固定哑哈希（sync.OnceValue 惰性
// 生成 + init 预热，语义与 app/client 的 dummyPasswordHash 一致）。
var consoleDummyPasswordHash = sync.OnceValue(func() string {
	h, err := password.Hash("torchwood-dummy-console-signin-password")
	if err != nil {
		return ""
	}
	return h
})

func init() { consoleDummyPasswordHash() }

func (a *Auth) RefreshToken(ctx context.Context, cmd RefreshTokenCommand) (*TokenPair, error) {
	if cmd.RefreshToken == "" {
		return nil, status.Error(codes.InvalidArgument, "refresh_token is required")
	}
	claims, ok := jwtparser.Parse(jwtparser.DeriveKey(a.cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), cmd.RefreshToken)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	if claims.TokenType != jwtparser.TokenTypeRefresh || claims.ActorKind != "admin" {
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	// 以库内记录为准：已删除账号不得续签，角色变更立即生效（不用 JWT 快照）。
	// 撤销判定随行读出（M5 C1）：DB revoked_at 为事实源，Redis 为登出快
	// 路径，取两者 max——改密/降级后的旧 refresh token 不得换取新对。
	admin, err := a.adminRepo.GetAdmin(ctx, claims.UserID)
	if err != nil {
		return nil, status.Error(codes.Internal, "admin lookup failed")
	}
	if admin == nil {
		return nil, status.Error(codes.Unauthenticated, "invalid refresh token")
	}
	if err := a.checkAdminTokenRevoked(ctx, claims, admin.RevokedAt); err != nil {
		return nil, err
	}
	if a.rotation == nil {
		return a.issueAdminTokens(ctx, admin.ID, admin.Email, admin.Role)
	}

	refreshTTL := a.refreshTTL()
	newRefreshTokenID := idgen.UUID().String()
	result, err := a.rotation.Rotate(ctx, domainauth.RefreshRotationKey("admin", claims.UserID), claims.TokenID, newRefreshTokenID, refreshTTL)
	if err != nil {
		return nil, err
	}
	switch result {
	case domainauth.RotateOK:
		return a.issueAdminTokensWithRefreshID(ctx, admin.ID, admin.Email, admin.Role, newRefreshTokenID)
	case domainauth.RotateMismatch:
		// 旧 refresh token 被再次使用：判定为重用，撤销该 admin 此前签发的全部 token。
		if a.adminRevokeStore != nil {
			_ = a.adminRevokeStore.RevokeBefore(ctx, claims.UserID, time.Now(), refreshTTL)
		}
		return nil, status.Error(codes.Unauthenticated, "refresh token reuse detected")
	default: // RotateMissing
		return nil, status.Error(codes.Unauthenticated, "session expired")
	}
}

func (a *Auth) SignOut(ctx context.Context) error {
	if a.adminRevokeStore == nil {
		return nil
	}
	adminID := ""
	if p, ok := contexts.Principal(ctx); ok && p.ActorKind == shared.ActorKindAdmin {
		adminID = p.AdminLookupID()
	}
	if adminID == "" {
		// No principal (e.g. the access token already expired): fall back to the
		// raw credential in the request metadata and revoke best-effort.
		adminID = a.adminIDFromMetadata(ctx)
	}
	if adminID == "" {
		return nil
	}
	return a.adminRevokeStore.RevokeBefore(ctx, adminID, time.Now(), a.refreshTTL())
}

func (a *Auth) refreshTTL() time.Duration {
	refreshTTL := 7 * 24 * time.Hour
	if d, err := time.ParseDuration(a.cfg.GetSecurity().GetJwt().GetRefreshTtl()); err == nil {
		refreshTTL = d
	}
	return refreshTTL
}

// AccessTTL returns the configured admin access token lifetime; used by the
// transport layer for the session cookie Max-Age. Defaults to 1h and is capped
// at 1h (P3-1: console admin session TTL收紧≤1h，cookie Max-Age同步).
func (a *Auth) AccessTTL() time.Duration {
	accessTTL := time.Hour
	if d, err := time.ParseDuration(a.cfg.GetSecurity().GetJwt().GetAccessTtl()); err == nil {
		accessTTL = d
	}
	if accessTTL > time.Hour {
		accessTTL = time.Hour
	}
	if accessTTL <= 0 {
		accessTTL = time.Hour
	}
	return accessTTL
}

// RefreshTTL returns the configured admin refresh token lifetime; used by the
// transport layer for the refresh cookie Max-Age.
func (a *Auth) RefreshTTL() time.Duration {
	return a.refreshTTL()
}

// SecureCookies reports whether console session cookies must carry the Secure
// attribute; true only when the public HTTP endpoint is served over TLS.
func (a *Auth) SecureCookies() bool {
	return strings.HasPrefix(a.cfg.GetServer().GetHttp().GetPublicUrl(), "https://")
}

// adminIDFromMetadata extracts the admin id from the bearer token or console
// session cookie in the request metadata, tolerating expired tokens while still
// verifying the signature.
func (a *Auth) adminIDFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	secret := jwtparser.DeriveKey(a.cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT)
	if raw := metadataValue(md, "authorization"); raw != "" {
		if parts := strings.Fields(raw); len(parts) == 2 && strings.EqualFold(parts[0], "bearer") {
			if claims, ok := jwtparser.ParseAllowExpired(secret, parts[1]); ok && claims.ActorKind == "admin" {
				return claims.UserID
			}
		}
	}
	if raw := metadataValue(md, "cookie"); raw != "" {
		for _, part := range strings.Split(raw, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(part), "=")
			if !found || value == "" || name != "TORCHWOOD_session_console" {
				continue
			}
			if claims, ok := jwtparser.ParseAllowExpired(secret, value); ok && claims.ActorKind == "admin" {
				return claims.UserID
			}
		}
	}
	return ""
}

func metadataValue(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}

func (a *Auth) checkLoginThrottle(ctx context.Context, email, ip string) error {
	if a.loginThrottle == nil {
		return nil
	}
	return a.loginThrottle.Check(ctx, domainauth.LoginNamespaceAdmin, email, ip)
}

func (a *Auth) recordLoginFailure(ctx context.Context, email, ip string) {
	if a.loginThrottle == nil {
		return
	}
	_ = a.loginThrottle.RecordFailure(ctx, domainauth.LoginNamespaceAdmin, email, ip)
}

func (a *Auth) resetLoginThrottle(ctx context.Context, email, ip string) {
	if a.loginThrottle == nil {
		return
	}
	_ = a.loginThrottle.Reset(ctx, domainauth.LoginNamespaceAdmin, email, ip)
}

// checkAdminTokenRevoked 判定 admin token 撤销（M5 C1）：DB revoked_at 为
// 事实源（改密/删除等管理动作同事务落库），Redis RevokedBefore 为登出快
// 路径；取两者 max，iat <= max 即拒。
func (a *Auth) checkAdminTokenRevoked(ctx context.Context, claims *jwtparser.Claims, dbRevokedAt time.Time) error {
	if claims == nil || claims.UserID == "" {
		return nil
	}
	revokedAt := dbRevokedAt
	if a.adminRevokeStore != nil {
		redisRevoked, err := a.adminRevokeStore.RevokedBefore(ctx, claims.UserID)
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

func (a *Auth) issueAdminTokens(ctx context.Context, adminID, email, role string) (*TokenPair, error) {
	return a.issueAdminTokensWithRefreshID(ctx, adminID, email, role, idgen.UUID().String())
}

func (a *Auth) issueAdminTokensWithRefreshID(ctx context.Context, adminID, email, role, refreshTokenID string) (*TokenPair, error) {
	accessTTL := time.Hour
	if d, err := time.ParseDuration(a.cfg.GetSecurity().GetJwt().GetAccessTtl()); err == nil {
		accessTTL = d
	}
	if accessTTL > time.Hour {
		accessTTL = time.Hour
	}
	if accessTTL <= 0 {
		accessTTL = time.Hour
	}
	refreshTTL := a.refreshTTL()
	now := time.Now()
	accessClaims := jwtparser.Claims{
		TokenID:   idgen.UUID().String(),
		UserID:    adminID,
		Username:  email,
		ActorKind: "admin",
		Roles:     []string{role},
		TokenType: jwtparser.TokenTypeAccess,
		ExpiresAt: now.Add(accessTTL).Unix(),
		IssuedAt:  now.Unix(),
	}
	adminKey := jwtparser.DeriveKey(a.cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT)
	accessToken, err := jwtparser.Generate(adminKey, accessClaims)
	if err != nil {
		return nil, err
	}
	refreshClaims := accessClaims
	refreshClaims.TokenID = refreshTokenID
	refreshClaims.TokenType = jwtparser.TokenTypeRefresh
	refreshClaims.ExpiresAt = now.Add(refreshTTL).Unix()
	refreshToken, err := jwtparser.Generate(adminKey, refreshClaims)
	if err != nil {
		return nil, err
	}
	if a.rotation != nil {
		if err := a.rotation.Register(ctx, domainauth.RefreshRotationKey("admin", adminID), refreshTokenID, refreshTTL); err != nil {
			return nil, err
		}
	}
	return &TokenPair{
		AccessToken:    accessToken,
		RefreshToken:   refreshToken,
		ExpiresAt:      accessClaims.ExpiresAt,
		RefreshTokenID: refreshTokenID,
	}, nil
}
