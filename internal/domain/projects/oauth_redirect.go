package projects

import (
	"fmt"
	"net/url"
	"strings"
)

const SettingsKeyOAuthAllowedRedirectURLs = "auth.oauth_allowed_redirect_urls"

// 白名单条目上限（与 proto UpdateOAuthRedirectAllowlistRequest 的
// buf.validate 注解对齐；proto 管形状，这里管业务口径的单一声明）。
const (
	MaxAllowlistEntries  = 100
	MaxAllowlistEntryLen = 2048
)

// ValidateAllowlistEntry 校验白名单条目：必须可解析为绝对 http/https URL
// 且带 host。条目即白名单本身（MatchRedirectURL 的匹配输入），故不做任何
// 前缀/通配匹配——scheme+host（可含路径前缀）即合法条目。
func ValidateAllowlistEntry(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("entry is required")
	}
	if len(raw) > MaxAllowlistEntryLen {
		return fmt.Errorf("entry must be at most %d characters", MaxAllowlistEntryLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("entry is not a valid url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("entry must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("entry host is required")
	}
	return nil
}

// OAuthAllowedRedirectURLs reads configured redirect URL allowlist from project settings.
func OAuthAllowedRedirectURLs(settings map[string]any) []string {
	if settings == nil {
		return nil
	}
	raw, ok := settings[SettingsKeyOAuthAllowedRedirectURLs]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	default:
		return nil
	}
}

// DefaultOAuthRedirectAllowlist returns fallback allowed origins for development.
func DefaultOAuthRedirectAllowlist(publicBaseURL string) []string {
	out := []string{
		"http://localhost",
		"http://127.0.0.1",
		"https://localhost",
		"https://127.0.0.1",
	}
	if origin := urlOrigin(publicBaseURL); origin != "" {
		out = append(out, origin)
	}
	return out
}

// MatchRedirectURL reports whether rawURL matches one of the allowed redirect entries.
func MatchRedirectURL(rawURL string, allowed []string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	for _, entry := range allowed {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		pattern, err := url.Parse(entry)
		if err != nil || pattern.Scheme == "" || pattern.Host == "" {
			continue
		}
		if !strings.EqualFold(u.Scheme, pattern.Scheme) || !strings.EqualFold(u.Host, pattern.Host) {
			continue
		}
		if pattern.Path == "" || pattern.Path == "/" {
			return true
		}
		if strings.HasPrefix(u.Path, pattern.Path) {
			return true
		}
	}
	return false
}

func urlOrigin(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
