package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

func TestValidator_Authenticate_ConsoleCookieIsAccessToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	admin := &projects.Admin{ID: "admin-1", Email: "admin@torchwood.local", Role: "owner"}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{admins: map[string]*projects.Admin{admin.ID: admin}}, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	token := signToken(t, jwtparser.Claims{
		UserID:    admin.ID,
		Username:  admin.Email,
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	p, err := v.Authenticate(ctx, shared.AuthnRequest{
		CookieHeaders: []string{shared.ConsoleSessionCookieName + "=" + token},
	})
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindAdmin, p.ActorKind)
	require.Equal(t, shared.CredentialTypeToken, p.CredentialType)
	require.Equal(t, admin.ID, p.AdminID)
	require.Empty(t, p.UserID)
	require.True(t, p.HasRole(shared.RoleConsole))
	require.NotContains(t, p.DocPrincipal().Roles, shared.RoleConsole)
}

func TestValidator_Authenticate_APIKeyIsService(t *testing.T) {
	t.Parallel()
	secret := "torchwood-test-api-key"
	key := &projects.APIKey{ID: "key-1", ProjectID: "proj-1", Scopes: []string{"storage"}, Enabled: true}
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{keys: map[string]*projects.APIKey{hashSecret(secret): key}}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil)

	p, err := v.Authenticate(context.Background(), shared.AuthnRequest{APIKey: []string{secret}})
	require.NoError(t, err)
	require.Equal(t, shared.ActorKindService, p.ActorKind)
	require.Equal(t, shared.CredentialTypeAPIKey, p.CredentialType)
	require.Equal(t, "key-1", p.APIKeyID)
	require.False(t, p.IsSystem())
}

func TestValidator_Authenticate_MultipleCredentials(t *testing.T) {
	t.Parallel()
	v := auth.NewValidator(testValidatorConfig(), &stubAPIKeyRepo{}, nil, &stubAdminRepo{}, &stubAdminProjectRepo{}, nil, nil, nil, nil)
	_, err := v.Authenticate(context.Background(), shared.AuthnRequest{
		Authorization: []string{"Bearer tok"},
		APIKey:        []string{"key"},
	})
	requireCode(t, err, codes.Unauthenticated)
	require.Contains(t, err.Error(), "multiple credentials provided")
}

func TestAuthnRequestFromHTTP_MatchesMetadataShape(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	r.Header.Set("Authorization", "Bearer tok")
	r.Header.Set("X-Api-Key", "k")
	r.AddCookie(&http.Cookie{Name: shared.ConsoleSessionCookieName, Value: "jwt"}) // #nosec G124 -- 客户端测试请求无需安全属性
	httpReq := auth.AuthnRequestFromHTTP(r)

	md := metadata.Pairs(
		"authorization", "Bearer tok",
		"x-api-key", "k",
		"cookie", shared.ConsoleSessionCookieName+"=jwt",
	)
	mdReq := shared.AuthnRequest{
		Authorization: md.Get("authorization"),
		APIKey:        md.Get("x-api-key"),
		CookieHeaders: md.Get("cookie"),
	}
	_, _, errHTTP := shared.ParseAuthnRequest(httpReq)
	_, _, errMD := shared.ParseAuthnRequest(mdReq)
	require.ErrorIs(t, errHTTP, shared.ErrMultipleCredentials)
	require.ErrorIs(t, errMD, shared.ErrMultipleCredentials)
}
