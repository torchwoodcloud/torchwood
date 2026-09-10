package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	infraauth "github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSessionRepository_CRUDAndEvict(t *testing.T) {
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
		ID:           "u-sess",
		Email:        "sess@torchwood.local",
		PasswordHash: "h",
		Name:         "Sess",
		Status:       domainusers.StatusActive,
	})
	repo := bunrepo.NewSessionRepository(db)

	now := time.Now().UTC().Truncate(time.Millisecond)
	hashed := infraauth.HashOTP("already-hashed-secret")
	s1 := &domainauth.Session{
		ID:         "s1",
		UserID:     user.ID,
		SecretHash: hashed,
		Provider:   domainauth.ProviderEmail,
		UserAgent:  "ua-1",
		IP:         "127.0.0.1",
		Country:    "US",
		ExpireAt:   now.Add(4 * time.Hour),
	}
	require.NoError(t, repo.Insert(ctx, projectID, s1))

	got, err := repo.GetByID(ctx, projectID, s1.ID)
	require.NoError(t, err)
	require.Equal(t, hashed, got.SecretHash)
	require.Equal(t, user.ID, got.UserID)
	require.Equal(t, "ua-1", got.UserAgent)
	require.Equal(t, "127.0.0.1", got.IP)
	require.Equal(t, "US", got.Country)

	plain := "550e8400-e29b-41d4-a716-446655440000"
	require.Len(t, plain, 36)
	sPlain := &domainauth.Session{
		ID:         "s-plain",
		UserID:     user.ID,
		SecretHash: plain,
		Provider:   domainauth.ProviderEmail,
		// 2.5h：不得与 s4 的 3h 相同——DeleteOldestByUser 按 expire_at 排序，
		// expire 相同的 tie 无第二排序键、删除选择不确定（-p 4 下实证 flake：
		// s4 被误删导致断言 {s1, s4} 不含 s4）。
		ExpireAt: now.Add(150 * time.Minute),
	}
	require.NoError(t, repo.Insert(ctx, projectID, sPlain))
	gotPlain, err := repo.GetByID(ctx, projectID, sPlain.ID)
	require.NoError(t, err)
	require.Equal(t, infraauth.HashOTP(plain), gotPlain.SecretHash, "GetByID 必须双读明文 UUID")

	for i, id := range []string{"s2", "s3", "s4"} {
		require.NoError(t, repo.Insert(ctx, projectID, &domainauth.Session{
			ID:         id,
			UserID:     user.ID,
			SecretHash: hashed,
			ExpireAt:   now.Add(time.Duration(i+1) * time.Hour),
		}))
	}

	listed, err := repo.ListByUser(ctx, projectID, user.ID)
	require.NoError(t, err)
	require.Len(t, listed, 5)

	require.NoError(t, repo.Delete(ctx, projectID, "s2"))
	got, err = repo.GetByID(ctx, projectID, "s2")
	require.NoError(t, err)
	require.Nil(t, got)

	require.NoError(t, repo.DeleteOldestByUser(ctx, projectID, user.ID, 2))
	listed, err = repo.ListByUser(ctx, projectID, user.ID)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	ids := map[string]struct{}{}
	for _, s := range listed {
		ids[s.ID] = struct{}{}
	}
	require.Contains(t, ids, "s1", "expire_at 最晚的应保留")
	require.Contains(t, ids, "s4")

	require.NoError(t, repo.DeleteByUser(ctx, projectID, user.ID))
	listed, err = repo.ListByUser(ctx, projectID, user.ID)
	require.NoError(t, err)
	require.Empty(t, listed)

	err = repo.Insert(ctx, "", s1)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
