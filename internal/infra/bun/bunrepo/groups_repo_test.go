package bunrepo_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domaingroups "github.com/torchwoodcloud/torchwood/internal/domain/groups"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGroupRepository_AddTotalGREATESTAndConcurrent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()
	repo := bunrepo.NewGroupRepository(db)

	require.NoError(t, repo.Insert(ctx, projectID, &domaingroups.Group{
		ID:   "g-total",
		Name: "Totals",
	}))

	require.NoError(t, repo.AddTotal(ctx, projectID, "g-total", 2))
	got, err := repo.GetByID(ctx, projectID, "g-total")
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Total)

	require.NoError(t, repo.AddTotal(ctx, projectID, "g-total", -1))
	got, err = repo.GetByID(ctx, projectID, "g-total")
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Total)

	require.NoError(t, repo.AddTotal(ctx, projectID, "g-total", -10))
	got, err = repo.GetByID(ctx, projectID, "g-total")
	require.NoError(t, err)
	require.Equal(t, int64(0), got.Total, "GREATEST 下限 0")

	require.NoError(t, repo.Update(ctx, projectID, "g-total", map[string]any{
		"name":        "Renamed",
		"permissions": []string{"read:any"},
		"prefs":       map[string]any{"k": "v"},
	}))
	got, err = repo.GetByID(ctx, projectID, "g-total")
	require.NoError(t, err)
	require.Equal(t, "Renamed", got.Name)
	require.Equal(t, []string{"read:any"}, got.Permissions)
	require.Equal(t, "v", got.Prefs["k"])
	require.Equal(t, int64(0), got.Total, "Update 不得写 total")

	err = repo.Update(ctx, projectID, "g-total", map[string]any{"total": int64(9)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	require.NoError(t, repo.Insert(ctx, projectID, &domaingroups.Group{ID: "g-conc", Name: "Conc"}))
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			errCh <- repo.AddTotal(ctx, projectID, "g-conc", 1)
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for e := range errCh {
		require.NoError(t, e)
	}
	got, err = repo.GetByID(ctx, projectID, "g-conc")
	require.NoError(t, err)
	require.Equal(t, int64(2), got.Total)

	err = repo.Insert(ctx, "", &domaingroups.Group{ID: "g-x", Name: "x"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestGroupRepository_RecountAcceptedAfterUserCascade(t *testing.T) {
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
		ID:           "u-mem",
		Email:        "mem@torchwood.local",
		PasswordHash: "h",
		Name:         "Mem",
		Status:       domainusers.StatusActive,
	})
	groupsRepo := bunrepo.NewGroupRepository(db)
	memRepo := bunrepo.NewMembershipRepository(db)
	require.NoError(t, groupsRepo.Insert(ctx, projectID, &domaingroups.Group{ID: "g-rc", Name: "Recount"}))
	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-rc",
		GroupID: "g-rc",
		UserID:  user.ID,
		Email:   user.Email,
		Roles:   []string{domaingroups.RoleMember},
		Status:  domaingroups.StatusAccepted,
	}))
	require.NoError(t, groupsRepo.RecountAccepted(ctx, projectID, "g-rc"))
	got, err := groupsRepo.GetByID(ctx, projectID, "g-rc")
	require.NoError(t, err)
	require.Equal(t, int64(1), got.Total)

	require.NoError(t, usersRepo.Delete(ctx, projectID, user.ID))
	listed, err := memRepo.ListByGroup(ctx, projectID, "g-rc")
	require.NoError(t, err)
	require.Empty(t, listed, "users FK CASCADE 应删 memberships")

	require.NoError(t, groupsRepo.RecountAccepted(ctx, projectID, "g-rc"))
	got, err = groupsRepo.GetByID(ctx, projectID, "g-rc")
	require.NoError(t, err)
	require.Equal(t, int64(0), got.Total)
}

