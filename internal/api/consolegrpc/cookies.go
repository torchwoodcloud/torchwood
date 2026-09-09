package consolegrpc

// Console 管理后台的会话凭证通过 HttpOnly cookie 下发（设计见
// docs/p0-foundation-design.md）：浏览器 JS 无法读取 token，天然免疫 XSS
// 窃取。两个 cookie 均为 SameSite=Lax，跨站 POST 不会携带 cookie，足以覆盖
// 本服务的全部变更类端点（均为 POST），因此无需额外的 CSRF token 校验；
// 该前提依赖 cookie 仅限同源 /v1 API 使用。
//
// grpc-gateway 通过 internal/infra/server 的 authOutgoingHeaderMatcher 把
// set-cookie metadata 透传为 Set-Cookie 响应头；直连 gRPC 的客户端不受影响，
// 仍从 proto 响应体取 token。

import (
	"context"
	"net/http"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/app/console"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	// sessionCookieName 与 shared.ConsoleSessionCookieName 一致。
	sessionCookieName = "TORCHWOOD_session_console"
	refreshCookieName = "TORCHWOOD_console_refresh"
	// refreshCookiePath 把 refresh cookie 限制为只发向 console auth 端点。
	refreshCookiePath = "/v1/console/auth"
)

// setSessionCookies 在 SignIn/RefreshToken 成功后下发 access + refresh 两个
// HttpOnly cookie。SetHeader 失败（理论上只有直接调用 handler 且无 transport
// stream 时）不影响 gRPC 客户端从响应体取 token，故忽略错误。
func setSessionCookies(ctx context.Context, auth *console.Auth, tokens *console.TokenPair) {
	secure := auth.SecureCookies()
	_ = grpc.SetHeader(ctx, metadata.Pairs(
		"set-cookie", newCookie(sessionCookieName, tokens.AccessToken, "/", int(auth.AccessTTL().Seconds()), secure),
		"set-cookie", newCookie(refreshCookieName, tokens.RefreshToken, refreshCookiePath, int(auth.RefreshTTL().Seconds()), secure),
	))
}

// clearSessionCookies 在 SignOut 时以 Max-Age=0 过期同名 cookie（Path 必须与
// 签发时一致才能生效）。
func clearSessionCookies(ctx context.Context, auth *console.Auth) {
	secure := auth.SecureCookies()
	_ = grpc.SetHeader(ctx, metadata.Pairs(
		"set-cookie", newCookie(sessionCookieName, "", "/", -1, secure),
		"set-cookie", newCookie(refreshCookieName, "", refreshCookiePath, -1, secure),
	))
}

func newCookie(name, value, path string, maxAgeSeconds int, secure bool) string {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     path,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAgeSeconds,
	}
	return c.String()
}

// refreshTokenFromCookie 支持 cookie-only 浏览器流：RefreshToken 请求体为空时
// 从 cookie metadata 中取 TORCHWOOD_console_refresh。
func refreshTokenFromCookie(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, raw := range md.Get("cookie") {
		for _, part := range strings.Split(raw, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(part), "=")
			if found && value != "" && name == refreshCookieName {
				return value
			}
		}
	}
	return ""
}
