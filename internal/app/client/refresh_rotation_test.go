package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func setupRefreshRotationAccount(t *testing.T) (context.Context, *Account, string, *miniredis.Miniredis) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccountWithRedis(buildTestConfig(), projectRepo, db, rdb)
	return ctx, account, projectID, mr
}

func signUpForRefresh(t *testing.T, ctx context.Context, account *Account, projectID, email string) *TokenBundle {
	t.Helper()
	_, tokens, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     email,
		Password:  "User@123",
		Name:      "Refresh Rotation",
	})
	require.NoError(t, err)
	require.NotEmpty(t, tokens.RefreshToken)
	require.NotEmpty(t, tokens.RefreshTokenID)
	return tokens
}

func parseRefreshClaims(t *testing.T, cfgSecret, token string) *jwtparser.Claims {
	t.Helper()
	claims, ok := jwtparser.Parse(jwtparser.DeriveKey(cfgSecret, jwtparser.PurposeEndUserJWT), token)
	require.True(t, ok)
	return claims
}

func TestAccount_RefreshToken_RotationAndReuseDetection(t *testing.T) {
	ctx, account, projectID, mr := setupRefreshRotationAccount(t)
	tokens := signUpForRefresh(t, ctx, account, projectID, "rotation@torchwood.local")

	// First refresh rotates the token.
	newTokens, _, err := account.RefreshToken(ctx, RefreshTokenCommand{
		ProjectID:    projectID,
		RefreshToken: tokens.RefreshToken,
	})
	require.NoError(t, err)
	require.NotEmpty(t, newTokens.RefreshToken)
	require.NotEqual(t, tokens.RefreshTokenID, newTokens.RefreshTokenID)

	// Re-presenting the old refresh token inside the grace window succeeds
	// against the current chain (multi-tab race) instead of killing the session.
	graceTokens, _, err := account.RefreshToken(ctx, RefreshTokenCommand{
		ProjectID:    projectID,
		RefreshToken: tokens.RefreshToken,
	})
	require.NoError(t, err)
	require.Equal(t, newTokens.RefreshTokenID, graceTokens.RefreshTokenID)

	// After the grace window the old token is treated as reuse again: the
	// session is deleted and every token for it dies.
	mr.FastForward(auth.GraceWindow + time.Second)
	_, _, err = account.RefreshToken(ctx, RefreshTokenCommand{
		ProjectID:    projectID,
		RefreshToken: tokens.RefreshToken,
	})
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())

	claims := parseRefreshClaims(t, buildTestConfig().GetSecurity().GetJwt().GetSecret(), tokens.RefreshToken)
	sess, err := account.sessionRepo.GetByID(ctx, projectID, claims.SessionID)
	require.NoError(t, err)
	require.Nil(t, sess)

	// The rotated token is dead too, because its session was deleted.
	_, _, err = account.RefreshToken(ctx, RefreshTokenCommand{
		ProjectID:    projectID,
		RefreshToken: newTokens.RefreshToken,
	})
	require.Error(t, err)
}

func TestAccount_RefreshToken_ConcurrentRefreshGraceBothSucceed(t *testing.T) {
	ctx, account, projectID, _ := setupRefreshRotationAccount(t)
	tokens := signUpForRefresh(t, ctx, account, projectID, "concurrent@torchwood.local")

	// 双发同一 refresh token:一个正常轮换、一个宽限续签,都成功且收敛到同一链
	// (旧语义下败者被判重用、连坐删会话——console 登录循环的根因之一)。
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	refreshTokenIDs := map[string]struct{}{}
	refreshJWT := tokens.RefreshToken
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pair, _, err := account.RefreshToken(ctx, RefreshTokenCommand{
				ProjectID:    projectID,
				RefreshToken: refreshJWT,
			})
			if err == nil {
				mu.Lock()
				successes++
				refreshTokenIDs[pair.RefreshTokenID] = struct{}{}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 2, successes)
	require.Len(t, refreshTokenIDs, 1)
}
