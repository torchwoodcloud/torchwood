package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIdentityRepository_CRUDAndUniqueProviderUID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	usersRepo := bunrepo.NewUserRepository(db)
	user := seedSysUser(t, ctx, usersRepo, projectID, &domainusers.User{
		ID:           "u-idty",
		Email:        "idty@torchwood.local",
		PasswordHash: "h",
		Name:         "Idty",
		Status:       domainusers.StatusActive,
	})
	other := seedSysUser(t, ctx, usersRepo, projectID, &domainusers.User{
		ID:           "u-idty-2",
		Email:        "idty2@torchwood.local",
		PasswordHash: "h",
		Name:         "Idty2",
		Status:       domainusers.StatusActive,
	})
	repo := bunrepo.NewIdentityRepository(db)

	exp := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Millisecond)
	in := &domainauth.Identity{
		ID:            "i1",
		UserID:        user.ID,
		Provider:      domainauth.ProviderGoogle,
		ProviderUID:   "google-uid-1",
		ProviderEmail: "idty@gmail.com",
		ProviderData:  map[string]any{"name": "Idty"},
		ExpireAt:      &exp,
	}
	require.NoError(t, repo.Insert(ctx, projectID, in))

	got, err := repo.GetByID(ctx, projectID, in.ID)
	require.NoError(t, err)
	require.Equal(t, user.ID, got.UserID)
	require.Equal(t, domainauth.ProviderGoogle, got.Provider)
	require.Equal(t, "google-uid-1", got.ProviderUID)
	require.Equal(t, "idty@gmail.com", got.ProviderEmail)
	require.Equal(t, "Idty", got.ProviderData["name"])
	require.NotNil(t, got.ExpireAt)
	require.WithinDuration(t, exp, *got.ExpireAt, time.Second)

	byUID, err := repo.GetByProviderUID(ctx, projectID, domainauth.ProviderGoogle, "google-uid-1")
	require.NoError(t, err)
	require.Equal(t, in.ID, byUID.ID)

	dup := &domainauth.Identity{
		ID:          "i-dup",
		UserID:      other.ID,
		Provider:    domainauth.ProviderGoogle,
		ProviderUID: "google-uid-1",
	}
	err = repo.Insert(ctx, projectID, dup)
	require.ErrorIs(t, err, domainauth.ErrIdentityAlreadyLinked)

	require.NoError(t, repo.Insert(ctx, projectID, &domainauth.Identity{
		ID:          "i2",
		UserID:      user.ID,
		Provider:    domainauth.ProviderGitHub,
		ProviderUID: "gh-1",
	}))
	listed, err := repo.ListByUser(ctx, projectID, user.ID)
	require.NoError(t, err)
	require.Len(t, listed, 2)

	require.NoError(t, repo.Delete(ctx, projectID, "i2"))
	got, err = repo.GetByID(ctx, projectID, "i2")
	require.NoError(t, err)
	require.Nil(t, got)

	err = repo.Insert(ctx, "", in)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
