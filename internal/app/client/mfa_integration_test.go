package client

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/pquerna/otp/totp"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

// TestMFAFullFlowIntegration 覆盖 MFA 全流程：注册 → 创建因子 → 激活 →
// 登出 → 登录（mfa_required，无会话文档）→ 完成挑战（provider=mfa 会话）
// → 会话可用。
func TestMFAFullFlowIntegration(t *testing.T) {
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

	account := NewTestAccountWithRedis(mfaTestConfig(), projectRepo, db, rdb)

	// 1. 注册。
	user, _, _, mfa, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "mfa-flow@example.com",
		Password:  "User@123",
		Name:      "MFA Flow",
	})
	require.NoError(t, err)
	require.Nil(t, mfa)

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	// 2. 创建 TOTP 因子。
	factor, plainSecret, otpauthURL, err := account.CreateTOTPFactor(userCtx, projectID, user.ID, "")
	require.NoError(t, err)
	require.Contains(t, otpauthURL, "otpauth://totp/")

	// 3. 激活。
	code, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	verified, err := account.VerifyTOTPFactor(userCtx, projectID, user.ID, factor.ID, code)
	require.NoError(t, err)
	require.Equal(t, auth.FactorStatusVerified, verified.Status)

	// 4. 登出（注册会话删除）。
	sessions, err := account.ListSessions(userCtx)
	require.NoError(t, err)
	for _, s := range sessions {
		require.NoError(t, account.DeleteSession(userCtx, s.ID))
	}

	// 5. 再次登录 → mfa_required，无会话文档。
	_, tokens, cookie, challenge, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "mfa-flow@example.com",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.NotNil(t, challenge)
	require.Nil(t, tokens)
	require.Empty(t, cookie)
	require.Len(t, challenge.Factors, 1)
	require.Equal(t, factor.ID, challenge.Factors[0].ID)

	sessionsAfter, err := account.ListSessions(userCtx)
	require.NoError(t, err)
	require.Empty(t, sessionsAfter)

	// 等激活 code 的 60s 防重放窗口过后再完成挑战。
	mr.FastForward(61 * time.Second)

	// 6. 完成挑战 → 签发 provider=mfa 的会话。
	goodCode, err := totp.GenerateCode(plainSecret, time.Now())
	require.NoError(t, err)
	user2, tokens2, cookie2, err := account.CompleteMFASession(ctx, projectID, challenge.Token, factor.ID, goodCode)
	require.NoError(t, err)
	require.Equal(t, user.ID, user2.ID)
	require.NotEmpty(t, tokens2.AccessToken)
	require.NotEmpty(t, cookie2)

	sessionsFinal, err := account.ListSessions(userCtx)
	require.NoError(t, err)
	require.Len(t, sessionsFinal, 1)
	require.Equal(t, auth.ProviderMFA, sessionsFinal[0].Provider)

	// 7. 会话可用：Me + Refresh。
	me, err := account.Me(userCtx)
	require.NoError(t, err)
	require.Equal(t, user.ID, me.ID)

	newTokens, _, err := account.RefreshToken(ctx, RefreshTokenCommand{
		ProjectID:    projectID,
		RefreshToken: tokens2.RefreshToken,
	})
	require.NoError(t, err)
	require.NotEmpty(t, newTokens.AccessToken)
}
