package consolegrpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	consolev1 "github.com/torchwooddev/torchwood/genproto/console/v1"
	"github.com/torchwooddev/torchwood/internal/app/console"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/pkg/jwtparser"
	"github.com/torchwooddev/torchwood/pkg/password"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// captureTransportStream 是一个 grpc.ServerTransportStream 桩，捕获 handler
// 通过 grpc.SetHeader 写入的 metadata（直接调用 handler 时 context 里没有
// transport stream，SetHeader 会失败）。
type captureTransportStream struct {
	header metadata.MD
}

func (s *captureTransportStream) Method() string { return "/console.v1.ConsoleAuthService/test" }
func (s *captureTransportStream) SetHeader(md metadata.MD) error {
	s.header = metadata.Join(s.header, md)
	return nil
}
func (s *captureTransportStream) SendHeader(metadata.MD) error { return nil }
func (s *captureTransportStream) SetTrailer(metadata.MD) error { return nil }

var _ grpc.ServerTransportStream = (*captureTransportStream)(nil)

func testCtx(t *testing.T) (context.Context, *captureTransportStream) {
	t.Helper()
	stream := &captureTransportStream{}
	return grpc.NewContextWithServerTransportStream(context.Background(), stream), stream
}

func testConfig(publicURL string) *config.AppConfig {
	return &config.AppConfig{
		Server: &config.Server{
			Http: &config.Http{PublicUrl: publicURL},
		},
		Security: &config.Security{
			Jwt: &config.Security_Jwt{
				Secret:     "console-cookie-test-secret",
				AccessTtl:  "24h",
				RefreshTtl: "168h",
			},
		},
	}
}

type stubAdminRepo struct {
	admin *projects.Admin
}

func (r *stubAdminRepo) GetAdmin(_ context.Context, id string) (*projects.Admin, error) {
	if r.admin != nil && r.admin.ID == id {
		return r.admin, nil
	}
	return nil, nil
}

func (r *stubAdminRepo) GetAdminByEmail(_ context.Context, email string) (*projects.Admin, error) {
	if r.admin != nil && r.admin.Email == email {
		return r.admin, nil
	}
	return nil, nil
}

func (r *stubAdminRepo) ListAdmins(context.Context) ([]projects.Admin, error) {
	if r.admin == nil {
		return nil, nil
	}
	return []projects.Admin{*r.admin}, nil
}

func (r *stubAdminRepo) CreateAdmin(context.Context, *projects.Admin) error {
	return nil
}

func (r *stubAdminRepo) UpdateAdmin(context.Context, *projects.Admin) error {
	return nil
}

func (r *stubAdminRepo) RevokeCredentials(_ context.Context, _ string, _ time.Time) error {
	return nil
}

func (r *stubAdminRepo) DeleteAdmin(context.Context, string) error {
	return nil
}

func (r *stubAdminRepo) CountAdminsByRole(context.Context, string) (int64, error) {
	return 1, nil
}

func (r *stubAdminRepo) WithBootstrapLock(_ context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(context.Background())
}

var _ projects.AdminRepository = (*stubAdminRepo)(nil)

func newSignInService(t *testing.T, cfg *config.AppConfig) *AuthService {
	t.Helper()
	hash, err := password.Hash("Admin@123")
	require.NoError(t, err)
	repo := &stubAdminRepo{admin: &projects.Admin{
		ID:           "admin-1",
		Email:        "admin@torchwood.local",
		PasswordHash: hash,
		Role:         "admin",
	}}
	return NewAuthService(console.NewAuth(cfg, repo, nil, nil, nil), newTestSetup())
}

// newTestSetup 构造一个仅用于既有 SignIn/RefreshToken/SignOut 测试的 Setup
// 桩（SignUp/GetSetupStatus 相关测试不依赖其内部字段）。
func newTestSetup() *console.Setup {
	return console.NewSetup(nil, nil, nil, nil, nil, nil, nil)
}

// findCookie 在 set-cookie metadata 值里按 cookie 名查找。
func findCookie(t *testing.T, stream *captureTransportStream, name string) string {
	t.Helper()
	for _, raw := range stream.header.Get("set-cookie") {
		if strings.HasPrefix(raw, name+"=") {
			return raw
		}
	}
	t.Fatalf("cookie %q not found in set-cookie headers: %v", name, stream.header.Get("set-cookie"))
	return ""
}

