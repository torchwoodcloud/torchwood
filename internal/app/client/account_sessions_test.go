package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAccount_SessionsUpdatePrefs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	cfg := buildTestConfig()
	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccount(cfg, projectRepo, db)
	user, tokens, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "sessions@torchwood.local",
		Password:  "User@123456",
		Name:      "Sessions User",
	})
	require.NoError(t, err)

	_, tokens2, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "sessions@torchwood.local",
		Password:  "User@123456",
	})
	require.NoError(t, err)
	_ = tokens2

	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	sessions, err := account.ListSessions(authCtx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(sessions), 2)

	updated, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Name: strPtr("Updated Name"),
	})
	require.NoError(t, err)
	require.Equal(t, "Updated Name", updated.Name)

	// D-1 presence 语义：未设置（nil）=不修改；设置空串=清空。
	unchanged, err := account.UpdateAccount(authCtx, UpdateAccountCommand{})
	require.NoError(t, err)
	require.Equal(t, "Updated Name", unchanged.Name)

	cleared, err := account.UpdateAccount(authCtx, UpdateAccountCommand{
		Name: strPtr(""),
	})
	require.NoError(t, err)
	require.Empty(t, cleared.Name)

	prefs, err := account.UpdatePrefs(authCtx, map[string]any{"theme": "dark"})
	require.NoError(t, err)
	require.Equal(t, "dark", prefs["theme"])

	gotPrefs, err := account.GetPrefs(authCtx)
	require.NoError(t, err)
	require.Equal(t, "dark", gotPrefs["theme"])

	otherSessionID := ""
	for _, s := range sessions {
		if s.ID != "" {
			otherSessionID = s.ID
			break
		}
	}
	require.NotEmpty(t, otherSessionID)

	deleteCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		SessionID: otherSessionID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})
	require.NoError(t, account.DeleteSession(deleteCtx, otherSessionID))

	require.NoError(t, account.DeleteSessions(deleteCtx, true))

	_, err = account.UpdateAccount(authCtx, UpdateAccountCommand{
		Password:    "NewPass@123",
		OldPassword: "wrong",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())

	_, err = account.UpdateAccount(authCtx, UpdateAccountCommand{
		Password:    "NewPass@123",
		OldPassword: "User@123456",
	})
	require.NoError(t, err)

	_, newTokens, _, _, err := account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     "sessions@torchwood.local",
		Password:  "NewPass@123",
	})
	require.NoError(t, err)
	require.NotEmpty(t, newTokens.AccessToken)
	_ = tokens
}