func TestMembershipRepository_AcceptCASAndDuplicates(t *testing.T) {
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
		ID:           "u-acc",
		Email:        "acc@torchwood.local",
		PasswordHash: "h",
		Name:         "Acc",
		Status:       domainusers.StatusActive,
	})
	other := seedSysUser(t, ctx, usersRepo, projectID, &domainusers.User{
		ID:           "u-acc-2",
		Email:        "acc2@torchwood.local",
		PasswordHash: "h",
		Name:         "Acc2",
		Status:       domainusers.StatusActive,
	})
	groupsRepo := bunrepo.NewGroupRepository(db)
	memRepo := bunrepo.NewMembershipRepository(db)
	require.NoError(t, groupsRepo.Insert(ctx, projectID, &domaingroups.Group{ID: "g-acc", Name: "Accept"}))

	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-pend",
		GroupID: "g-acc",
		Email:   "invite@torchwood.local",
		Roles:   []string{domaingroups.RoleMember},
		Status:  domaingroups.StatusPending,
	}))
	got, err := memRepo.GetByID(ctx, projectID, "m-pend")
	require.NoError(t, err)
	require.Empty(t, got.UserID)

	joined := time.Now().UTC().Truncate(time.Millisecond)
	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	start := make(chan struct{})
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			errCh <- memRepo.Accept(ctx, projectID, "m-pend", user.ID, joined)
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	var okN, failN int
	for e := range errCh {
		if e == nil {
			okN++
			continue
		}
		require.ErrorIs(t, e, domaingroups.ErrMembershipNotPending)
		failN++
	}
	require.Equal(t, 1, okN, "并发 Accept 只能成功一次")
	require.Equal(t, 1, failN)

	got, err = memRepo.GetByID(ctx, projectID, "m-pend")
	require.NoError(t, err)
	require.Equal(t, domaingroups.StatusAccepted, got.Status)
	require.Equal(t, user.ID, got.UserID)
	g, err := groupsRepo.GetByID(ctx, projectID, "g-acc")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total)

	err = memRepo.Accept(ctx, projectID, "m-pend", user.ID, time.Now())
	require.ErrorIs(t, err, domaingroups.ErrMembershipNotPending)
	g, err = groupsRepo.GetByID(ctx, projectID, "g-acc")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total, "非 pending Accept 不得再 +1")

	err = memRepo.Accept(ctx, projectID, "m-missing", user.ID, time.Now())
	require.ErrorIs(t, err, domaingroups.ErrMembershipNotFound)

	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-rej",
		GroupID: "g-acc",
		Email:   "reject@torchwood.local",
		Status:  domaingroups.StatusPending,
	}))
	require.NoError(t, memRepo.Reject(ctx, projectID, "m-rej"))
	got, err = memRepo.GetByID(ctx, projectID, "m-rej")
	require.NoError(t, err)
	require.Equal(t, domaingroups.StatusRejected, got.Status)
	g, err = groupsRepo.GetByID(ctx, projectID, "g-acc")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total, "Reject 不得 AddTotal")
	err = memRepo.Reject(ctx, projectID, "m-rej")
	require.ErrorIs(t, err, domaingroups.ErrMembershipNotPending)
	err = memRepo.Accept(ctx, projectID, "m-rej", user.ID, time.Now())
	require.ErrorIs(t, err, domaingroups.ErrMembershipNotPending, "rejected 不得再 Accept")
	err = memRepo.Reject(ctx, projectID, "m-pend")
	require.ErrorIs(t, err, domaingroups.ErrMembershipNotPending, "accepted 不得 Reject")
	g, err = groupsRepo.GetByID(ctx, projectID, "g-acc")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total)
	err = memRepo.Reject(ctx, projectID, "m-missing")
	require.ErrorIs(t, err, domaingroups.ErrMembershipNotFound)

	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-other",
		GroupID: "g-acc",
		UserID:  other.ID,
		Email:   other.Email,
		Status:  domaingroups.StatusPending,
	}))
	err = memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-dup-user",
		GroupID: "g-acc",
		UserID:  other.ID,
		Status:  domaingroups.StatusPending,
	})
	require.ErrorIs(t, err, domaingroups.ErrMembershipAlreadyExists)

	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-email",
		GroupID: "g-acc",
		Email:   "  Dup-Email@Torchwood.local ",
		Status:  domaingroups.StatusPending,
	}))
	got, err = memRepo.GetByID(ctx, projectID, "m-email")
	require.NoError(t, err)
	require.Equal(t, "dup-email@torchwood.local", got.Email)
	err = memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-email-2",
		GroupID: "g-acc",
		Email:   "DUP-EMAIL@torchwood.local",
		Status:  domaingroups.StatusPending,
	})
	require.ErrorIs(t, err, domaingroups.ErrMembershipAlreadyExists)

	require.NoError(t, memRepo.UpdateRoles(ctx, projectID, "m-other", func(txCtx context.Context, current *domaingroups.Membership) ([]string, error) {
		listed, listErr := memRepo.ListByGroup(txCtx, projectID, current.GroupID)
		if listErr != nil {
			return nil, listErr
		}
		require.GreaterOrEqual(t, len(listed), 1)
		return []string{domaingroups.RoleAdmin}, nil
	}))
	got, err = memRepo.GetByID(ctx, projectID, "m-other")
	require.NoError(t, err)
	require.Equal(t, []string{domaingroups.RoleAdmin}, got.Roles)
	err = memRepo.UpdateRoles(ctx, projectID, "m-other", func(context.Context, *domaingroups.Membership) ([]string, error) {
		return nil, status.Error(codes.FailedPrecondition, "group must keep at least one owner")
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	got, err = memRepo.GetByID(ctx, projectID, "m-other")
	require.NoError(t, err)
	require.Equal(t, []string{domaingroups.RoleAdmin}, got.Roles, "回调失败不得改 roles")

	require.NoError(t, groupsRepo.Delete(ctx, projectID, "g-acc"))
	listed, err := memRepo.ListByGroup(ctx, projectID, "g-acc")
	require.NoError(t, err)
	require.Empty(t, listed, "groups FK CASCADE 应删 memberships")
}

func TestMembershipRepository_AcceptedInsertDeleteAdjustsTotal(t *testing.T) {
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
		ID:           "u-ins",
		Email:        "ins@torchwood.local",
		PasswordHash: "h",
		Name:         "Ins",
		Status:       domainusers.StatusActive,
	})
	groupsRepo := bunrepo.NewGroupRepository(db)
	memRepo := bunrepo.NewMembershipRepository(db)
	require.NoError(t, groupsRepo.Insert(ctx, projectID, &domaingroups.Group{ID: "g-ins", Name: "Insert"}))

	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-acc",
		GroupID: "g-ins",
		UserID:  user.ID,
		Email:   user.Email,
		Roles:   []string{domaingroups.RoleMember},
		Status:  domaingroups.StatusAccepted,
	}))
	g, err := groupsRepo.GetByID(ctx, projectID, "g-ins")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total, "accepted Insert 必须 AddTotal(+1)")

	require.NoError(t, memRepo.Insert(ctx, projectID, &domaingroups.Membership{
		ID:      "m-pend",
		GroupID: "g-ins",
		Email:   "pend@torchwood.local",
		Status:  domaingroups.StatusPending,
	}))
	g, err = groupsRepo.GetByID(ctx, projectID, "g-ins")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total, "pending Insert 不得 AddTotal")

	require.NoError(t, memRepo.Delete(ctx, projectID, "m-pend"))
	g, err = groupsRepo.GetByID(ctx, projectID, "g-ins")
	require.NoError(t, err)
	require.Equal(t, int64(1), g.Total, "pending Delete 不得 AddTotal")

	require.NoError(t, memRepo.Delete(ctx, projectID, "m-acc"))
	g, err = groupsRepo.GetByID(ctx, projectID, "g-ins")
	require.NoError(t, err)
	require.Equal(t, int64(0), g.Total, "accepted Delete 必须 AddTotal(-1)")
	got, err := memRepo.GetByID(ctx, projectID, "m-acc")
	require.NoError(t, err)
	require.Nil(t, got)
}
