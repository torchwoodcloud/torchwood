package serverhttp

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// oauthStateTTLSeconds 与 app 层 oauth state 的 Redis TTL（10min）对齐：
// nonce cookie 比 state 活得久没有意义，反之则回调必失败。
const oauthStateTTLSeconds = 600

// authorize 端点默认 IP 限流参数（与 interceptor 通用限流 ip 维度同源同默认，
// 可经 security.rate_limit.ip 覆盖）；窗口默认 60s。
const authorizeRateLimitDefault = 300

// OAuthHandler handles browser OAuth2 callback redirects.
type OAuthHandler struct {
	account       *client.Account
	trusted       *interceptor.TrustedProxies
	secureCookies bool
	// audit 是可选的拒绝审计 sink（M5 C6：回调校验失败落 denied 审计行）。
	audit audit.Repository
	// limiter 是 authorize 端点的 per-IP 限流器（Redis 固定窗口；nil 时跳过限流，
	// 仅测试装配）。基础设施故障 fail-open：authorize 不校验任何凭证，Redis 抖动
	// 不应打断登录；滥用窗口由 state TTL 兜底。
	limiter    domainauth.RateLimiter
	rateLimit  int
	rateWindow time.Duration
}

func NewOAuthHandler(account *client.Account, cfg *config.AppConfig, auditRepo audit.Repository, limiter domainauth.RateLimiter) (*OAuthHandler, error) {
	trusted, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, fmt.Errorf("parse security.trusted_proxies: %w", err)
	}
	// 与 console 会话 cookie 一致：以 public_url 是否 https 决定 Secure 标志。
	// 反向代理 TLS 终结部署下 r.TLS 恒为 nil，不能依赖该字段判断。
	h := &OAuthHandler{
		account:       account,
		trusted:       trusted,
		secureCookies: strings.HasPrefix(cfg.GetServer().GetHttp().GetPublicUrl(), "https://"),
		audit:         auditRepo,
		limiter:       limiter,
		rateLimit:     authorizeRateLimitDefault,
		rateWindow:    60 * time.Second,
	}
	if d := cfg.GetSecurity().GetRateLimit().GetIp(); d != nil {
		if d.GetLimit() > 0 {
			h.rateLimit = int(d.GetLimit())
		}
		if w, perr := time.ParseDuration(d.GetWindow()); perr == nil && w > 0 {
			h.rateWindow = w
		}
	}
	return h, nil
}

func (h *OAuthHandler) Register(mux *runtime.ServeMux) {
	_ = mux.HandlePath("GET", "/v1/account/oauth2/{provider}/callback", h.callback)
	// authorize 是浏览器 302 发起端点（命名对齐 Auth0/Supabase 等主流与
	// RFC 6749 的授权入口心智；与 callback 成对）：发起从"前端跨源 fetch"
	// 变为 top-level 导航，nonce cookie 在 API 域第一方上下文落库，回调（同为
	// top-level 导航）必然携带——任意客户前端域零 CORS 配置。
	_ = mux.HandlePath("GET", "/v1/account/oauth2/{provider}/authorize", h.authorize)
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
		// M5 C6：回调校验拒绝（state/nonce/link 归属等）补一条 deny 审计。
		if st, ok := status.FromError(err); ok {
			auditDenyFromHTTP(r, h.clientIP(r), h.audit, nil,
				"/v1/account/oauth2/"+provider+"/callback", st.Code().String())
		}
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
// （完整名 TORCHWOOD_oauth_nonce_<project>，M5 C5 login CSRF 绑定）。
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

// authorize 处理浏览器 OAuth2 发起（top-level 导航，302 到 provider 授权页）：
// 复用 CreateOAuth2Session 的全部校验（provider 归一、success/failure 项目
// 白名单、provider 启用），nonce 经 Set-Cookie 种在 API 域第一方上下文——
// 回调（同为 top-level 导航）必然携带，跨域前端零 CORS 依赖。
//
// 失败分层：白名单校验之前的失败（项目不存在/URL 非法/未过白名单）不可信任
// query 里的 failure URL，返回 400 纯文本（不反射输入）；白名单之后的失败
// （provider 未启用等）failure 已可信，302 回 failure?error=oauth_failed。
func (h *OAuthHandler) authorize(w http.ResponseWriter, r *http.Request, pathParams map[string]string) {
	w.Header().Set("Cache-Control", "no-store")
	provider := strings.TrimSpace(pathParams["provider"])
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	success := strings.TrimSpace(r.URL.Query().Get("success"))
	failure := strings.TrimSpace(r.URL.Query().Get("failure"))
	if provider == "" || projectID == "" || success == "" || failure == "" {
		http.Error(w, "invalid oauth authorize request", http.StatusBadRequest)
		return
	}

	ctx := contexts.WithClientInfo(r.Context(), contexts.ClientInfo{
		IP:        h.clientIP(r),
		UserAgent: r.UserAgent(),
	})
	if h.limiter != nil && h.rateLimit > 0 && h.rateWindow > 0 {
		if err := h.limiter.Allow(ctx, "oauthauthorize:ip:"+h.clientIP(r), h.rateLimit, h.rateWindow); err != nil {
			if status.Code(err) == codes.ResourceExhausted {
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			// limiter 基础设施故障 fail-open：与通用 API 限流的熔断放行同一
			// 产品取向（E-1）；authorize 不校验凭证，拒绝的代价不成比例。
			slog.WarnContext(ctx, "oauth authorize rate limiter unavailable; failing open",
				"err", err)
		}
	}

	authorizeURL, nonce, err := h.account.CreateOAuth2Session(ctx, client.CreateOAuth2SessionCommand{
		ProjectID: projectID,
		Provider:  provider,
		Success:   success,
		Failure:   failure,
	})
	if err != nil {
		if status.Code(err) == codes.FailedPrecondition {
			// provider 未启用/凭据缺失：success/failure 已过项目白名单，跳回可信。
			http.Redirect(w, r, appendQueryParam(failure, "error", "oauth_failed"), http.StatusFound)
			return
		}
		http.Error(w, "invalid oauth authorize request", http.StatusBadRequest)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     fmt.Sprintf("TORCHWOOD_oauth_nonce_%s", projectID),
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   oauthStateTTLSeconds,
	})
	http.Redirect(w, r, authorizeURL, http.StatusFound)
}

// appendQueryParam 向 raw URL 追加单个 query 参数（已有同名则覆盖）。
func appendQueryParam(raw, key, value string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}
