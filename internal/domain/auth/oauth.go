package auth

import (
	"context"
	"time"
)

// OAuthState captures pending OAuth2 authorization context stored server-side.
type OAuthState struct {
	StateID      string
	ProjectID    string
	Provider     string
	SuccessURL   string
	FailureURL   string
	PKCEVerifier string
	// LinkUserID, when set, binds the OAuth identity to an existing authenticated user.
	LinkUserID string
	// Nonce 是发起端点生成的一次性随机值（仅存服务端 state 记录，M5 C5）：
	// HTTP 回调必须回带 TORCHWOOD_oauth_nonce_<project> cookie 且与本值一致
	//（login CSRF 绑定浏览器，state 注入失效）。
	Nonce string
}

// SessionCookieVerifier 校验端用户会话 cookie（HMAC）并解出其归属（M5 C5：
// OAuth link 流回调面校验请求者身份）。实现位于 infra/auth.SessionCookieCodec。
type SessionCookieVerifier interface {
	Verify(value string) (projectID, sessionID string, err error)
}

// OAuthStateStore persists OAuth2 state and PKCE verifiers until callback.
type OAuthStateStore interface {
	Save(ctx context.Context, state OAuthState, ttl time.Duration) error
	// Consume atomically reads and deletes the state, enforcing one-time use
	// so a callback cannot be replayed concurrently.
	Consume(ctx context.Context, stateID string) (*OAuthState, error)
}

// OAuthUserInfo is normalized profile data from an OAuth2 provider.
type OAuthUserInfo struct {
	ProviderUID string
	UnionID     string
	OpenID      string
	Email       string
	// EmailVerified 表示 provider 已确认邮箱归属；resolveOAuthUser 对带 Email 但
	// 未验证的 profile 一律拒绝，防止未验证邮箱占号（安全评审 M8）。
	EmailVerified bool
	Name          string
	AvatarURL     string
	Raw           map[string]any
}

// OAuthAuthenticator builds authorize URLs and exchanges authorization codes.
type OAuthAuthenticator interface {
	AuthorizeURL(stateID, pkceChallenge string) string
	Exchange(ctx context.Context, code, pkceVerifier string) (*OAuthUserInfo, error)
}

// OAuthAuthenticatorFactory 构建各 provider 的 OAuthAuthenticator。
// 引入工厂端口使 app 层无需依赖 infra 构造细节，可在测试中注入 fake。
type OAuthAuthenticatorFactory interface {
	NewAuthenticator(provider, clientID, clientSecret, redirectURL string, scopes []string) (OAuthAuthenticator, error)
}
