package serverhttp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/client"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---- 桩（仅覆盖 authorize 端点触达的路径）----

type oauthTestProjectRepo struct{ project *projects.Project }

func (r *oauthTestProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (r *oauthTestProjectRepo) GetProject(_ context.Context, id string) (*projects.Project, error) {
	if r.project == nil || r.project.ID != id {
		return nil, nil
	}
	return r.project, nil
}
func (r *oauthTestProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (r *oauthTestProjectRepo) ListProjects(context.Context) ([]projects.Project, error) {
	return nil, nil
}
func (r *oauthTestProjectRepo) UpdateProject(context.Context, *projects.Project) error { return nil }
func (r *oauthTestProjectRepo) DeleteProject(context.Context, string) error            { return nil }
func (r *oauthTestProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error {
	return nil
}

type oauthTestProviderRepo struct{ provider *projects.OAuthProvider }

func (r *oauthTestProviderRepo) GetOAuthProvider(context.Context, string, string) (*projects.OAuthProvider, error) {
	return r.provider, nil
}
func (r *oauthTestProviderRepo) ListOAuthProviders(context.Context, string) ([]projects.OAuthProvider, error) {
	return nil, nil
}
func (r *oauthTestProviderRepo) UpsertOAuthProvider(context.Context, *projects.OAuthProvider) error {
	return nil
}
func (r *oauthTestProviderRepo) DeleteOAuthProvider(context.Context, string, string) error {
	return nil
}

type oauthTestLimiter struct{ err error }

func (l *oauthTestLimiter) Allow(context.Context, string, int, time.Duration) error {
	return l.err
}

var _ domainauth.RateLimiter = (*oauthTestLimiter)(nil)

// newAuthorizeTestHandler 组装 OAuthHandler（miniredis + 内存桩，无需数据库）。
func newAuthorizeTestHandler(t *testing.T, projectSettings map[string]any, providerEnabled bool, limiter domainauth.RateLimiter) (*OAuthHandler, string) {
	t.Helper()
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := &config.AppConfig{
		Server: &config.Server{Http: &config.Http{PublicUrl: "https://auth.example.com"}},
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "oauth-authorize-test-secret"}, // #nosec G101 -- 测试固定值
		},
	}
	project := &projects.Project{
		ID: "p1", InternalID: 1, Name: "P1", Status: "active",
		Settings: projectSettings,
	}
	providerRepo := &oauthTestProviderRepo{}
	if providerEnabled {
		providerRepo.provider = &projects.OAuthProvider{
			Provider: "github", Enabled: true,
			ClientID: "test-client-id", ClientSecret: "test-secret",
			Scopes: []string{"read:user", "user:email"},
		}
	}
	account := client.NewAccount(
		cfg,
		&oauthTestProjectRepo{project: project},
		nil,
		providerRepo,
		nil, // sessions
		nil, // otp
		auth.NewRedisOAuthStateStore(rdb),
		nil, // tokens
		nil, // loginThrottle
		nil, // rotation
		nil, // idGen
		nil, // mailer
		nil, // sms
		nil, // rateLimiter（Account 用例自身不在此路径消费）
		nil, // roles
		nil, // mfa
		nil, // mfaChallenges
		nil, // oneTimeTokens
		nil, // auditRepo
		nil, // usersRepo
		nil, // identities
		nil, // sessionRepo
		auth.NewOAuthAuthenticatorFactory(),
		nil, // weChatExchanger
		nil, // otpGenerator
		nil, // sessionCookies
		nil, // analyticsDeletions
		nil, // uow.Runner
	)
	h, err := NewOAuthHandler(account, cfg, nil, limiter)
	require.NoError(t, err)
	return h, project.ID
}

func authorizeRequest(h *OAuthHandler, provider string, query url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet,
		"/v1/account/oauth2/"+provider+"/authorize?"+query.Encode(), nil)
	rec := httptest.NewRecorder()
	h.authorize(rec, req, map[string]string{"provider": provider})
	return rec
}

