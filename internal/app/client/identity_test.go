package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestResolveOAuthUser_RejectsExistingEmailWithoutIdentity(t *testing.T) {
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

	_, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "existing@torchwood.local",
		Password:  "User@123",
		Name:      "Existing",
	})
	require.NoError(t, err)

	_, err = account.resolveOAuthUser(ctx, projectID, domainauth.ProviderGoogle, &domainauth.OAuthUserInfo{
		ProviderUID: "google-123",
		Email:       "existing@torchwood.local",
		Name:        "OAuth User",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())
}

func TestResolveOAuthUser_RejectsUnverifiedEmail(t *testing.T) {
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

	// email_verified=false 一律拒绝（安全评审 M8），且不占号。
	_, err := account.resolveOAuthUser(ctx, projectID, domainauth.ProviderGoogle, &domainauth.OAuthUserInfo{
		ProviderUID:   "google-unverified",
		Email:         "unverified@torchwood.local",
		EmailVerified: false,
		Name:          "Unverified",
	})
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.FailedPrecondition, st.Code())

	// 已验证明路径正常走通。
	_, err = account.resolveOAuthUser(ctx, projectID, domainauth.ProviderGoogle, &domainauth.OAuthUserInfo{
		ProviderUID:   "google-verified",
		Email:         "verified@torchwood.local",
		EmailVerified: true,
		Name:          "Verified",
	})
	require.NoError(t, err)
}
