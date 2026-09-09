package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAccount_SignUpSignInMe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	// Set a JWT secret via the generated struct. Because fields are unexported message
	// types we use a JSON round-trip through Viper in production; for tests we just
	// set the secret through the package-level default used by NewAccount.
	cfg := buildTestConfig()

	projectRepo := bunrepo.NewProjectRepository(db)
	account := NewTestAccount(cfg, projectRepo, db)

	// Sign up.
	user, tokens, cookie, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "account-test@torchwood.local",
		Password:  "User@123",
		Name:      "Account Test",
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	require.NotEmpty(t, tokens.AccessToken)
	require.NotEmpty(t, cookie)

	// Duplicate email.
	_, _, _, _, err = account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "account-test@torchwood.local",
		Password:  "User@123",
		Name:      "Account Test 2",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.AlreadyExists, st.Code())

	// Sign in.
	user2, tokens2, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "account-test@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	require.Equal(t, user.ID, user2.ID)
	require.NotEmpty(t, tokens2.AccessToken)

	// Me with authenticated context.
	meCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})
	me, err := account.Me(meCtx)
	require.NoError(t, err)
	require.Equal(t, user.ID, me.ID)
	require.Equal(t, "account-test@torchwood.local", me.Email)

	// Sign out.
	require.NoError(t, account.SignOut(meCtx))

	// Refresh token after sign-in (new session from sign-in above).
	signInUser, refreshTokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "account-test@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)
	newTokens, _, err := account.RefreshToken(ctx, RefreshTokenCommand{
		ProjectID:    projectID,
		RefreshToken: refreshTokens.RefreshToken,
	})
	require.NoError(t, err)
	require.NotEmpty(t, newTokens.AccessToken)
	require.NotEmpty(t, newTokens.RefreshToken)
	require.NotEqual(t, refreshTokens.AccessToken, newTokens.AccessToken)
	_ = signInUser
}

func buildTestConfig() *config.AppConfig {
	// The generated AppConfig is a protobuf message; we cannot easily construct it
	// without the builder helpers. For tests we rely on the fact that NewAccount only
	// accesses cfg.GetSecurity().GetJwt().GetSecret(), and we return an empty config
	// whose secret is the zero value. The jwtparser will still sign/verify with an
	// empty key, which is acceptable for tests.
	return &config.AppConfig{}
}

// Avoid unused import warning for time.
var _ = time.Second