func TestOAuthAuthorize_RedirectsToProviderWithNonce(t *testing.T) {
	h, projectID := newAuthorizeTestHandler(t, map[string]any{
		"auth.oauth_allowed_redirect_urls": []any{"https://app.example.com"},
	}, true, nil)

	rec := authorizeRequest(h, "github", url.Values{
		"project_id": {projectID},
		"success":    {"https://app.example.com/auth/callback"},
		"failure":    {"https://app.example.com/login?oauth=failed"},
	})
	require.Equal(t, http.StatusFound, rec.Code)
	loc := rec.Header().Get("Location")
	require.True(t, strings.HasPrefix(loc, "https://github.com/login/oauth/authorize"), loc)
	require.Contains(t, loc, "client_id=test-client-id")
	require.Contains(t, loc, "state=")

	cookies := rec.Result().Cookies()
	var nonce *http.Cookie
	for _, c := range cookies {
		if c.Name == "TORCHWOOD_oauth_nonce_"+projectID {
			nonce = c
		}
	}
	require.NotNil(t, nonce, "必须种下 nonce cookie")
	require.Equal(t, 600, nonce.MaxAge)
	require.True(t, nonce.Secure)
	require.Equal(t, http.SameSiteLaxMode, nonce.SameSite)
	require.True(t, nonce.HttpOnly)
	require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
}

func TestOAuthAuthorize_ProviderDisabled_RedirectsToFailure(t *testing.T) {
	// provider 未启用发生在白名单校验之后：failure URL 已可信，302 回跳。
	h, projectID := newAuthorizeTestHandler(t, map[string]any{
		"auth.oauth_allowed_redirect_urls": []any{"https://app.example.com"},
	}, false, nil)

	rec := authorizeRequest(h, "github", url.Values{
		"project_id": {projectID},
		"success":    {"https://app.example.com/auth/callback"},
		"failure":    {"https://app.example.com/login"},
	})
	require.Equal(t, http.StatusFound, rec.Code)
	require.Equal(t, "https://app.example.com/login?error=oauth_failed", rec.Header().Get("Location"))
}

func TestOAuthAuthorize_ProjectNotFound_Returns400(t *testing.T) {
	// 项目不存在 → 白名单未校验 → failure URL 不可信，必须 400 不跳转。
	h, _ := newAuthorizeTestHandler(t, nil, true, nil)

	rec := authorizeRequest(h, "github", url.Values{
		"project_id": {"missing"},
		"success":    {"https://app.example.com/cb"},
		"failure":    {"https://app.example.com/login"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))
}

func TestOAuthAuthorize_DisallowedSuccess_Returns400(t *testing.T) {
	h, projectID := newAuthorizeTestHandler(t, map[string]any{
		"auth.oauth_allowed_redirect_urls": []any{"https://app.example.com"},
	}, true, nil)

	rec := authorizeRequest(h, "github", url.Values{
		"project_id": {projectID},
		"success":    {"https://evil.example.com/cb"},
		"failure":    {"https://app.example.com/login"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))
}

func TestOAuthAuthorize_MissingParams_Returns400(t *testing.T) {
	h, projectID := newAuthorizeTestHandler(t, nil, true, nil)

	for _, q := range []url.Values{
		{},
		{"project_id": {projectID}},
		{"project_id": {projectID}, "success": {"https://app.example.com/cb"}},
	} {
		rec := authorizeRequest(h, "github", q)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	}
}

func TestOAuthAuthorize_RateLimited(t *testing.T) {
	h, projectID := newAuthorizeTestHandler(t, nil, true,
		&oauthTestLimiter{err: status.Error(codes.ResourceExhausted, "rate limit exceeded")})
	rec := authorizeRequest(h, "github", url.Values{
		"project_id": {projectID},
		"success":    {"https://app.example.com/cb"},
		"failure":    {"https://app.example.com/login"},
	})
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
}

func TestOAuthAuthorize_LimiterInfraError_FailsOpen(t *testing.T) {
	h, projectID := newAuthorizeTestHandler(t, map[string]any{
		"auth.oauth_allowed_redirect_urls": []any{"https://app.example.com"},
	}, true, &oauthTestLimiter{err: status.Error(codes.Internal, "redis down")})

	rec := authorizeRequest(h, "github", url.Values{
		"project_id": {projectID},
		"success":    {"https://app.example.com/cb"},
		"failure":    {"https://app.example.com/login"},
	})
	require.Equal(t, http.StatusFound, rec.Code, "Redis 抖动不应打断登录发起（fail-open）")
}
