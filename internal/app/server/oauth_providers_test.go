package server

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNormalizeServerOAuthProvider(t *testing.T) {
	t.Parallel()
	require.Equal(t, domainauth.ProviderGoogle, normalizeServerOAuthProvider("google"))
	require.Equal(t, domainauth.ProviderWeChatWeb, normalizeServerOAuthProvider("wechat_web"))
	require.Equal(t, "", normalizeServerOAuthProvider("unknown"))
}

// stubOAuthProviderRepo 是 OAuthProviderRepository 的内存实现（map + RWMutex）。
type stubOAuthProviderRepo struct {
	mu   sync.RWMutex
	prov map[string]*projects.OAuthProvider
}

func newStubOAuthProviderRepo() *stubOAuthProviderRepo {
	return &stubOAuthProviderRepo{prov: make(map[string]*projects.OAuthProvider)}
}

func (s *stubOAuthProviderRepo) key(projectID, provider string) string {
	return projectID + "/" + provider
}

func (s *stubOAuthProviderRepo) GetOAuthProvider(ctx context.Context, projectID, provider string) (*projects.OAuthProvider, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.prov[s.key(projectID, provider)]
	if !ok {
		return nil, nil
	}
	cp := *p
	cp.Scopes = append([]string(nil), p.Scopes...)
	return &cp, nil
}

func (s *stubOAuthProviderRepo) ListOAuthProviders(ctx context.Context, projectID string) ([]projects.OAuthProvider, error) {
	return nil, nil
}

func (s *stubOAuthProviderRepo) UpsertOAuthProvider(ctx context.Context, cfg *projects.OAuthProvider) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *cfg
	cp.Scopes = append([]string(nil), cfg.Scopes...)
	s.prov[s.key(cfg.ProjectID, cfg.Provider)] = &cp
	return nil
}

func (s *stubOAuthProviderRepo) DeleteOAuthProvider(ctx context.Context, projectID, provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.prov, s.key(projectID, provider))
	return nil
}

func TestOAuthProviders_Upsert_SecretRequiredOnlyWhenEnabled(t *testing.T) {
	t.Parallel()
	repo := newStubOAuthProviderRepo()
	uc := NewOAuthProviders(repo)
	ctx := context.Background()

	// enabled=false 无 secret → 成功创建占位 provider。
	p, err := uc.Upsert(ctx, UpsertOAuthProviderCommand{
		ProjectID: "p1", Provider: "google", Enabled: false, ClientID: "cid",
	})
	require.NoError(t, err)
	require.False(t, p.Enabled)
	require.Equal(t, "cid", p.ClientID)
	require.Empty(t, p.ClientSecret)

	// enabled=true 无 secret → InvalidArgument。
	_, err = uc.Upsert(ctx, UpsertOAuthProviderCommand{
		ProjectID: "p1", Provider: "github", Enabled: true, ClientID: "cid2",
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	// 已有 secret 后 update 不传 secret → 成功且 secret 不变。
	p, err = uc.Upsert(ctx, UpsertOAuthProviderCommand{
		ProjectID: "p1", Provider: "google", Enabled: true, ClientID: "cid", ClientSecret: "secret-1",
	})
	require.NoError(t, err)
	require.Equal(t, "secret-1", p.ClientSecret)

	p, err = uc.Upsert(ctx, UpsertOAuthProviderCommand{
		ProjectID: "p1", Provider: "google", Enabled: true, ClientID: "cid",
	})
	require.NoError(t, err)
	require.True(t, p.Enabled)
	require.Equal(t, "secret-1", p.ClientSecret, "未传 secret 时保留既有 secret")
}
