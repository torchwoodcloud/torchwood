package server

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGroups_Memberships(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectRepo := bunrepo.NewProjectRepository(db)
	uc := NewGroups(projectRepo, bunrepo.NewUserRepository(db), bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db))
	ownerID := "owner-user-id"
	ownerEmail := "owner@torchwood.local"
	require.NoError(t, bunrepo.NewUserRepository(db).Insert(ctx, projectID, &users.User{
		ID:     ownerID,
		Email:  ownerEmail,
		Name:   "Owner",
		Status: users.StatusActive,
	}))
	principal := databases.Principal{Roles: []string{"users", "user:" + ownerID}}
	// 写方法入口双面守卫：server 面（admin 会话）主体注入。
	serverCtx := contexts.WithPrincipal(ctx, &shared.Principal{ActorKind: shared.ActorKindAdmin, ProjectID: projectID})
	group, ownerMembership, err := uc.CreateGroupWithOwner(serverCtx, projectID, "Engineering", ownerID, ownerEmail, principal)
	require.NoError(t, err)
	require.NotEmpty(t, group.ID)
	require.Equal(t, groups.StatusAccepted, ownerMembership.Data["status"])
	require.Equal(t, int64(1), groupTotal(t, group))

	ownerRoles := databases.Principal{Roles: []string{"users", "user:" + ownerID, "group:" + group.ID}}

	memberUserID := "member-user-id"
	require.NoError(t, bunrepo.NewUserRepository(db).Insert(ctx, projectID, &users.User{
		ID:           memberUserID,
		Email:        "member@torchwood.local",
		PasswordHash: "hash",
		Name:         "Member User",
		Status:       users.StatusActive,
	}))

	invite, err := uc.CreateMembership(serverCtx, projectID, CreateMembershipCommand{
		GroupID: group.ID,
		Email:   "member@torchwood.local",
		Name:    "Member User",
		Roles:   []string{groups.RoleMember},
	}, ownerRoles)
	require.NoError(t, err)
	require.Equal(t, groups.StatusPending, invite.Data["status"])
	require.Equal(t, memberUserID, invite.Data["user_id"])

	memberRoles := databases.Principal{Roles: []string{"users", "user:" + memberUserID}}
	authCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    memberUserID,
		Email:     "member@torchwood.local",
		Roles:     memberRoles.Roles,
	})

	accepted, err := uc.UpdateMembershipStatus(authCtx, projectID, group.ID, invite.ID, groups.StatusAccepted, memberRoles)
	require.NoError(t, err)
	require.Equal(t, groups.StatusAccepted, accepted.Data["status"])
	require.Equal(t, memberUserID, accepted.Data["user_id"])

	groupAfter, err := uc.GetGroup(ctx, projectID, group.ID, ownerRoles)
	require.NoError(t, err)
	require.Equal(t, int64(2), groupTotal(t, groupAfter))

	list, _, _, err := uc.ListMemberships(ctx, projectID, group.ID, databases.Query{}, ownerRoles)
	require.NoError(t, err)
	require.Len(t, list, 2)

	updated, err := uc.UpdateMembership(serverCtx, projectID, group.ID, accepted.ID, UpdateMembershipCommand{
		Roles: []string{groups.RoleAdmin},
	}, ownerRoles)
	require.NoError(t, err)
	require.Equal(t, []string{groups.RoleAdmin}, stringSliceField(updated.Data["roles"]))

	groupRoles, err := uc.ListAcceptedGroupRoles(ctx, projectID, memberUserID)
	require.NoError(t, err)
	require.Contains(t, groupRoles, "group:"+group.ID)
	require.Contains(t, groupRoles, "member:"+accepted.ID)

	require.NoError(t, uc.DeleteMembership(authCtx, projectID, group.ID, accepted.ID, memberRoles))

	groupAfterLeave, err := uc.GetGroup(ctx, projectID, group.ID, ownerRoles)
	require.NoError(t, err)
	require.Equal(t, int64(1), groupTotal(t, groupAfterLeave))

	require.NoError(t, uc.DeleteGroup(serverCtx, projectID, group.ID, ownerRoles))
	_, err = uc.GetGroup(ctx, projectID, group.ID, ownerRoles)
	require.Equal(t, codes.NotFound, status.Code(err))
}

