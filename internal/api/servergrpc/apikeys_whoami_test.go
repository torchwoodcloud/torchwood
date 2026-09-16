package servergrpc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
)

// 本文件测试 APIKeysService.WhoAmI 的 handler 映射：判定与行读取语义在
// app 用例层测过（apikeys_test.go SelfDescribe / MaxAgeSeconds），这里聚焦
// proto 字段投影（key_id/name/project_id/scopes 原样 + max_age_seconds）。

type whoamiStubRepo struct {
	key *projects.APIKey
}

func (r *whoamiStubRepo) CreateAPIKey(context.Context, *projects.APIKey) error { return nil }
func (r *whoamiStubRepo) GetAPIKey(context.Context, string, string) (*projects.APIKey, error) {
	return r.key, nil
}
func (r *whoamiStubRepo) GetAPIKeyBySecretHash(context.Context, string) (*projects.APIKey, error) {
	return nil, nil
}
func (r *whoamiStubRepo) ListAPIKeys(context.Context, string) ([]projects.APIKey, error) {
	return nil, nil
}
func (r *whoamiStubRepo) UpdateAPIKey(context.Context, string, string, map[string]any) error {
	return nil
}
func (r *whoamiStubRepo) DeleteAPIKey(context.Context, string, string) error { return nil }

func newTestAPIKeysService(key *projects.APIKey) *APIKeysService {
	return NewAPIKeysService(appserver.NewAPIKeys(&whoamiStubRepo{key: key}, whoamiVocabulary()))
}

func whoamiVocabulary() *domainauth.ScopeVocabulary {
	set, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{{
		Method: "/t/users.read", Service: "/t",
		Access: domainauth.AccessServer,
		Scope:  &domainauth.ScopeRule{Resource: domainauth.ScopeUsers, Op: domainauth.ScopeRead},
	}})
	if err != nil {
		panic(err)
	}
	return domainauth.VocabularyFromPolicies(set)
}

func whoamiAPIKeyCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey, ProjectID: "acme", APIKeyID: "key-1",
	})
}

// TestAPIKeysService_WhoAmI_MapsKeyRow：API key 主体 → 四字段原样投影 +
// max_age_seconds 服务端计算。
func TestAPIKeysService_WhoAmI_MapsKeyRow(t *testing.T) {
	exp := time.Now().Add(2 * time.Minute).UTC()
	key := &projects.APIKey{
		ID: "key-1", ProjectID: "acme", Name: "tenant",
		Scopes:   []string{"users.read", "messageloop.session.act"},
		ExpireAt: &exp, Enabled: true,
	}
	svc := newTestAPIKeysService(key)

	resp, err := svc.WhoAmI(whoamiAPIKeyCtx(), nil)
	require.NoError(t, err)
	require.Equal(t, "key-1", resp.GetKeyId())
	require.Equal(t, "tenant", resp.GetName())
	require.Equal(t, "acme", resp.GetProjectId())
	require.Equal(t, []string{"users.read", "messageloop.session.act"}, resp.GetScopes())
	require.Greater(t, resp.GetMaxAgeSeconds(), int64(110))
	require.LessOrEqual(t, resp.GetMaxAgeSeconds(), int64(120))
}

// TestAPIKeysService_WhoAmI_NoExpiry：未设 expire_at → max_age_seconds = 0。
func TestAPIKeysService_WhoAmI_NoExpiry(t *testing.T) {
	svc := newTestAPIKeysService(&projects.APIKey{
		ID: "key-1", ProjectID: "acme", Name: "tenant",
		Scopes: []string{"*"}, Enabled: true,
	})
	resp, err := svc.WhoAmI(whoamiAPIKeyCtx(), nil)
	require.NoError(t, err)
	require.EqualValues(t, 0, resp.GetMaxAgeSeconds())
}

// TestAPIKeysService_WhoAmI_NonAPIKeyCredential：匿名 / admin 会话 / 端用户
// 没有 key 可述 → 401（该端点只对 API key 凭证有意义）。
func TestAPIKeysService_WhoAmI_NonAPIKeyCredential(t *testing.T) {
	svc := newTestAPIKeysService(&projects.APIKey{ID: "key-1", ProjectID: "acme", Enabled: true})

	for name, ctx := range map[string]context.Context{
		"anonymous": context.Background(),
		"admin": contexts.WithPrincipal(context.Background(), &shared.Principal{
			ActorID: "admin-1", ActorKind: shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeToken,
		}),
		"end_user": contexts.WithPrincipal(context.Background(), &shared.Principal{
			ActorID: "u1", ActorKind: shared.ActorKindEndUser,
			CredentialType: shared.CredentialTypeToken, ProjectID: "acme", UserID: "u1",
		}),
	} {
		_, err := svc.WhoAmI(ctx, nil)
		require.Equal(t, codes.Unauthenticated, status.Code(err), "%s 主体应 401", name)
	}
}
