package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

// weChatAPIBase is overridable in tests to point at httptest servers.
var weChatAPIBase = "https://api.weixin.qq.com"

// weChatOpenBase is overridable in tests for authorize URL generation.
var weChatOpenBase = "https://open.weixin.qq.com"

type weChatOAuth struct {
	appID       string
	appSecret   string
	redirectURL string
	mode        string // web | mp
	httpClient  *http.Client
}

func newWeChatOAuth(provider, appID, appSecret, redirectURL string) (*weChatOAuth, error) {
	mode := ""
	switch provider {
	case domainauth.ProviderWeChatWeb:
		mode = "web"
	case domainauth.ProviderWeChatMP:
		mode = "mp"
	default:
		return nil, fmt.Errorf("unsupported wechat oauth provider: %s", provider)
	}
	return &weChatOAuth{
		appID:       appID,
		appSecret:   appSecret,
		redirectURL: redirectURL,
		mode:        mode,
		httpClient:  httpClient,
	}, nil
}

func (w *weChatOAuth) AuthorizeURL(stateID, _ string) string {
	scope := "snsapi_login"
	endpoint := weChatOpenBase + "/connect/qrconnect"
	if w.mode == "mp" {
		scope = "snsapi_userinfo"
		endpoint = weChatOpenBase + "/connect/oauth2/authorize"
	}
	values := url.Values{}
	values.Set("appid", w.appID)
	values.Set("redirect_uri", w.redirectURL)
	values.Set("response_type", "code")
	values.Set("scope", scope)
	values.Set("state", stateID)
	return endpoint + "?" + values.Encode() + "#wechat_redirect"
}

func (w *weChatOAuth) Exchange(ctx context.Context, code, _ string) (*domainauth.OAuthUserInfo, error) {
	tokenPayload, err := w.exchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	accessToken, _ := tokenPayload["access_token"].(string)
	openid, _ := tokenPayload["openid"].(string)
	unionid, _ := tokenPayload["unionid"].(string)
	if openid == "" {
		return nil, fmt.Errorf("wechat response missing openid")
	}

	info := &domainauth.OAuthUserInfo{
		OpenID:      openid,
		UnionID:     unionid,
		ProviderUID: domainauth.WeChatIdentityUID(unionid, openid),
		Raw:         tokenPayload,
	}
	if accessToken == "" {
		return info, nil
	}
	profile, err := w.fetchUserInfo(ctx, accessToken, openid)
	if err != nil {
		return info, nil
	}
	mergeWeChatProfile(info, profile)
	return info, nil
}

func (w *weChatOAuth) exchangeCode(ctx context.Context, code string) (map[string]any, error) {
	endpoint := weChatAPIBase + "/sns/oauth2/access_token"
	values := url.Values{}
	values.Set("appid", w.appID)
	values.Set("secret", w.appSecret)
	values.Set("code", code)
	values.Set("grant_type", "authorization_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeWeChatResponse(resp)
}

func (w *weChatOAuth) fetchUserInfo(ctx context.Context, accessToken, openid string) (map[string]any, error) {
	values := url.Values{}
	values.Set("access_token", accessToken)
	values.Set("openid", openid)
	values.Set("lang", "zh_CN")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, weChatAPIBase+"/sns/userinfo?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return decodeWeChatResponse(resp)
}

// ExchangeWeChatMiniProgramCode exchanges wx.login code for openid/unionid.
func ExchangeWeChatMiniProgramCode(ctx context.Context, appID, appSecret, code string) (*domainauth.OAuthUserInfo, error) {
	return exchangeWeChatMiniProgramCode(ctx, httpClient, appID, appSecret, code)
}

func exchangeWeChatMiniProgramCode(ctx context.Context, client *http.Client, appID, appSecret, code string) (*domainauth.OAuthUserInfo, error) {
	if client == nil {
		client = httpClient
	}
	values := url.Values{}
	values.Set("appid", appID)
	values.Set("secret", appSecret)
	values.Set("js_code", code)
	values.Set("grant_type", "authorization_code")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, weChatAPIBase+"/sns/jscode2session?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := decodeWeChatResponse(resp)
	if err != nil {
		return nil, err
	}
	openid, _ := payload["openid"].(string)
	unionid, _ := payload["unionid"].(string)
	if openid == "" {
		return nil, fmt.Errorf("wechat response missing openid")
	}
	info := &domainauth.OAuthUserInfo{
		OpenID:      openid,
		UnionID:     unionid,
		ProviderUID: domainauth.WeChatIdentityUID(unionid, openid),
		Raw:         payload,
	}
	return info, nil
}

func decodeWeChatResponse(resp *http.Response) (map[string]any, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if errCode, ok := payload["errcode"].(float64); ok && errCode != 0 {
		msg, _ := payload["errmsg"].(string)
		if msg == "" {
			msg = "wechat api error"
		}
		return nil, fmt.Errorf("%s (code %.0f)", msg, errCode)
	}
	return payload, nil
}

func mergeWeChatProfile(info *domainauth.OAuthUserInfo, profile map[string]any) {
	if info == nil || profile == nil {
		return
	}
	if unionid, ok := profile["unionid"].(string); ok && unionid != "" {
		info.UnionID = unionid
		info.ProviderUID = domainauth.WeChatIdentityUID(unionid, info.OpenID)
	}
	if nickname, ok := profile["nickname"].(string); ok {
		info.Name = nickname
	}
	if avatar, ok := profile["headimgurl"].(string); ok {
		info.AvatarURL = avatar
	}
	if info.Raw == nil {
		info.Raw = map[string]any{}
	}
	info.Raw["userinfo"] = profile
}

//nolint:unused
func isWeChatOAuthProvider(provider string) bool {
	switch provider {
	case domainauth.ProviderWeChatWeb, domainauth.ProviderWeChatMP:
		return true
	default:
		return false
	}
}

// sanitizeWeChatRedirect ensures redirect URI matches WeChat requirements.
func sanitizeWeChatRedirect(raw string) string {
	return strings.TrimSpace(raw)
}