// TestGroups_LastOwnerGuard_ConcurrentOwnerChanges（缺陷修复验收）：剩 2 个
// owner 时并发降级/删除，守卫判定与变更收进同一事务并在组行 FOR UPDATE 锁
// 下执行——修复前两个事务都能通过无锁 count 检查并双双提交，组永久失去
// owner；修复后组行锁串行化，后到者守卫拒绝（FailedPrecondition），组内
// 始终保留 ≥1 个 owner。
func TestGroups_LastOwnerGuard_ConcurrentOwnerChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectRepo := bunrepo.NewProjectRepository(db)
	usersRepo := bunrepo.NewUserRepository(db)
	uc := NewGroups(projectRepo, usersRepo, bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db))

	owner1ID := "owner-a-user-id"
	require.NoError(t, usersRepo.Insert(ctx, projectID, &users.User{
		ID:     owner1ID,
		Email:  "owner-a@torchwood.local",
		Name:   "Owner A",
		Status: users.StatusActive,
	}))
	owner2ID := "owner-b-user-id"
	require.NoError(t, usersRepo.Insert(ctx, projectID, &users.User{
		ID:     owner2ID,
		Email:  "owner-b@torchwood.local",
		Name:   "Owner B",
		Status: users.StatusActive,
	}))

	// 写方法入口双面守卫：server 面（admin 会话）主体注入。
	serverCtx := contexts.WithPrincipal(ctx, &shared.Principal{ActorKind: shared.ActorKindAdmin, ProjectID: projectID})
	group, owner1Membership, err := uc.CreateGroupWithOwner(serverCtx, projectID, "Concurrency", owner1ID, "owner-a@torchwood.local",
		databases.Principal{Roles: []string{"users", "user:" + owner1ID}})
	require.NoError(t, err)
	owner2Membership, err := uc.CreateMembership(serverCtx, projectID, CreateMembershipCommand{
		GroupID: group.ID,
		UserID:  owner2ID,
		Roles:   []string{groups.RoleOwner},
		Status:  groups.StatusAccepted,
	}, databases.Principal{Roles: []string{"users", "user:" + owner1ID, "group:" + group.ID}})
	require.NoError(t, err)

	// 并发对冲：A 降级 owner1 → member；B 删除 owner2。
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = uc.UpdateMembership(serverCtx, projectID, group.ID, owner1Membership.ID,
			UpdateMembershipCommand{Roles: []string{groups.RoleMember}}, databases.Principal{Roles: []string{"admin"}})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = uc.DeleteMembership(serverCtx, projectID, group.ID, owner2Membership.ID, databases.Principal{Roles: []string{"admin"}})
	}()
	close(start)
	wg.Wait()

	var successes int
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	require.LessOrEqual(t, successes, 1, "并发 owner 变更至多一个能提交（组行锁串行化）：%v / %v", errs[0], errs[1])

	// 组内必须保留至少一个 accepted owner。
	list, _, _, err := uc.ListMemberships(ctx, projectID, group.ID, databases.Query{}, databases.Principal{Roles: []string{"admin"}})
	require.NoError(t, err)
	owners := 0
	for _, doc := range list {
		if doc.Data["status"] != groups.StatusAccepted {
			continue
		}
		for _, r := range stringSliceField(doc.Data["roles"]) {
			if r == groups.RoleOwner {
				owners++
				break
			}
		}
	}
	require.GreaterOrEqual(t, owners, 1, "组必须保留至少一个 owner")
}

func groupTotal(t *testing.T, doc *databases.Document) int64 {
	t.Helper()
	switch v := doc.Data["total"].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		t.Fatalf("unexpected total type %T", v)
		return 0
	}
}

func stringSliceField(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		if s, ok := v.([]string); ok {
			return s
		}
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
