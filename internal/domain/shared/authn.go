package shared

import (
	"errors"
	"strings"
)

var (
	ErrMultipleCredentials  = errors.New("multiple credentials provided")
	ErrMissingCredential    = errors.New("authentication credential is not provided")
	ErrInvalidAuthorization = errors.New("invalid authorization header")
)

// AuthnRequest 是三处传输共用的凭证面：gRPC metadata / HTTP header / Realtime hello。
// Grant（Realtime 禁 API key、HTTP upload 禁 end-user）由调用方在 Authenticate 之后施加。
type AuthnRequest struct {
	Authorization []string // Authorization 头，多值即多凭证
	APIKey        []string // X-Api-Key，多值即多凭证
	CookieHeaders []string // 原始 Cookie 头
	AccessToken   string   // 传输层额外的 bearer（Realtime hello.access_token）
}

// ParseAuthnRequest 从传输面抽出恰好一种凭证。
// Console cookie（TORCHWOOD_session_console）是运输中的 Access JWT，类型为 Token 而非 Session。
func ParseAuthnRequest(req AuthnRequest) (CredentialType, string, error) {
	if len(req.Authorization) > 1 || len(req.APIKey) > 1 {
		return "", "", ErrMultipleCredentials
	}

	var (
		credType CredentialType
		token    string
		seen     bool
	)
	if tok := strings.TrimSpace(req.AccessToken); tok != "" {
		credType, token, seen = CredentialTypeToken, tok, true
	}

	if len(req.Authorization) == 1 {
		raw := strings.TrimSpace(req.Authorization[0])
		if raw != "" {
			ct, tok, ok := ParseAuthorizationHeader(raw)
			if !ok {
				return "", "", ErrInvalidAuthorization
			}
			if seen {
				return "", "", ErrMultipleCredentials
			}
			credType, token, seen = ct, tok, true
		}
	}

	sessionCookies := sessionCookiesOf(req.CookieHeaders)
	if len(sessionCookies) > 1 {
		return "", "", ErrMultipleCredentials
	}
	if len(sessionCookies) == 1 {
		if seen {
			return "", "", ErrMultipleCredentials
		}
		ct := CredentialTypeSession
		if sessionCookies[0].console {
			// Cookie 只是运输：console 会话 cookie 里是 Access JWT。
			ct = CredentialTypeToken
		}
		credType, token, seen = ct, sessionCookies[0].value, true
	}

	if len(req.APIKey) == 1 {
		raw := strings.TrimSpace(req.APIKey[0])
		if raw != "" {
			if seen {
				return "", "", ErrMultipleCredentials
			}
			return CredentialTypeAPIKey, raw, nil
		}
	}
	if seen {
		return credType, token, nil
	}
	return "", "", ErrMissingCredential
}

// ExecutionTokenPrefix 是函数执行 token 的保留前缀（P0 执行身份）：
// `Authorization: Bearer twx_...` 在凭证解析层即判定为独立凭证族
// （CredentialTypeExecution），不进入 JWT 解析路径；前缀由随机 32 字节
// base64url token（infra/auth.RedisExecutionTokenService）保证与 JWT
// （"eyJ" 开头）无撞形。
const ExecutionTokenPrefix = "twx_"

// ParseAuthorizationHeader 解析 Authorization 头，支持 Bearer / Session / ApiKey 三种 scheme。
// Bearer 值带 ExecutionTokenPrefix 前缀时返回 CredentialTypeExecution
// （先前缀判断，避免执行 token 被当作 JWT 解析报错）。
func ParseAuthorizationHeader(raw string) (CredentialType, string, bool) {
	parts := strings.Fields(raw)
	if len(parts) != 2 {
		return "", "", false
	}
	switch strings.ToLower(parts[0]) {
	case "bearer":
		if strings.HasPrefix(parts[1], ExecutionTokenPrefix) {
			return CredentialTypeExecution, parts[1], true
		}
		return CredentialTypeToken, parts[1], true
	case "session":
		return CredentialTypeSession, parts[1], true
	case "apikey", "api-key":
		return CredentialTypeAPIKey, parts[1], true
	}
	return "", "", false
}

type sessionCookie struct {
	value   string
	console bool
}

func sessionCookiesOf(headers []string) []sessionCookie {
	var out []sessionCookie
	for _, h := range headers {
		for _, part := range strings.Split(h, ";") {
			name, value, found := strings.Cut(strings.TrimSpace(part), "=")
			if !found || value == "" {
				continue
			}
			if name == ConsoleSessionCookieName {
				out = append(out, sessionCookie{value: value, console: true})
				continue
			}
			if strings.HasPrefix(name, SessionCookiePrefix) {
				out = append(out, sessionCookie{value: value})
			}
		}
	}
	return out
}
