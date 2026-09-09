package auth_test

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRedisOAuthStateStore(t *testing.T) {
	t.Parallel()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	defer mr.Close()

	store := auth.NewRedisOAuthStateStore(redis.NewClient(&redis.Options{Addr: mr.Addr()}))
	ctx := context.Background()

	state := domainauth.OAuthState{
		StateID:      "state-1",
		ProjectID:    "proj",
		Provider:     domainauth.ProviderGoogle,
		SuccessURL:   "https://app.example/success",
		FailureURL:   "https://app.example/failure",
		PKCEVerifier: "verifier",
	}
	require.NoError(t, store.Save(ctx, state, time.Minute))

	got, err := store.Consume(ctx, "state-1")
	require.NoError(t, err)
	require.Equal(t, "proj", got.ProjectID)
	require.Equal(t, domainauth.ProviderGoogle, got.Provider)

	// Consume is one-time: a replay (concurrent callback) must fail.
	_, err = store.Consume(ctx, "state-1")
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unauthenticated, st.Code())
}

func TestNewOAuthAuthenticator_Unsupported(t *testing.T) {
	t.Parallel()
	_, err := auth.NewOAuthAuthenticator("unknown", "id", "secret", "http://localhost/cb", nil)
	require.Error(t, err)
}
