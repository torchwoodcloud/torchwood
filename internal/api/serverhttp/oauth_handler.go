package serverhttp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	"github.com/torchwooddev/torchwood/internal/app/client"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
)

// OAuthHandler handles browser OAuth2 callback redirects.
type OAuthHandler struct {
	account       *client.Account
	trusted       *interceptor.TrustedProxies
	secureCookies bool
}

func NewOAuthHandler(account *client.Account, cfg *config.AppConfig) (*OAuthHandler, error) {
	trusted, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, fmt.Errorf("parse security.trusted_proxies: %w", err)
	}
	// 与 console 会话 cookie 一致：以 public_url 是否 https 决定 Secure 标志。
	// 反向代理 TLS 终结部署下 r.TLS 恒为 nil，不能依赖该字段判断。
	return &OAuthHandler{
		account:       account,
		trusted:       trusted,
		secureCookies: strings.HasPrefix(cfg.GetServer().GetHttp().GetPublicUrl(), "https://"),
	}, nil
}

func (h *OAuthHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("GET", "/v1/account/oauth2/{provider}/callback", h.callback)
}

func (h *OAuthHandler) callback(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	provider := strings.TrimSpace(pathParams["provider"])
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if provider == "" || code == "" || state == "" {
		http.Redirect(w, r, "/?error=invalid_oauth_callback", http.StatusFound)
		return
	}

	ctx := contexts.WithClientInfo(r.Context(), contexts.ClientInfo{
		IP:        h.clientIP(r),
		UserAgent: r.UserAgent(),
	})

	// M5 C5：抽取浏览器 cookie 供 use-case 做归属/CSRF 校验——
	//   TORCHWOOD_session_<project>（除 console 外）：link 流验本人；
	//   TORCHWOOD_oauth_nonce_<project>：login 流与 state 配对。
	// 两 map 恒非 nil 传入（空 map 也强制校验，fail-closed）。
	result, err := h.account.HandleOAuth2Callback(ctx, provider, code, state,
		oauthCookiesOf(r, shared.SessionCookiePrefix, shared.ConsoleSessionCookieName),
		oauthCookiesOf(r, oauthNonceCookiePrefix, ""))
	if err != nil {
		target := "/?error=oauth_failed"
		if result != nil && result.RedirectURL != "" {
			target = result.RedirectURL
		}
		http.Redirect(w, r, target, http.StatusFound)
		return
	}

	if result.SessionCookie != "" && result.User != nil {
		http.SetCookie(w, &http.Cookie{
			Name:     fmt.Sprintf("TORCHWOOD_session_%s", result.ProjectID),
			Value:    result.SessionCookie,
			Path:     "/",
			HttpOnly: true,
			Secure:   h.secureCookies,
			SameSite: http.SameSiteLaxMode,
		})
	}
	http.Redirect(w, r, result.RedirectURL, http.StatusFound)
}

// oauthNonceCookiePrefix 是 OAuth 发起时种的一次性 nonce cookie 前缀
//（完整名 TORCHWOOD_oauth_nonce_<project>，M5 C5 login CSRF 绑定）。
const oauthNonceCookiePrefix = "TORCHWOOD_oauth_nonce_"

// oauthCookiesOf 按前缀抽取浏览器 cookie 为 project → value 映射；
// exclude 用于把 console 会话 cookie 挡在端用户 session 前缀之外。
func oauthCookiesOf(r *http.Request, prefix, exclude string) map[string]string {
	out := map[string]string{}
	for _, c := range r.Cookies() {
		if c.Value == "" || !strings.HasPrefix(c.Name, prefix) || (exclude != "" && c.Name == exclude) {
			continue
		}
		out[strings.TrimPrefix(c.Name, prefix)] = c.Value
	}
	return out
}

// clientIP 与 gRPC ClientInfoInterceptor 走同一 trusted-proxy 规则：
// 仅当直连对端命中可信代理网段时才采纳 X-Forwarded-For 首跳，否则用对端地址。
func (h *OAuthHandler) clientIP(r *http.Request) string {
	return h.trusted.ResolveClientIP(
		interceptor.PeerIPFromAddr(r.RemoteAddr),
		r.Header.Get("X-Forwarded-For"),
		r.Header.Get("X-Real-Ip"),
	)
}
