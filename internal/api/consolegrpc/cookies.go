package consolegrpc

// Console 管理后台的会话凭证通过 HttpOnly cookie 下发（设计见
// docs/p0-foundation-design.md）：浏览器 JS 无法读取 token，天然免疫 XSS
// 窃取。免疫承诺要求 gateway（浏览器）流的响应体不回传 token——否则同域
// XSS 可直接读响应体外带（见 auth.go 的 gateway 流置空）；直连 gRPC 的
// 客户端（SDK/测试）不受影响，仍从 proto 响应体取 token。
//
// 两个 cookie 均为 SameSite=Lax，跨站 POST 不会携带 cookie，足以覆盖
// 本服务的全部变更类端点（均为 POST），因此无需额外的 CSRF token 校验；
// 该前提依赖 cookie 仅限同源 /v1 API 使用。
//
// grpc-gateway 通过 internal/infra/server 的 authOutgoingHeaderMatcher 把
// set-cookie metadata 透传为 Set-Cookie 响应头。

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

// gatewayMetadataPrefix 是 grpc-gateway 注入 incoming metadata 的键前缀：
// AnnotateIncomingContext 的 incomingHeaderMatcher（生产为
// cmd/server/internal/runtime/grpc_gateway.go 的 authIncomingHeaderMatcher，
// 默认分支走 runtime.DefaultHeaderMatcher）把永久 HTTP 头（Accept、
// User-Agent、Content-Type 等）以 "grpcgateway-" 前缀注入 metadata。
// 直连 gRPC 的客户端传输层不会产生该前缀的 key；客户端手工伪造只会让自己
// 收不到响应体 token（降级到 cookie-only），不构成绕过面。
const gatewayMetadataPrefix = "grpcgateway-"

// fromGateway 判定请求是否经 grpc-gateway（浏览器 HTTP 流）：incoming
// metadata 中存在任意 "grpcgateway-" 前缀 key 即是。判据优于 cookie 存在性
// （首次登录浏览器尚无任何 cookie，直连 gRPC 也不会带 cookie 头）。
func fromGateway(ctx context.Context) bool {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return false
	}
	for k := range md {
		if strings.HasPrefix(strings.ToLower(k), gatewayMetadataPrefix) {
			return true
		}
	}
	return false
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
