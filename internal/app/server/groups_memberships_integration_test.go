package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/domain/groups"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/domain/users"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/internal/testutil"
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
