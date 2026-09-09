package auth

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/auth/principalcache"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/idgen"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const defaultSessionTTL = 7 * 24 * time.Hour

// 短时会话（M5 C2，评审 B-1 补偿控制）：CreateUserToken 服务端登录桥接签发的
// 会话目标 TTL 15min；硬上限 1h 是包级常量——access/refresh TTL 与会话
// expireAt 一并被封顶，security.jwt.* 配置无法突破。
const (
	CreateUserTokenSessionTTL    = 15 * time.Minute
	CreateUserTokenMaxSessionTTL = time.Hour
)

// defaultMaxSessionsPerUser 是 security.sessions.max_per_user 未配置（0）时的
// 单用户会话上限默认值。
const defaultMaxSessionsPerUser = 50

// SessionService implements domainauth.SessionService.
type SessionService struct {
	cfg          *config.AppConfig
	sessions     domainauth.SessionRepository
	sessionCodec *SessionCookieCodec
	roles        domainauth.UserRoleResolver
	rotation     domainauth.RefreshRotationStore
	// principalCache 是端用户 principal 缓存（P0.5；登出/封禁删除会话时写
	// 失效标记——nil 安全，见 SetPrincipalCache）。
	principalCache *principalcache.Cache
}

// SetPrincipalCache 注入 principal 缓存（组合根装配；nil 安全）。
func (s *SessionService) SetPrincipalCache(c *principalcache.Cache) {
	s.principalCache = c
}

func NewSessionService(
	cfg *config.AppConfig,
	sessions domainauth.SessionRepository,
	roles domainauth.UserRoleResolver,
	rotation domainauth.RefreshRotationStore,
) *SessionService {
	return &SessionService{
		cfg:          cfg,
		sessions:     sessions,
		sessionCodec: NewSessionCookieCodec(string(jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeSessionCookie))),
		roles:        roles,
		rotation:     rotation,
	}
}

func (s *SessionService) CreateSessionAndTokens(ctx context.Context, projectID, userID, email, provider string) (*domainauth.TokenBundle, string, error) {
	sessionID, err := s.insertSession(ctx, projectID, userID, provider, defaultSessionTTL)
	if err != nil {
		return nil, "", err
	}
	return s.IssueTokens(ctx, projectID, userID, email, sessionID)
}

// CreateShortLivedSessionAndTokens 签发短时会话（M5 C2）：会话 15min；refresh
// TTL 收敛为 min(配置值, 1h 硬上限)，access TTL 同样被 1h 硬上限封死——
// 模拟登录铸造的凭证集合整体不超过 1h，刷新续命受会话 expireAt 二次门控。
func (s *SessionService) CreateShortLivedSessionAndTokens(ctx context.Context, projectID, userID, email, provider string) (*domainauth.TokenBundle, string, error) {
	ttl := CreateUserTokenSessionTTL
	if ttl > CreateUserTokenMaxSessionTTL {
		// 防御未来调整目标值越过硬上限。
		ttl = CreateUserTokenMaxSessionTTL
	}
	sessionID, err := s.insertSession(ctx, projectID, userID, provider, ttl)
	if err != nil {
		return nil, "", err
	}
	refreshTTL := s.refreshTTL()
	if refreshTTL > CreateUserTokenMaxSessionTTL {
		refreshTTL = CreateUserTokenMaxSessionTTL
	}
	return s.issueTokensWithCaps(ctx, projectID, userID, email, sessionID, idgen.UUID().String(), refreshTTL, CreateUserTokenMaxSessionTTL)
}

