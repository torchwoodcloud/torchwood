package client

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func jwtTestConfig() *config.AppConfig {
	return &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "jwt-app-test-secret"}, // #nosec G101 -- 测试固定值
		},
	}
}

func TestAccount_CreateJWT(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccountWithRedis(jwtTestConfig(), projectRepo, db, rdb)

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "jwt-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	token, err := account.CreateJWT(userCtx)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	claims, ok := jwtparser.Parse(jwtparser.DeriveKey(jwtTestConfig().GetSecurity().GetJwt().GetSecret(), jwtparser.PurposeEndUserJWT), token)
	require.True(t, ok)
	require.Equal(t, user.ID, claims.UserID)
	require.Equal(t, projectID, claims.ProjectID)
	require.Equal(t, "end_user", claims.ActorKind)
	require.Equal(t, jwtparser.TokenTypeAccess, claims.TokenType)
	require.Equal(t, user.Email, claims.Username)
	require.Contains(t, claims.Roles, "user:"+user.ID)

	// 一次性 JWT：携带唯一 jti（TokenID）与一次性消费记录（5min TTL）。
	require.NotEmpty(t, claims.TokenID)
	n, err := rdb.Exists(ctx, "Torchwood:jwt:one-time:"+claims.TokenID).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), n)

	// TTL ≈ 5 分钟。
	ttl := time.Until(time.Unix(claims.ExpiresAt, 0))
	require.InDelta(t, 5*time.Minute, ttl, float64(30*time.Second))
	require.WithinDuration(t, time.Now().Add(5*time.Minute), time.Unix(claims.ExpiresAt, 0), 30*time.Second)
}

func TestAccount_CreateJWT_Unauthenticated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccount(jwtTestConfig(), projectRepo, db)

	// 无 principal → 401。
	_, err := account.CreateJWT(ctx)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())

	// 先建立系统集合（SignUp），再查不存在的用户 → 404。
	_, _, _, _, err = account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "jwt-setup@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    "no-such-user",
		Roles:     []string{"users"},
	})
	_, err = account.CreateJWT(userCtx)
	require.Error(t, err)
	st, _ = status.FromError(err)
	require.Equal(t, codes.NotFound, st.Code())
}

// TestAccount_CreateJWT_SecondUseRejected（R05-P2-8 端到端）：签发方
// Register 的消费记录必须被验证方 Consume 原子消费——同一 JWT 二次
// 提交给 Validator 必须 Unauthenticated，普通 access token 不受影响。
func TestAccount_CreateJWT_SecondUseRejected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccountWithRedis(jwtTestConfig(), projectRepo, db, rdb)

	user, tokens, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "jwt-onetime@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotNil(t, tokens)

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	oneTime, err := account.CreateJWT(userCtx)
	require.NoError(t, err)

	validator := auth.NewValidatorWithOneTimeTokens(
		jwtTestConfig(),
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
		auth.NewRedisOneTimeTokenStore(rdb),
		nil,
	)

	// 第一次验证放行。
	p, err := validator.ValidateToken(ctx, oneTime)
	require.NoError(t, err)
	require.Equal(t, user.ID, p.UserID)

	// 同一 JWT 二次使用被拒（消费记录已 GETDEL）。
	_, err = validator.ValidateToken(ctx, oneTime)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())

	// 普通 access token（SignUp 签发）不受一次性消费路径影响。
	_, err = validator.ValidateToken(ctx, tokens.AccessToken)
	require.NoError(t, err)
}
