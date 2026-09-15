package servergrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件测试 AuthService handler 的 proto ↔ 用例映射（判定语义在 app 用例
// 层测过真实校验器，这里用 CredentialVerifier 桩聚焦转换）。

type stubCredentialVerifier struct {
	principal   *shared.Principal
	err         error
	gotRaw      string
	gotType     shared.CredentialType
	parseOK     bool
	parseClaims *jwtparser.Claims
}

func (s *stubCredentialVerifier) ValidateCredential(_ context.Context, raw string, t shared.CredentialType) (*shared.Principal, error) {
	s.gotRaw = raw
	s.gotType = t
	if s.err != nil {
		return nil, s.err
	}
	return s.principal, nil
}

func (s *stubCredentialVerifier) ParseClaims(raw string) (*jwtparser.Claims, bool) {
	return s.parseClaims, s.parseOK
}

type stubVerifyAPIKeyRepo struct {
	key *projects.APIKey
}

func (r *stubVerifyAPIKeyRepo) CreateAPIKey(context.Context, *projects.APIKey) error { return nil }
func (r *stubVerifyAPIKeyRepo) GetAPIKey(context.Context, string, string) (*projects.APIKey, error) {
	return r.key, nil
}
func (r *stubVerifyAPIKeyRepo) GetAPIKeyBySecretHash(context.Context, string) (*projects.APIKey, error) {
	return nil, nil
}
func (r *stubVerifyAPIKeyRepo) ListAPIKeys(context.Context, string) ([]projects.APIKey, error) {
	return nil, nil
}
func (r *stubVerifyAPIKeyRepo) UpdateAPIKey(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *stubVerifyAPIKeyRepo) DeleteAPIKey(context.Context, string, string) error { return nil }

func newTestAuthService(v domainauth.CredentialVerifier, keys *stubVerifyAPIKeyRepo) *AuthService {
	return NewAuthService(appserver.NewAuth(v, keys, nil, nil))
}

func verifyAdminCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindAdmin, IsPlatformAdmin: true,
	})
}

// TestAuthService_VerifyToken_MapsEndUserResult：合法端用户判定 → proto 字段
// 全量映射（roles/groups/labels/actor_kind/expires_at）。
func TestAuthService_VerifyToken_MapsEndUserResult(t *testing.T) {
	claims := &jwtparser.Claims{ExpiresAt: time.Now().Add(30 * time.Minute).Unix()}
	v := &stubCredentialVerifier{
		parseOK:     true,
		parseClaims: claims,
		principal: &shared.Principal{
			ActorKind: shared.ActorKindEndUser,
			ProjectID: "proj-1",
			UserID:    "user-1",
			SessionID: "sess-1",
			Email:     "u@example.com",
			Roles:     []string{"users", "user:user-1"},
		},
	}
	s := newTestAuthService(v, nil)

	resp, err := s.VerifyToken(verifyAdminCtx(), &serverv1.VerifyTokenRequest{
		Token: "jwt-raw",
		Type:  serverv1.VerifyCredentialType_VERIFY_CREDENTIAL_TYPE_TOKEN,
	})
	require.NoError(t, err)
	require.True(t, resp.GetValid())
	require.Equal(t, "user-1", resp.GetUserId())
	require.Equal(t, "u@example.com", resp.GetUsername())
	require.Equal(t, "proj-1", resp.GetProjectId())
	require.Equal(t, "sess-1", resp.GetSessionId())
	require.Equal(t, "end_user", resp.GetActorKind())
	require.Equal(t, []string{"users", "user:user-1"}, resp.GetRoles())
	require.Empty(t, resp.GetApiKeyId())
	require.NotNil(t, resp.GetExpiresAt())
	require.Equal(t, time.Unix(claims.ExpiresAt, 0).UTC(), resp.GetExpiresAt().AsTime())
	// 桩收到原文与正确凭证类型。
	require.Equal(t, "jwt-raw", v.gotRaw)
	require.Equal(t, shared.CredentialTypeToken, v.gotType)
}

// TestAuthService_VerifyToken_InvalidIsNotError：校验失败（Unauthenticated）
// 映射为 valid=false + 无错误（HTTP 200），不透传 401。
func TestAuthService_VerifyToken_InvalidIsNotError(t *testing.T) {
	v := &stubCredentialVerifier{
		parseOK:     true,
		parseClaims: &jwtparser.Claims{ExpiresAt: time.Now().Add(time.Minute).Unix()},
		err:         status.Error(codes.Unauthenticated, "session not found or revoked"),
	}
	s := newTestAuthService(v, nil)

	resp, err := s.VerifyToken(verifyAdminCtx(), &serverv1.VerifyTokenRequest{Token: "jwt-raw"})
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.False(t, resp.GetValid())
	require.Empty(t, resp.GetUserId())
	require.Empty(t, resp.GetRoles())
}

// TestAuthService_VerifyToken_MapsAPIKeyResult：API key 主体 → actor_kind=
// service、api_key_id、密钥行 expire_at。
func TestAuthService_VerifyToken_MapsAPIKeyResult(t *testing.T) {
	expireAt := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	v := &stubCredentialVerifier{
		principal: &shared.Principal{
			ActorKind: shared.ActorKindService,
			ProjectID: "proj-1",
			APIKeyID:  "key-1",
			Roles:     []string{"keys", "key:key-1"},
		},
	}
	s := newTestAuthService(v, &stubVerifyAPIKeyRepo{key: &projects.APIKey{
		ID: "key-1", ProjectID: "proj-1", Enabled: true, ExpireAt: &expireAt,
	}})

	resp, err := s.VerifyToken(verifyAdminCtx(), &serverv1.VerifyTokenRequest{
		Token: "sk-xxx",
		Type:  serverv1.VerifyCredentialType_VERIFY_CREDENTIAL_TYPE_API_KEY,
	})
	require.NoError(t, err)
	require.True(t, resp.GetValid())
	require.Equal(t, "service", resp.GetActorKind())
	require.Equal(t, "key-1", resp.GetApiKeyId())
	require.Equal(t, "proj-1", resp.GetProjectId())
	require.Empty(t, resp.GetUserId())
	require.Equal(t, expireAt, resp.GetExpiresAt().AsTime())
	require.Equal(t, shared.CredentialTypeAPIKey, v.gotType)
}