// insertSession 落会话行（含单用户会话上限淘汰，R05-P1-6）并返回会话 ID。
func (s *SessionService) insertSession(ctx context.Context, projectID, userID, provider string, sessionTTL time.Duration) (string, error) {
	if provider == "" {
		provider = domainauth.ProviderEmail
	}
	client := contexts.ClientInfoFrom(ctx)

	// R05-P1-6：会话数量上限（security.sessions.max_per_user，未配置/0 = 默认
	// 50；-1 = 不限）。超限时先淘汰最旧会话（按 expire_at 升序）再创建，
	// 保证并发登录不越界。
	if max := s.maxSessionsPerUser(); max > 0 {
		if err := s.evictOldestSessions(ctx, projectID, userID, max); err != nil {
			return "", err
		}
	}

	sessionID := idgen.UUID().String()
	// UUID 高熵，可用无盐 SHA-256（HashOTP）。
	sessionSecret := idgen.UUID().String()
	if err := s.sessions.Insert(ctx, projectID, &domainauth.Session{
		ID:         sessionID,
		UserID:     userID,
		SecretHash: HashOTP(sessionSecret),
		Provider:   provider,
		UserAgent:  client.UserAgent,
		IP:         client.IP,
		ExpireAt:   time.Now().Add(sessionTTL),
	}); err != nil {
		return "", err
	}
	return sessionID, nil
}

func (s *SessionService) IssueTokens(ctx context.Context, projectID, userID, email, sessionID string) (*domainauth.TokenBundle, string, error) {
	return s.IssueTokensWithRefreshID(ctx, projectID, userID, email, sessionID, idgen.UUID().String())
}

func (s *SessionService) IssueTokensWithRefreshID(ctx context.Context, projectID, userID, email, sessionID, refreshTokenID string) (*domainauth.TokenBundle, string, error) {
	return s.issueTokensWithCaps(ctx, projectID, userID, email, sessionID, refreshTokenID, s.refreshTTL(), 0)
}

// issueTokensWithCaps 签发 access/refresh 对：refreshTTL 显式传入；accessCap
// > 0 时 access TTL 被硬上限封顶（短时会话路径防配置突破，M5 C2）。
func (s *SessionService) issueTokensWithCaps(ctx context.Context, projectID, userID, email, sessionID, refreshTokenID string, refreshTTL, accessCap time.Duration) (*domainauth.TokenBundle, string, error) {
	accessTTL := 15 * time.Minute
	if d, err := time.ParseDuration(s.cfg.GetSecurity().GetJwt().GetAccessTtl()); err == nil {
		accessTTL = d
	}
	if accessCap > 0 && accessTTL > accessCap {
		accessTTL = accessCap
	}

	now := time.Now()
	// 签发路径手头无 users 行，传 nil 由解析器兜底单查（查询次数与修复前
	// 一致；热路径的省查在 validator 侧完成）。
	baseRoles, err := s.roles.LoadUserRoles(ctx, projectID, userID, nil)
	if err != nil {
		return nil, "", err
	}
	// B2：模拟登录可区分——若调用方为 admin，写入 imp 字段（impersonator admin id）。
	var impersonator string
	if p, ok := contexts.Principal(ctx); ok && p.ActorKind == shared.ActorKindAdmin && p.AdminLookupID() != "" {
		impersonator = p.AdminLookupID()
	}
	accessClaims := jwtparser.Claims{
		TokenID:   idgen.UUID().String(),
		UserID:    userID,
		Username:  email,
		ActorKind: "end_user",
		ProjectID: projectID,
		SessionID: sessionID,
		TokenType: jwtparser.TokenTypeAccess,
		Roles:     baseRoles,
		Imp:       impersonator,
		ExpiresAt: now.Add(accessTTL).Unix(),
		IssuedAt:  now.Unix(),
	}
	endUserKey := jwtparser.DeriveKey(s.cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeEndUserJWT)
	accessToken, err := jwtparser.Generate(endUserKey, accessClaims)
	if err != nil {
		return nil, "", err
	}
	refreshClaims := accessClaims
	refreshClaims.TokenID = refreshTokenID
	refreshClaims.TokenType = jwtparser.TokenTypeRefresh
	refreshClaims.ExpiresAt = now.Add(refreshTTL).Unix()
	refreshToken, err := jwtparser.Generate(endUserKey, refreshClaims)
	if err != nil {
		return nil, "", err
	}

	if s.rotation != nil {
		if err := s.rotation.Register(ctx, domainauth.RefreshRotationKey(projectID, sessionID), refreshTokenID, refreshTTL); err != nil {
			return nil, "", err
		}
	}

	cookie := s.sessionCodec.Sign(projectID, sessionID)
	return &domainauth.TokenBundle{
		AccessToken:    accessToken,
		RefreshToken:   refreshToken,
		ExpiresAt:      accessClaims.ExpiresAt,
		RefreshTokenID: refreshTokenID,
	}, cookie, nil
}

