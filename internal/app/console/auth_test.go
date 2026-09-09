package console_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/console"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type memAdminRevokeStore struct {
	revoked map[string]time.Time
}

func newMemAdminRevokeStore() *memAdminRevokeStore {
	return &memAdminRevokeStore{revoked: map[string]time.Time{}}
}

func (s *memAdminRevokeStore) RevokeBefore(_ context.Context, adminID string, revokedAt time.Time, _ time.Duration) error {
	if existing, ok := s.revoked[adminID]; !ok || revokedAt.After(existing) {
		s.revoked[adminID] = revokedAt
	}
	return nil
}

func (s *memAdminRevokeStore) RevokedBefore(_ context.Context, adminID string) (time.Time, error) {
	return s.revoked[adminID], nil
}

var _ domainauth.AdminTokenRevokeStore = (*memAdminRevokeStore)(nil)

func testConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{
				Secret:     "console-auth-test-secret", // #nosec G101 -- 测试固定密钥
				RefreshTtl: "168h",
			},
		},
	}
}

func TestAuth_SignOut_RevokesAdminTokens(t *testing.T) {
	t.Parallel()
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "admin-1",
	})
	store := newMemAdminRevokeStore()
	authUC := console.NewAuth(testConfig(), nil, store, nil, nil)

	require.NoError(t, authUC.SignOut(ctx))
	revoked, err := store.RevokedBefore(ctx, "admin-1")
	require.NoError(t, err)
	require.False(t, revoked.IsZero())
}

func TestAuth_SignOut_ExpiredTokenStillRevokes(t *testing.T) {
	t.Parallel()
	store := newMemAdminRevokeStore()
	authUC := console.NewAuth(testConfig(), nil, store, nil, nil)

	// No principal in context (access token expired); the raw token is only
	// available in the request metadata.
	expiredToken, err := jwtparser.Generate(jwtparser.DeriveKey(testConfig().GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), jwtparser.Claims{
		UserID:    "admin-9",
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Add(-48 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-24 * time.Hour).Unix(),
	})
	require.NoError(t, err)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+expiredToken))

	require.NoError(t, authUC.SignOut(ctx))
	revoked, err := store.RevokedBefore(ctx, "admin-9")
	require.NoError(t, err)
	require.False(t, revoked.IsZero())
}

func TestAuth_SignOut_ExpiredTokenWrongSignatureIgnored(t *testing.T) {
	t.Parallel()
	store := newMemAdminRevokeStore()
	authUC := console.NewAuth(testConfig(), nil, store, nil, nil)

	forgedToken, err := jwtparser.Generate([]byte("another-secret"), jwtparser.Claims{
		UserID:    "admin-9",
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  time.Now().Add(-48 * time.Hour).Unix(),
		ExpiresAt: time.Now().Add(-24 * time.Hour).Unix(),
	})
	require.NoError(t, err)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+forgedToken))

	require.NoError(t, authUC.SignOut(ctx))
	revoked, err := store.RevokedBefore(ctx, "admin-9")
	require.NoError(t, err)
	require.True(t, revoked.IsZero())
}

