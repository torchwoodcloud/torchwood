package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
)

func TestWeChatOAuth_AuthorizeURL(t *testing.T) {
	w, err := newWeChatOAuth(domainauth.ProviderWeChatWeb, "wx-app", "secret", "https://example.com/cb")
	if err != nil {
		t.Fatal(err)
	}
	u := w.AuthorizeURL("state-1", "")
	if !strings.Contains(u, "qrconnect") {
		t.Fatalf("expected qrconnect url, got %q", u)
	}
	if !strings.Contains(u, "appid=wx-app") {
		t.Fatalf("missing appid: %q", u)
	}
	if !strings.Contains(u, "state=state-1") {
		t.Fatalf("missing state: %q", u)
	}

	mp, err := newWeChatOAuth(domainauth.ProviderWeChatMP, "wx-mp", "secret", "https://example.com/cb")
	if err != nil {
		t.Fatal(err)
	}
	mpURL := mp.AuthorizeURL("state-2", "")
	if !strings.Contains(mpURL, "oauth2/authorize") {
		t.Fatalf("expected mp authorize url, got %q", mpURL)
	}
	if !strings.Contains(mpURL, "snsapi_userinfo") {
		t.Fatalf("expected snsapi_userinfo scope, got %q", mpURL)
	}
}

func TestWeChatOAuth_Exchange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "access_token"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "at-1",
				"openid":       "openid-1",
				"unionid":      "union-1",
			})
		case strings.Contains(r.URL.Path, "userinfo"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"openid":     "openid-1",
				"unionid":    "union-1",
				"nickname":   "Test User",
				"headimgurl": "https://example.com/avatar.png",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	oldBase := weChatAPIBase
	weChatAPIBase = srv.URL
	t.Cleanup(func() { weChatAPIBase = oldBase })

	w, err := newWeChatOAuth(domainauth.ProviderWeChatWeb, "wx-app", "secret", "https://example.com/cb")
	if err != nil {
		t.Fatal(err)
	}
	w.httpClient = srv.Client()

	info, err := w.Exchange(context.Background(), "code-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if info.ProviderUID != "union-1" {
		t.Fatalf("expected unionid as provider uid, got %q", info.ProviderUID)
	}
	if info.Name != "Test User" {
		t.Fatalf("expected nickname, got %q", info.Name)
	}
}

func TestExchangeWeChatMiniProgramCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "jscode2session") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"openid":  "mp-openid",
			"unionid": "mp-union",
		})
	}))
	defer srv.Close()

	oldBase := weChatAPIBase
	weChatAPIBase = srv.URL
	t.Cleanup(func() { weChatAPIBase = oldBase })

	info, err := exchangeWeChatMiniProgramCode(context.Background(), srv.Client(), "wx-mp", "secret", "js-code")
	if err != nil {
		t.Fatal(err)
	}
	if info.ProviderUID != "mp-union" {
		t.Fatalf("expected unionid, got %q", info.ProviderUID)
	}
	if info.OpenID != "mp-openid" {
		t.Fatalf("expected openid, got %q", info.OpenID)
	}
}

func TestDecodeWeChatResponse_Error(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	rec.WriteHeader(http.StatusOK)
	_, _ = rec.WriteString(`{"errcode":40029,"errmsg":"invalid code"}`)
	_, err := decodeWeChatResponse(rec.Result())
	if err == nil || !strings.Contains(err.Error(), "invalid code") {
		t.Fatalf("expected wechat error, got %v", err)
	}
}