func (s *SessionService) EnsureActiveSession(ctx context.Context, projectID, sessionID, userID string) error {
	sess, err := s.sessions.GetByID(ctx, projectID, sessionID)
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

// DeleteSessionsByUser 删除该用户全部会话（FK 级联之外的显式清会话：
// 登出全部/改密/封禁的单一咽喉）。成功后写 principal 缓存失效标记（P0.5：
// 缓存命中最坏吊销延迟 = TTL 30s，主动标记把本进程与跨实例都收敛到即时）。
func (s *SessionService) DeleteSessionsByUser(ctx context.Context, projectID, userID string) error {
	if err := s.sessions.DeleteByUser(ctx, projectID, userID); err != nil {
		slog.Warn("delete sessions by user failed", "project_id", projectID, "user_id", userID, "error", err)
		return err
	}
	if s.principalCache != nil {
		if err := s.principalCache.InvalidateUser(ctx, projectID, userID); err != nil {
			// 标记写失败不回滚删除：仍有 TTL 30s 上界兜底（注释见 principalcache）。
			slog.Warn("invalidate principal cache failed", "project_id", projectID, "user_id", userID, "error", err)
		}
	}
	return nil
}

// refreshTTL 返回刷新令牌 TTL：配置值或默认 7 天会话 TTL。
func (s *SessionService) refreshTTL() time.Duration {
	refreshTTL := defaultSessionTTL
	if d, err := time.ParseDuration(s.cfg.GetSecurity().GetJwt().GetRefreshTtl()); err == nil {
		refreshTTL = d
	}
	return refreshTTL
}

// maxSessionsPerUser 返回单用户会话上限：未配置（0 值）回退默认 50；
// -1 表示不限（返回 0，调用方跳过淘汰）。
func (s *SessionService) maxSessionsPerUser() int {
	if s.cfg == nil || s.cfg.GetSecurity() == nil || s.cfg.GetSecurity().GetSessions() == nil {
		return defaultMaxSessionsPerUser
	}
	switch v := s.cfg.GetSecurity().GetSessions().GetMaxPerUser(); {
	case v < 0:
		return 0 // -1 = 不限
	case v == 0:
		return defaultMaxSessionsPerUser
	default:
		return int(v)
	}
}

// evictOldestSessions 当会话数达到上限时，淘汰最旧（expire_at 最早）的
// 会话，使插入前剩余 = max-1。
func (s *SessionService) evictOldestSessions(ctx context.Context, projectID, userID string, max int) error {
	if max <= 0 {
		return nil
	}
	if err := s.sessions.DeleteOldestByUser(ctx, projectID, userID, max-1); err != nil {
		slog.Warn("evict old sessions failed", "project_id", projectID, "user_id", userID, "error", err)
		return err
	}
	return nil
}

//nolint:unused
func parseSessionTime(v any) (time.Time, error) {
	return ParseSessionTime(v)
}

// sha256HexLen 是 SHA-256 十六进制编码长度。
const sha256HexLen = 64

// sessionSecretLooksHashed 判定 stored 是否为 64 字符 hex（视为已哈希）。
func sessionSecretLooksHashed(stored string) bool {
	if len(stored) != sha256HexLen {
		return false
	}
	_, err := hex.DecodeString(stored)
	return err == nil
}

// canonicalizeSessionSecretHash 双读 secret_hash：64 字符 hex 原样返回，否则按明文做 HashOTP。
func canonicalizeSessionSecretHash(stored string) string {
	if stored == "" || sessionSecretLooksHashed(stored) {
		return stored
	}
	return HashOTP(stored)
}

// ParseSessionTime decodes session expire_at values from document storage.
func ParseSessionTime(v any) (time.Time, error) {
	switch t := v.(type) {
	case time.Time:
		return t, nil
	case string:
		return time.Parse(time.RFC3339Nano, t)
	}
	return time.Time{}, fmt.Errorf("unsupported time type")
}