func TestAuth_RefreshToken_RejectsRevokedAdmin(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	issuedAt := time.Now().Add(-time.Hour)
	store := newMemAdminRevokeStore()
	require.NoError(t, store.RevokeBefore(ctx, "admin-1", time.Now(), time.Hour))

	refreshToken, err := jwtparser.Generate(jwtparser.DeriveKey(testConfig().GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), jwtparser.Claims{
		UserID:    "admin-1",
		Username:  "admin@torchwood.local",
		ActorKind: "admin",
		Roles:     []string{"admin"},
		TokenType: jwtparser.TokenTypeRefresh,
		IssuedAt:  issuedAt.Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	authUC := console.NewAuth(testConfig(), newAdminRepo(mkAdmin("admin-1", "admin@torchwood.local", "admin")), store, nil, nil)
	_, err = authUC.RefreshToken(ctx, console.RefreshTokenCommand{RefreshToken: refreshToken})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

func TestAuth_ValidateCredential_ChecksRevokeStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newMemAdminRevokeStore()
	require.NoError(t, store.RevokeBefore(ctx, "admin-1", time.Now(), time.Hour))

	issuedAt := time.Now().Add(-2 * time.Hour).Unix()
	token, err := jwtparser.Generate(jwtparser.DeriveKey(testConfig().GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), jwtparser.Claims{
		UserID:    "admin-1",
		ActorKind: "admin",
		TokenType: jwtparser.TokenTypeAccess,
		IssuedAt:  issuedAt,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)

	// M5 C1 后撤销判定随 admins 行读出（GetAdmin 先行），测试注入含目标行
	// 的桩仓库，revoked_at 零值时仅 Redis 快路径生效。
	v := auth.NewValidator(
		testConfig(),
		nil,
		nil,
		newAdminRepo(mkAdmin("admin-1", "revoke@x.com", "owner")),
		nil,
		store,
		nil,
		nil,
		nil,
	)
	_, err = v.ValidateToken(ctx, token)
	require.Error(t, err)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// memRotationStore is an in-memory domainauth.RefreshRotationStore for tests.
type memRotationStore struct {
	mu     sync.Mutex
	values map[string]string
}

func newMemRotationStore() *memRotationStore {
	return &memRotationStore{values: map[string]string{}}
}

func (s *memRotationStore) Register(_ context.Context, key, tokenID string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = tokenID
	return nil
}

func (s *memRotationStore) Rotate(_ context.Context, key, presentedTokenID, newTokenID string, _ time.Duration) (domainauth.RotateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.values[key]
	if !ok {
		return domainauth.RotateMissing, nil
	}
	if cur != presentedTokenID {
		return domainauth.RotateMismatch, nil
	}
	s.values[key] = newTokenID
	return domainauth.RotateOK, nil
}

func (s *memRotationStore) current(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[key]
}

var _ domainauth.RefreshRotationStore = (*memRotationStore)(nil)

func adminRefreshToken(t *testing.T, adminID, tokenID string) string {
	t.Helper()
	return adminRefreshTokenWithRoles(t, adminID, tokenID, "admin")
}

func adminRefreshTokenWithRoles(t *testing.T, adminID, tokenID string, roles ...string) string {
	t.Helper()
	token, err := jwtparser.Generate(jwtparser.DeriveKey(testConfig().GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), jwtparser.Claims{
		TokenID:   tokenID,
		UserID:    adminID,
		Username:  "admin@torchwood.local",
		ActorKind: "admin",
		Roles:     roles,
		TokenType: jwtparser.TokenTypeRefresh,
		IssuedAt:  time.Now().Unix(),
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	require.NoError(t, err)
	return token
}

func parseAdminToken(t *testing.T, raw string) *jwtparser.Claims {
	t.Helper()
	claims, ok := jwtparser.Parse(jwtparser.DeriveKey(testConfig().GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeAdminJWT), raw)
	require.True(t, ok)
	return claims
}

func TestAuth_RefreshToken_RotatesToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	revokeStore := newMemAdminRevokeStore()
	rotation := newMemRotationStore()
	key := domainauth.RefreshRotationKey("admin", "admin-1")
	require.NoError(t, rotation.Register(ctx, key, "tid-old", time.Hour))

	authUC := console.NewAuth(testConfig(), newAdminRepo(mkAdmin("admin-1", "admin@torchwood.local", "admin")), revokeStore, nil, rotation)
	pair, err := authUC.RefreshToken(ctx, console.RefreshTokenCommand{
		RefreshToken: adminRefreshToken(t, "admin-1", "tid-old"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, pair.AccessToken)
	require.NotEmpty(t, pair.RefreshToken)
	require.NotEmpty(t, pair.RefreshTokenID)
	require.NotEqual(t, "tid-old", pair.RefreshTokenID)
	require.Equal(t, pair.RefreshTokenID, rotation.current(key))

	// No revocation happened on the happy path.
	revoked, err := revokeStore.RevokedBefore(ctx, "admin-1")
	require.NoError(t, err)
	require.True(t, revoked.IsZero())
}

func TestAuth_RefreshToken_ReuseRevokesAllAdminTokens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	revokeStore := newMemAdminRevokeStore()
	rotation := newMemRotationStore()
	key := domainauth.RefreshRotationKey("admin", "admin-1")
	// The store holds the rotated id; the presented token carries the old one.
	require.NoError(t, rotation.Register(ctx, key, "tid-new", time.Hour))

	authUC := console.NewAuth(testConfig(), newAdminRepo(mkAdmin("admin-1", "admin@torchwood.local", "admin")), revokeStore, nil, rotation)
	_, err := authUC.RefreshToken(ctx, console.RefreshTokenCommand{
		RefreshToken: adminRefreshToken(t, "admin-1", "tid-old"),
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())

	// Reuse detection revokes every token issued for this admin so far.
	revoked, err := revokeStore.RevokedBefore(ctx, "admin-1")
	require.NoError(t, err)
	require.False(t, revoked.IsZero())

	// The stored rotation value was not overwritten by the attacker.
	require.Equal(t, "tid-new", rotation.current(key))
}

func TestAuth_RefreshToken_DeletedAdminUnauthenticated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name         string
		withRotation bool
	}{
		{name: "without_rotation"},
		{name: "with_rotation", withRotation: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rotation domainauth.RefreshRotationStore
			if tc.withRotation {
				store := newMemRotationStore()
				require.NoError(t, store.Register(ctx, domainauth.RefreshRotationKey("admin", "admin-1"), "tid-old", time.Hour))
				rotation = store
			}
			authUC := console.NewAuth(testConfig(), newAdminRepo(), newMemAdminRevokeStore(), nil, rotation)
			_, err := authUC.RefreshToken(ctx, console.RefreshTokenCommand{
				RefreshToken: adminRefreshToken(t, "admin-1", "tid-old"),
			})
			require.Error(t, err)
			st, ok := status.FromError(err)
			require.True(t, ok)
			require.Equal(t, codes.Unauthenticated, st.Code())
			require.NotContains(t, strings.ToLower(st.Message()), "not found")
		})
	}
}

func TestAuth_RefreshToken_UsesDatabaseRoleNotJWTSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := []struct {
		name         string
		withRotation bool
		jwtRoles     []string
	}{
		{name: "without_rotation_owner_to_viewer", jwtRoles: []string{"owner"}},
		{name: "with_rotation_owner_to_viewer", withRotation: true, jwtRoles: []string{"owner"}},
		{name: "empty_jwt_roles_do_not_default_admin", withRotation: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var rotation domainauth.RefreshRotationStore
			if tc.withRotation {
				store := newMemRotationStore()
				require.NoError(t, store.Register(ctx, domainauth.RefreshRotationKey("admin", "admin-1"), "tid-old", time.Hour))
				rotation = store
			}
			authUC := console.NewAuth(
				testConfig(),
				newAdminRepo(mkAdmin("admin-1", "admin@torchwood.local", "viewer")),
				newMemAdminRevokeStore(),
				nil,
				rotation,
			)
			pair, err := authUC.RefreshToken(ctx, console.RefreshTokenCommand{
				RefreshToken: adminRefreshTokenWithRoles(t, "admin-1", "tid-old", tc.jwtRoles...),
			})
			require.NoError(t, err)
			access := parseAdminToken(t, pair.AccessToken)
			require.Equal(t, []string{"viewer"}, access.Roles)
			require.Equal(t, jwtparser.TokenTypeAccess, access.TokenType)
			refresh := parseAdminToken(t, pair.RefreshToken)
			require.Equal(t, []string{"viewer"}, refresh.Roles)
			require.Equal(t, jwtparser.TokenTypeRefresh, refresh.TokenType)
		})
	}
}
