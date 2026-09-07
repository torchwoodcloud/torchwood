package auth

import "context"

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

// UserRoleResolver loads JWT role claims for a user at token issuance time.
type UserRoleResolver interface {
	LoadUserRoles(ctx context.Context, projectID, userID string) ([]string, error)
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
