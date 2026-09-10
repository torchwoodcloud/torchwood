package bunrepo_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestUserRepository_CRUDAndListWhitelist(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewUserRepository(db)

	alice := seedSysUser(t, ctx, repo, projectID, &domainusers.User{
		ID:           "u-alice",
		Email:        "Alice@Torchwood.local",
		PasswordHash: "hash-alice",
		Name:         "Alice",
		Status:       domainusers.StatusActive,
		Phone:        "+15550001",
		Labels:       []string{"vip"},
		Prefs:        map[string]any{"theme": "light"},
	})
	require.Equal(t, "alice@torchwood.local", alice.Email)

	got, err := repo.GetByID(ctx, projectID, alice.ID)
	require.NoError(t, err)
	require.Equal(t, "Alice", got.Name)
	require.Equal(t, "hash-alice", got.PasswordHash)
	require.Equal(t, []string{"vip"}, got.Labels)
	require.Equal(t, "light", got.Prefs["theme"])

	byEmail, err := repo.GetByEmail(ctx, projectID, "ALICE@torchwood.local")
	require.NoError(t, err)
	require.Equal(t, alice.ID, byEmail.ID)

	byPhone, err := repo.GetByPhone(ctx, projectID, "+15550001")
	require.NoError(t, err)
	require.Equal(t, alice.ID, byPhone.ID)

	dup := &domainusers.User{
		ID:           "u-dup",
		Email:        "alice@torchwood.local",
		PasswordHash: "x",
		Name:         "Dup",
		Status:       domainusers.StatusActive,
	}
	err = repo.Insert(ctx, projectID, dup)
	require.ErrorIs(t, err, domainusers.ErrEmailAlreadyRegistered)

	require.NoError(t, repo.Update(ctx, projectID, alice.ID, map[string]any{
		"password_hash": "hash-new",
	}))
	got, err = repo.GetByID(ctx, projectID, alice.ID)
	require.NoError(t, err)
	require.Equal(t, "hash-new", got.PasswordHash)
	require.Equal(t, "Alice", got.Name, "分列 UPDATE 不得覆盖未点名列")
	require.Equal(t, "light", got.Prefs["theme"])

	_ = seedSysUser(t, ctx, repo, projectID, &domainusers.User{
		ID:           "u-bob",
		Email:        "bob@torchwood.local",
		PasswordHash: "hash-bob",
		Name:         "Bob",
		Status:       domainusers.StatusActive,
	})
	_ = seedSysUser(t, ctx, repo, projectID, &domainusers.User{
		ID:           "u-carol",
		Email:        "carol@torchwood.local",
		PasswordHash: "hash-carol",
		Name:         "Alice",
		Status:       domainusers.StatusInactive,
	})

	listed, err := repo.List(ctx, projectID, domainusers.ListFilter{
		Queries: []string{query.BuildEqual("name", "Alice")},
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), listed.TotalCount)
	require.Len(t, listed.Users, 2)

	_, err = repo.List(ctx, projectID, domainusers.ListFilter{
		Queries: []string{query.BuildEqual("password_hash", "x")},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "未知属性必须 InvalidArgument")

	_, err = repo.List(ctx, projectID, domainusers.ListFilter{
		Queries: []string{query.BuildFilter("contains", "name", "A")},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	otherID, _, otherCleanup := testutil.CreateTestProject(ctx, db)
	defer otherCleanup()
	missing, err := repo.GetByID(ctx, otherID, alice.ID)
	require.NoError(t, err)
	require.Nil(t, missing)

	err = repo.Insert(ctx, "", alice)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	require.NoError(t, repo.Delete(ctx, projectID, alice.ID))
	got, err = repo.GetByID(ctx, projectID, alice.ID)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestUserRepository_ColumnUpdateDoesNotClobber(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewUserRepository(db)

	u := seedSysUser(t, ctx, repo, projectID, &domainusers.User{
		ID:           "u-s15",
		Email:        "s15@torchwood.local",
		PasswordHash: "old-hash",
		Name:         "KeepMe",
		Status:       domainusers.StatusActive,
		Prefs:        map[string]any{"k": "v0"},
	})

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errCh <- repo.Update(ctx, projectID, u.ID, map[string]any{"password_hash": "new-hash"})
	}()
	go func() {
		defer wg.Done()
		<-start
		errCh <- repo.Update(ctx, projectID, u.ID, map[string]any{"prefs": map[string]any{"k": "v1"}})
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	got, err := repo.GetByID(ctx, projectID, u.ID)
	require.NoError(t, err)
	require.Equal(t, "KeepMe", got.Name)
	require.Equal(t, "new-hash", got.PasswordHash)
	require.Equal(t, "v1", got.Prefs["k"])
}

func TestUserRepository_UpdateFactorsSerializes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewUserRepository(db)

	u := seedSysUser(t, ctx, repo, projectID, &domainusers.User{
		ID:           "u-mfa",
		Email:        "mfa@torchwood.local",
		PasswordHash: "h",
		Name:         "MFA",
		Status:       domainusers.StatusActive,
		Factors:      json.RawMessage(`{}`),
	})

	mutate := func(key string) func(json.RawMessage) (json.RawMessage, error) {
		return func(current json.RawMessage) (json.RawMessage, error) {
			var m map[string]any
			if len(current) > 0 {
				if err := json.Unmarshal(current, &m); err != nil {
					return nil, err
				}
			}
			if m == nil {
				m = map[string]any{}
			}
			time.Sleep(80 * time.Millisecond)
			m[key] = true
			return json.Marshal(m)
		}
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errCh <- repo.UpdateFactors(ctx, projectID, u.ID, mutate("a"))
	}()
	go func() {
		defer wg.Done()
		<-start
		errCh <- repo.UpdateFactors(ctx, projectID, u.ID, mutate("b"))
	}()
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}

	got, err := repo.GetByID(ctx, projectID, u.ID)
	require.NoError(t, err)
	var factors map[string]any
	require.NoError(t, json.Unmarshal(got.Factors, &factors))
	require.Equal(t, true, factors["a"])
	require.Equal(t, true, factors["b"], "FOR UPDATE 后第二次必须看到第一次的 JSON")

	err = repo.UpdateFactors(ctx, projectID, u.ID, func(json.RawMessage) (json.RawMessage, error) {
		return json.RawMessage(`{`), nil
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func seedSysUser(t *testing.T, ctx context.Context, repo *bunrepo.UserRepository, projectID string, u *domainusers.User) *domainusers.User {
	t.Helper()
	require.NoError(t, repo.Insert(ctx, projectID, u))
	got, err := repo.GetByID(ctx, projectID, u.ID)
	require.NoError(t, err)
	require.NotNil(t, got)
	return got
}
