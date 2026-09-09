package auth_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testSessionJWTSecret = "session-service-test-secret"

func testSessionJWTConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: testSessionJWTSecret},
		},
	}
}

type stubRoleResolver struct{}

func (stubRoleResolver) LoadUserRoles(_ context.Context, _, userID string, _ *users.User) ([]string, error) {
	return []string{"users", "user:" + userID}, nil
}

func TestSessionService_RecordsClientInfo(t *testing.T) {
	t.Parallel()

	// Unit-level check: CreateSessionAndTokens reads ClientInfo from context.
	// Full integration is covered by account integration tests.
	svc := auth.NewSessionService(nil, nil, stubRoleResolver{}, nil)
	require.NotNil(t, svc)

	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{
		IP:        "203.0.113.10",
		UserAgent: "TorchwoodTest/1.0",
	})
	info := contexts.ClientInfoFrom(ctx)
	require.Equal(t, "203.0.113.10", info.IP)
	require.Equal(t, "TorchwoodTest/1.0", info.UserAgent)
}

func TestSessionService_EnsureActiveSession_CorruptExpireAtFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-bad", UserID: "user-1"})
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-ok", UserID: "user-1", ExpireAt: time.Now().Add(time.Hour)})
	svc := auth.NewSessionService(nil, sessions, nil, nil)

	err := svc.EnsureActiveSession(ctx, "proj-1", "sess-bad", "user-1")
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())

	require.NoError(t, svc.EnsureActiveSession(ctx, "proj-1", "sess-ok", "user-1"))
}

func TestSessionService_CreateSessionStoresHashedSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	svc := auth.NewSessionService(testSessionJWTConfig(), sessions, stubRoleResolver{}, nil)

	bundle, cookie, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "email")
	require.NoError(t, err)
	require.NotNil(t, bundle)
	require.NotEmpty(t, bundle.AccessToken)
	require.NotEmpty(t, bundle.RefreshToken)
	require.Equal(t, 1, sessions.len("proj-1"))

	var sessionID string
	var hash string
	for _, id := range sessions.ids("proj-1") {
		sessionID = id
		hash = sessions.get("proj-1", id).SecretHash
	}
	require.Len(t, hash, 64, "secret_hash 必须是 SHA-256 hex")
	_, decodeErr := hex.DecodeString(hash)
	require.NoError(t, decodeErr)
	require.NotContains(t, hash, "-", "secret_hash 不得是 UUID 原文")
	require.NotEqual(t, sessionID, hash)

	codec := auth.NewSessionCookieCodec(string(jwtparser.DeriveKey(testSessionJWTSecret, jwtparser.PurposeSessionCookie)))
	gotProject, gotSession, err := codec.Verify(cookie)
	require.NoError(t, err)
	require.Equal(t, "proj-1", gotProject)
	require.Equal(t, sessionID, gotSession)
	require.NotContains(t, cookie, hash)

	claims, parsed := jwtparser.Parse(jwtparser.DeriveKey(testSessionJWTSecret, jwtparser.PurposeEndUserJWT), bundle.AccessToken)
	require.True(t, parsed)
	require.Equal(t, sessionID, claims.SessionID)
	require.NotEqual(t, hash, claims.SessionID)
}

// M5 C2（评审 B-1 补偿控制）：短时会话通路——会话 15min；access/refresh 的
// exp-iat 被 1h 硬上限常量封顶，敌意配置（access_ttl/refresh_ttl 拉长）无法突破。
func TestSessionService_CreateShortLivedSessionAndTokens_HardCapsTTL(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	hostileCfg := testSessionJWTConfig()
	hostileCfg.Security.Jwt.AccessTtl = "24h"
	hostileCfg.Security.Jwt.RefreshTtl = "168h"
	sessions := newStubSessionRepo()
	svc := auth.NewSessionService(hostileCfg, sessions, stubRoleResolver{}, nil)

	before := time.Now()
	bundle, _, err := svc.CreateShortLivedSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "server_token")
	require.NoError(t, err)
	require.NotEmpty(t, bundle.AccessToken)
	require.NotEmpty(t, bundle.RefreshToken)

	hardCap := int64(auth.CreateUserTokenMaxSessionTTL.Seconds())
	accessClaims, ok := jwtparser.Parse(jwtparser.DeriveKey(testSessionJWTSecret, jwtparser.PurposeEndUserJWT), bundle.AccessToken)
	require.True(t, ok)
	require.LessOrEqual(t, accessClaims.ExpiresAt-accessClaims.IssuedAt, hardCap, "access exp-iat 不得超过 1h 硬上限")

	refreshClaims, ok := jwtparser.Parse(jwtparser.DeriveKey(testSessionJWTSecret, jwtparser.PurposeEndUserJWT), bundle.RefreshToken)
	require.True(t, ok)
	require.LessOrEqual(t, refreshClaims.ExpiresAt-refreshClaims.IssuedAt, hardCap, "refresh exp-iat 不得超过 1h 硬上限")

	// 会话行 TTL 目标 15min（允许小幅时钟偏移），远小于默认 7 天。
	require.Len(t, sessions.ids("proj-1"), 1)
	sess := sessions.get("proj-1", sessions.ids("proj-1")[0])
	ttl := sess.ExpireAt.Sub(before)
	require.Greater(t, ttl, 14*time.Minute)
	require.LessOrEqual(t, ttl, auth.CreateUserTokenSessionTTL+time.Minute)
	require.Equal(t, "server_token", sess.Provider)
}