func TestSignIn_IssuesSessionCookies(t *testing.T) {
	t.Parallel()
	svc := newSignInService(t, testConfig("http://localhost:9099"))
	ctx, stream := testCtx(t)

	_, err := svc.SignIn(ctx, &consolev1.SignInRequest{
		Email:    "admin@torchwood.local",
		Password: "Admin@123",
	})
	require.NoError(t, err)

	access := findCookie(t, stream, "TORCHWOOD_session_console")
	require.Contains(t, access, "Path=/")
	require.Contains(t, access, "HttpOnly")
	require.Contains(t, access, "SameSite=Lax")
	// R4 J7-1：console access TTL 默认收紧为 1h（硬性封顶），cookie Max-Age 同步。
	require.Contains(t, access, "Max-Age=3600")
	require.NotContains(t, access, "Secure")

	refresh := findCookie(t, stream, "TORCHWOOD_console_refresh")
	require.Contains(t, refresh, "Path=/v1/console/auth")
	require.Contains(t, refresh, "HttpOnly")
	require.Contains(t, refresh, "SameSite=Lax")
	require.Contains(t, refresh, "Max-Age=604800")
	require.NotContains(t, refresh, "Secure")
}

func TestSignIn_SecureCookiesOnTLS(t *testing.T) {
	t.Parallel()
	svc := newSignInService(t, testConfig("https://console.example.com"))
	ctx, stream := testCtx(t)

	_, err := svc.SignIn(ctx, &consolev1.SignInRequest{
		Email:    "admin@torchwood.local",
		Password: "Admin@123",
	})
	require.NoError(t, err)

	require.Contains(t, findCookie(t, stream, "TORCHWOOD_session_console"), "Secure")
	require.Contains(t, findCookie(t, stream, "TORCHWOOD_console_refresh"), "Secure")
}

func adminRefreshToken(t *testing.T, cfg *config.AppConfig) string {
	t.Helper()
	token, err := jwtparser.Generate(jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), jwtparser.Claims{
		TokenID:   "tid-1",
		UserID:    "admin-1",
		Username:  "admin@torchwood.local",
		ActorKind: "admin",
		Roles:     []string{"admin"},
		TokenType: jwtparser.TokenTypeRefresh,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	return token
}

func TestRefreshToken_ReadsCookieWhenBodyEmpty(t *testing.T) {
	t.Parallel()
	cfg := testConfig("http://localhost:9099")
	svc := NewAuthService(console.NewAuth(cfg, &stubAdminRepo{admin: &projects.Admin{
		ID:    "admin-1",
		Email: "admin@torchwood.local",
		Role:  "admin",
	}}, nil, nil, nil), newTestSetup())
	ctx, stream := testCtx(t)
	refresh := adminRefreshToken(t, cfg)
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		"cookie", "other=1; TORCHWOOD_console_refresh="+refresh,
	))

	res, err := svc.RefreshToken(ctx, &consolev1.RefreshTokenRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, res.GetAccessToken())
	// 刷新成功后重新下发两个 cookie（rotation 后的新 refresh token）。
	findCookie(t, stream, "TORCHWOOD_session_console")
	findCookie(t, stream, "TORCHWOOD_console_refresh")
}

func TestRefreshToken_BodyTakesPrecedenceOverCookie(t *testing.T) {
	t.Parallel()
	cfg := testConfig("http://localhost:9099")
	svc := NewAuthService(console.NewAuth(cfg, &stubAdminRepo{admin: &projects.Admin{
		ID:    "admin-1",
		Email: "admin@torchwood.local",
		Role:  "admin",
	}}, nil, nil, nil), newTestSetup())
	ctx, _ := testCtx(t)
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(
		"cookie", "TORCHWOOD_console_refresh=forged-token",
	))

	res, err := svc.RefreshToken(ctx, &consolev1.RefreshTokenRequest{
		RefreshToken: adminRefreshToken(t, cfg),
	})
	require.NoError(t, err)
	require.NotEmpty(t, res.GetAccessToken())
}

func TestSignOut_ClearsSessionCookies(t *testing.T) {
	t.Parallel()
	svc := NewAuthService(console.NewAuth(testConfig("http://localhost:9099"), nil, nil, nil, nil), newTestSetup())
	ctx, stream := testCtx(t)

	_, err := svc.SignOut(ctx, &consolev1.SignOutRequest{})
	require.NoError(t, err)

	access := findCookie(t, stream, "TORCHWOOD_session_console")
	require.Contains(t, access, "Max-Age=0")
	require.Contains(t, access, "Path=/")

	refresh := findCookie(t, stream, "TORCHWOOD_console_refresh")
	require.Contains(t, refresh, "Max-Age=0")
	require.Contains(t, refresh, "Path=/v1/console/auth")
}
