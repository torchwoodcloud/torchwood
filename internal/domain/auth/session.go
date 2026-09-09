package auth

import (
	"context"

	"github.com/torchwooddev/torchwood/internal/domain/users"
)

// Session provider identifiers stored on session documents.
const (
	ProviderEmail     = "email"
	ProviderEmailOTP  = "email_otp"
	ProviderPhoneOTP  = "phone_otp"
	ProviderPhone     = "phone"
	ProviderAnonymous = "anonymous"
	ProviderMagicURL  = "magic_url"
	ProviderMFA       = "mfa"
)

const (
	OTPChannelEmail = "email"
	OTPChannelPhone = "phone"
)

// TokenBundle holds JWT access and refresh tokens for an authenticated session.
type TokenBundle struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
	// RefreshTokenID is the jti of RefreshToken; used for rotation bookkeeping
	// and never mapped to proto responses.
	RefreshTokenID string
}

// UserRoleResolver loads JWT role claims (at token issuance, or per-request
// in the validator). u 是调用方已取回的 users 行（可 nil——实现侧兜底单查；
// 手头已有 user 的调用方必须传入，避免重复 users.GetByID——P0.5 热路径
// 清账项：客户端鉴权一次原本 4 次 DB 往返，本修复去掉其中一次重复）。
type UserRoleResolver interface {
	LoadUserRoles(ctx context.Context, projectID, userID string, u *users.User) ([]string, error)
}

// SessionService creates sessions and issues JWT tokens for authenticated users.
type SessionService interface {
	CreateSessionAndTokens(ctx context.Context, projectID, userID, email, provider string) (*TokenBundle, string, error)
	// CreateShortLivedSessionAndTokens 签发短时会话（CreateUserToken 服务端
	// 登录桥接专用，M5 C2）：会话 TTL 不继承 7 天默认值，access/refresh TTL
	// 被实现内的硬上限常量封顶，任何配置都无法突破。
	CreateShortLivedSessionAndTokens(ctx context.Context, projectID, userID, email, provider string) (*TokenBundle, string, error)
	IssueTokens(ctx context.Context, projectID, userID, email, sessionID string) (*TokenBundle, string, error)
	// IssueTokensWithRefreshID issues tokens with a caller-provided refresh token id
	// (jti) so the rotation store and the issued token stay in sync.
	IssueTokensWithRefreshID(ctx context.Context, projectID, userID, email, sessionID, refreshTokenID string) (*TokenBundle, string, error)
	EnsureActiveSession(ctx context.Context, projectID, sessionID, userID string) error
	// DeleteSessionsByUser removes every session of the user (e.g. after a password change).
	DeleteSessionsByUser(ctx context.Context, projectID, userID string) error
}