// 短时会话在默认配置下 access 同样落在目标 TTL 一侧（不超 1h，且与配置一致时取配置值）。
func TestSessionService_CreateShortLivedSessionAndTokens_DefaultConfig(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	svc := auth.NewSessionService(testSessionJWTConfig(), sessions, stubRoleResolver{}, nil)

	bundle, _, err := svc.CreateShortLivedSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)
	accessClaims, ok := jwtparser.Parse(jwtparser.DeriveKey(testSessionJWTSecret, jwtparser.PurposeEndUserJWT), bundle.AccessToken)
	require.True(t, ok)
	require.LessOrEqual(t, accessClaims.ExpiresAt-accessClaims.IssuedAt, int64((15*time.Minute + time.Minute).Seconds()))
}

func TestProviderConstants(t *testing.T) {
	t.Parallel()
	require.Equal(t, "email", domainauth.ProviderEmail)
	require.Equal(t, "wechat_web", domainauth.ProviderWeChatWeb)
}

// g3SessionData 构造会话文档数据（expire_at 为 RFC3339Nano，字符串序=时间序）.

//nolint:unused
func g3SessionData(expireAt time.Time) map[string]any {
	return map[string]any{
		"user_id":     "user-1",
		"expire_at":   expireAt.Format(time.RFC3339Nano),
		"provider":    "email",
		"secret_hash": "secret",
	}
}

// TestSessionService_EvictsOldestSessionsOverLimit（R05-P1-6）：max_per_user=2
// 且已有 3 个会话时，CreateSessionAndTokens 必须先淘汰最旧 2 个再创建。
func TestSessionService_EvictsOldestSessionsOverLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Now()
	sessions := newStubSessionRepo()
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-oldest", UserID: "user-1", ExpireAt: now.Add(-3 * time.Hour)})
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-middle", UserID: "user-1", ExpireAt: now.Add(-2 * time.Hour)})
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-newest", UserID: "user-1", ExpireAt: now.Add(-time.Hour)})
	cfg := &config.AppConfig{Security: &config.Security{Sessions: &config.Security_Sessions{MaxPerUser: 2}}}
	svc := auth.NewSessionService(cfg, sessions, stubRoleResolver{}, nil)

	_, _, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)

	require.Equal(t, 2, sessions.len("proj-1"))
	require.Nil(t, sessions.get("proj-1", "sess-oldest"), "最旧会话必须被淘汰")
	require.Nil(t, sessions.get("proj-1", "sess-middle"), "次旧会话必须被淘汰")
	require.NotNil(t, sessions.get("proj-1", "sess-newest"), "最新会话必须保留")
}

func seedUserSessions(r *stubSessionRepo, count int) {
	now := time.Now()
	for i := 0; i < count; i++ {
		r.seed("proj-1", &domainauth.Session{
			ID:       fmt.Sprintf("sess-%d", i),
			UserID:   "user-1",
			ExpireAt: now.Add(-time.Duration(count-i) * time.Hour),
		})
	}
}

func TestSessionService_NoEvictionUnderLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-a", UserID: "user-1", ExpireAt: time.Now().Add(-time.Hour)})
	cfg := &config.AppConfig{Security: &config.Security{Sessions: &config.Security_Sessions{MaxPerUser: 5}}}
	svc := auth.NewSessionService(cfg, sessions, stubRoleResolver{}, nil)

	_, _, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)
	require.Equal(t, 2, sessions.len("proj-1"))
	require.NotNil(t, sessions.get("proj-1", "sess-a"))
}

