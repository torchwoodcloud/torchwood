package client

import (
	"context"
	"regexp"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
)

func TestAccount_VerificationFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	cfg := &config.AppConfig{
		Messaging: &config.Messaging{DevLogOtp: true},
		Server: &config.Server{
			Http: &config.Http{PublicUrl: "http://localhost:9099"},
		},
	}
	projectRepo := bunrepo.NewProjectRepository(db)

	mailer := &CaptureMailer{}
	account := NewTestAccountWithMailer(cfg, projectRepo, db, rdb, mailer)

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "verify-me@example.com",
		Password:  "User@123",
		Name:      "Verify Me",
	})
	require.NoError(t, err)
	require.False(t, user.EmailVerified)

	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})
	challenge, err := account.CreateVerification(authCtx, CreateVerificationCommand{
		ProjectID: projectID,
		URL:       "http://localhost/verify",
	})
	require.NoError(t, err)
	require.Equal(t, user.ID, challenge.UserID)
	require.Len(t, mailer.Bodies, 1)

	re := regexp.MustCompile(`secret=([a-f0-9]+)`)
	matches := re.FindStringSubmatch(mailer.Bodies[0])
	require.Len(t, matches, 2)

	verified, err := account.UpdateVerification(ctx, UpdateVerificationCommand{
		ProjectID: projectID,
		UserID:    user.ID,
		Secret:    matches[1],
	})
	require.NoError(t, err)
	require.True(t, verified.EmailVerified)
}

func TestAccount_RecoveryFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	cfg := &config.AppConfig{
		Messaging: &config.Messaging{DevLogOtp: true},
		Server: &config.Server{
			Http: &config.Http{PublicUrl: "http://localhost:9099"},
		},
	}
	projectRepo := bunrepo.NewProjectRepository(db)

	mailer := &CaptureMailer{}
	account := NewTestAccountWithMailer(cfg, projectRepo, db, rdb, mailer)

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "recover-me@torchwood.local",
		Password:  "User@123",
		Name:      "Recover Me",
	})
	require.NoError(t, err)

	require.NoError(t, account.CreateRecovery(ctx, CreateRecoveryCommand{
		ProjectID: projectID,
		Email:     user.Email,
		URL:       "http://localhost/recovery",
	}))
	require.Len(t, mailer.Bodies, 1)

	re := regexp.MustCompile(`secret=([a-f0-9]+)`)
	matches := re.FindStringSubmatch(mailer.Bodies[0])
	require.Len(t, matches, 2)

	require.NoError(t, account.UpdateRecovery(ctx, UpdateRecoveryCommand{
		ProjectID: projectID,
		UserID:    user.ID,
		Secret:    matches[1],
		Password:  "NewPass@456",
	}))

	_, _, _, _, err = account.SignIn(ctx, SignInCommand{
		ProjectID: projectID,
		Email:     user.Email,
		Password:  "NewPass@456",
	})
	require.NoError(t, err)
}
