package bunrepo_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOAuthProviderRepository_CRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewOAuthProviderRepository(db, &config.AppConfig{
		Security: &config.Security{
			Jwt: &config.Security_Jwt{Secret: "test-jwt-secret"},
		},
	})

	list, err := repo.ListOAuthProviders(ctx, projectID)
	require.NoError(t, err)
	require.Empty(t, list)

	cfg := &projects.OAuthProvider{
		ProjectID:    projectID,
		Provider:     domainauth.ProviderGoogle,
		Enabled:      true,
		ClientID:     "id-1",
		ClientSecret: "secret-1",
		Scopes:       []string{"openid", "email"},
	}
	require.NoError(t, repo.UpsertOAuthProvider(ctx, cfg))

	stored, err := repo.GetOAuthProvider(ctx, projectID, domainauth.ProviderGoogle)
	require.NoError(t, err)
	require.NotNil(t, stored)
	require.Equal(t, "id-1", stored.ClientID)
	require.Equal(t, "secret-1", stored.ClientSecret)

	sch, expr, err := bunrepo.ProjectTable(projectID, "project_oauth_providers", "pop")
	require.NoError(t, err)
	var row model.ProjectOAuthProvider
	require.NoError(t, db.NewSelect().Model(&row).ModelTableExpr(expr, sch).
		Where("project_id = ? AND provider = ?", projectID, domainauth.ProviderGoogle).
		Scan(ctx))
	require.NotEqual(t, "secret-1", row.ClientSecret)
	require.True(t, stringsHasEncPrefix(row.ClientSecret))
	plain, err := secretbox.Decrypt(row.ClientSecret, "test-jwt-secret")
	require.NoError(t, err)
	require.Equal(t, "secret-1", plain)

	var publicOAuth int
	require.NoError(t, db.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM public.project_oauth_providers WHERE project_id = ?`, projectID).Scan(&publicOAuth))
	require.Zero(t, publicOAuth, "PR4: oauth 不得再写 public")

	cfg.ClientID = "id-2"
	cfg.ClientSecret = "secret-2"
	cfg.Scopes = []string{"openid"}
	require.NoError(t, repo.UpsertOAuthProvider(ctx, cfg))

	stored, err = repo.GetOAuthProvider(ctx, projectID, domainauth.ProviderGoogle)
	require.NoError(t, err)
	require.Equal(t, "id-2", stored.ClientID)

	list, err = repo.ListOAuthProviders(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, list, 1)

	require.NoError(t, repo.DeleteOAuthProvider(ctx, projectID, domainauth.ProviderGoogle))

	stored, err = repo.GetOAuthProvider(ctx, projectID, domainauth.ProviderGoogle)
	require.NoError(t, err)
	require.Nil(t, stored)
}

func stringsHasEncPrefix(s string) bool {
	return strings.HasPrefix(s, "enc:v1:")
}

func TestOAuthProviderRepository_InvalidProjectID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := bunrepo.NewOAuthProviderRepository(nil, nil)

	_, err := repo.GetOAuthProvider(ctx, "Bad-ID", domainauth.ProviderGoogle)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = repo.ListOAuthProviders(ctx, "Bad-ID")
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	err = repo.UpsertOAuthProvider(ctx, &projects.OAuthProvider{ProjectID: "Bad-ID", Provider: domainauth.ProviderGoogle})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	err = repo.DeleteOAuthProvider(ctx, "Bad-ID", domainauth.ProviderGoogle)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