func TestSessionService_DefaultLimitAppliedWhenUnset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	seedUserSessions(sessions, 50)
	svc := auth.NewSessionService(&config.AppConfig{}, sessions, stubRoleResolver{}, nil)

	_, _, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)
	require.Equal(t, 50, sessions.len("proj-1"), "未配置时必须按默认 50 淘汰")
	require.Nil(t, sessions.get("proj-1", "sess-0"), "最旧会话必须被淘汰")
}

func TestSessionService_ExplicitZeroUsesDefaultLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	seedUserSessions(sessions, 51)
	cfg := &config.AppConfig{Security: &config.Security{Sessions: &config.Security_Sessions{MaxPerUser: 0}}}
	svc := auth.NewSessionService(cfg, sessions, stubRoleResolver{}, nil)

	_, _, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)
	require.Equal(t, 50, sessions.len("proj-1"), "显式 0 必须回退默认 50")
}

func TestSessionService_ExplicitMinusOneUnlimited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	seedUserSessions(sessions, 55)
	cfg := &config.AppConfig{Security: &config.Security{Sessions: &config.Security_Sessions{MaxPerUser: -1}}}
	svc := auth.NewSessionService(cfg, sessions, stubRoleResolver{}, nil)

	_, _, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)
	require.Equal(t, 56, sessions.len("proj-1"), "-1 = 不限，不得淘汰")
}

func TestSessionService_ExplicitLimitEvictsOldest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	seedUserSessions(sessions, 15)
	cfg := &config.AppConfig{Security: &config.Security{Sessions: &config.Security_Sessions{MaxPerUser: 10}}}
	svc := auth.NewSessionService(cfg, sessions, stubRoleResolver{}, nil)

	_, _, err := svc.CreateSessionAndTokens(ctx, "proj-1", "user-1", "user@example.com", "")
	require.NoError(t, err)
	require.Equal(t, 10, sessions.len("proj-1"), "显式 10 必须淘汰至上限")
	require.Nil(t, sessions.get("proj-1", "sess-0"), "最旧会话必须被淘汰")
	require.NotNil(t, sessions.get("proj-1", "sess-14"), "最新会话必须保留")
}

func TestSessionService_DeleteSessionsByUser_BulkDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sessions := newStubSessionRepo()
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-1", UserID: "user-1", ExpireAt: time.Now().Add(time.Hour)})
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-2", UserID: "user-1", ExpireAt: time.Now().Add(time.Hour)})
	sessions.seed("proj-1", &domainauth.Session{ID: "sess-3", UserID: "user-2", ExpireAt: time.Now().Add(time.Hour)})
	svc := auth.NewSessionService(nil, sessions, nil, nil)

	require.NoError(t, svc.DeleteSessionsByUser(ctx, "proj-1", "user-1"))
	require.Equal(t, 1, sessions.len("proj-1"))
	require.NotNil(t, sessions.get("proj-1", "sess-3"), "其他用户的会话不得被误删")
}

// TestSessionService_ImpersonationClaim（B2）：admin 调用方（CreateUserToken 路径）
// 签发的 end_user JWT 带 imp=impersonator admin id；普通登录（无 admin principal）
// imp 为空。签发点共用 CreateSessionAndTokens，此处直接断言 claims。
func TestSessionService_ImpersonationClaim(t *testing.T) {
	t.Parallel()

	sessions := newStubSessionRepo()
	svc := auth.NewSessionService(testSessionJWTConfig(), sessions, stubRoleResolver{}, nil)
	cfg := testSessionJWTConfig()

	adminCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind:       shared.ActorKindAdmin,
		AdminID:         "adm-imp-1",
		ProjectID:       "p1",
		IsPlatformAdmin: true,
	})
	bundle, _, err := svc.CreateSessionAndTokens(adminCtx, "p1", "u1", "u1@test.local", "server_token")
	require.NoError(t, err)

	key := jwtparser.DeriveKey(cfg.GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeEndUserJWT)
	claims, ok := jwtparser.Parse(key, bundle.AccessToken)
	require.True(t, ok)
	require.Equal(t, "adm-imp-1", claims.Imp, "模拟登录的 access token 必须带 impersonator admin id")

	userCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: "p1",
		UserID:    "u1",
	})
	bundle2, _, err := svc.CreateSessionAndTokens(userCtx, "p1", "u1", "u1@test.local", "")
	require.NoError(t, err)
	claims2, ok := jwtparser.Parse(key, bundle2.AccessToken)
	require.True(t, ok)
	require.Empty(t, claims2.Imp, "普通登录不得携带 imp")
}
