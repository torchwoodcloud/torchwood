package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"golang.org/x/oauth2"
	githuboauth "golang.org/x/oauth2/github"
	googleoauth "golang.org/x/oauth2/google"
)

// httpClient is shared by all outbound OAuth HTTP calls; the timeout keeps a
// hanging provider from blocking login callbacks indefinitely.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// gitHubAPIBase 供测试覆盖为 httptest server（与 wechat 的 weChatAPIBase 模式一致）。
var gitHubAPIBase = "https://api.github.com"

// NewOAuthAuthenticator builds a provider-specific OAuth2 client.
func NewOAuthAuthenticator(provider, clientID, clientSecret, redirectURL string, scopes []string) (domainauth.OAuthAuthenticator, error) {
	switch strings.ToLower(provider) {
	case domainauth.ProviderGoogle:
		if len(scopes) == 0 {
			scopes = []string{"openid", "email", "profile"}
		}
		cfg := &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       scopes,
			Endpoint:     googleoauth.Endpoint,
		}
		return &genericOAuthAuthenticator{cfg: cfg, userInfoURL: "https://www.googleapis.com/oauth2/v2/userinfo"}, nil
	case domainauth.ProviderGitHub:
		if len(scopes) == 0 {
			scopes = []string{"read:user", "user:email"}
		}
		cfg := &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Scopes:       scopes,
			Endpoint:     githuboauth.Endpoint,
		}
		return &githubOAuthAuthenticator{genericOAuthAuthenticator{cfg: cfg}}, nil
	case domainauth.ProviderWeChatWeb, domainauth.ProviderWeChatMP:
		return newWeChatOAuth(provider, clientID, clientSecret, sanitizeWeChatRedirect(redirectURL))
	default:
		return nil, fmt.Errorf("unsupported oauth provider: %s", provider)
	}
}

type genericOAuthAuthenticator struct {
	cfg         *oauth2.Config
	userInfoURL string
}

func (a *genericOAuthAuthenticator) AuthorizeURL(stateID, pkceChallenge string) string {
	return a.cfg.AuthCodeURL(stateID,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("code_challenge", pkceChallenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	)
}

func (a *genericOAuthAuthenticator) Exchange(ctx context.Context, code, pkceVerifier string) (*domainauth.OAuthUserInfo, error) {
	token, err := a.cfg.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return nil, err
	}
	return a.fetchUserInfo(ctx, token)
}

func (a *genericOAuthAuthenticator) fetchUserInfo(ctx context.Context, token *oauth2.Token) (*domainauth.OAuthUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.userInfoURL, nil)
	if err != nil {
		return nil, err
	}
	token.SetAuthHeader(req)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("userinfo request failed: %s", strings.TrimSpace(string(body)))
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	info := &domainauth.OAuthUserInfo{Raw: raw}
	if id, ok := raw["id"].(string); ok {
		info.ProviderUID = id
	} else if idNum, ok := raw["id"].(float64); ok {
		info.ProviderUID = fmt.Sprintf("%.0f", idNum)
	}
	if email, ok := raw["email"].(string); ok {
		info.Email = strings.ToLower(strings.TrimSpace(email))
	}
	// Google /oauth2/v2/userinfo 自带 email_verified 字段，直接透传，
	// 由 resolveOAuthUser 强制校验未验证邮箱（安全评审 M8）。
	if verified, ok := raw["email_verified"].(bool); ok {
		info.EmailVerified = verified
	}
	if name, ok := raw["name"].(string); ok {
		info.Name = name
	}
	if avatar, ok := raw["picture"].(string); ok {
		info.AvatarURL = avatar
	}
	return info, nil
}

type githubOAuthAuthenticator struct {
	genericOAuthAuthenticator
}

func (a *githubOAuthAuthenticator) AuthorizeURL(stateID, pkceChallenge string) string {
	return a.genericOAuthAuthenticator.AuthorizeURL(stateID, pkceChallenge)
}

func (a *githubOAuthAuthenticator) Exchange(ctx context.Context, code, pkceVerifier string) (*domainauth.OAuthUserInfo, error) {
	token, err := a.cfg.Exchange(ctx, code, oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		return nil, err
	}
	info, err := a.fetchGitHubUser(ctx, token)
	if err != nil {
		return nil, err
	}
	if info.Email == "" {
		email, err := a.fetchGitHubPrimaryEmail(ctx, token)
		if err != nil {
			return nil, err
		}
		info.Email = email
	}
	// GitHub 邮箱要么来自 public profile，要么由 fetchGitHubPrimaryEmail 强制
	// verified 过滤后取得，email 非空即视为已验证（安全评审 M8）。
	info.EmailVerified = info.Email != ""
	return info, nil
}

func (a *githubOAuthAuthenticator) fetchGitHubUser(ctx context.Context, token *oauth2.Token) (*domainauth.OAuthUserInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubAPIBase+"/user", nil)
	if err != nil {
		return nil, err
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("github user request failed: status %d", resp.StatusCode)
	}
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	info := &domainauth.OAuthUserInfo{Raw: raw}
	if idNum, ok := raw["id"].(float64); ok {
		info.ProviderUID = fmt.Sprintf("%.0f", idNum)
	}
	if login, ok := raw["login"].(string); ok && info.Name == "" {
		info.Name = login
	}
	if name, ok := raw["name"].(string); ok && name != "" {
		info.Name = name
	}
	if email, ok := raw["email"].(string); ok {
		info.Email = strings.ToLower(strings.TrimSpace(email))
	}
	if avatar, ok := raw["avatar_url"].(string); ok {
		info.AvatarURL = avatar
	}
	return info, nil
}

func (a *githubOAuthAuthenticator) fetchGitHubPrimaryEmail(ctx context.Context, token *oauth2.Token) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gitHubAPIBase+"/user/emails", nil)
	if err != nil {
		return "", err
	}
	token.SetAuthHeader(req)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("github emails request failed: status %d", resp.StatusCode)
	}
	var emails []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return "", err
	}
	for _, item := range emails {
		primary, _ := item["primary"].(bool)
		verified, _ := item["verified"].(bool)
		email, _ := item["email"].(string)
		if primary && verified && email != "" {
			return strings.ToLower(strings.TrimSpace(email)), nil
		}
	}
	for _, item := range emails {
		verified, _ := item["verified"].(bool)
		email, _ := item["email"].(string)
		if verified && email != "" {
			return strings.ToLower(strings.TrimSpace(email)), nil
		}
	}
	return "", fmt.Errorf("github account has no verified email")
}
