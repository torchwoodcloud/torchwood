package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// serverActorCtx 为写方法入口双面守卫注入 server 面（admin 会话）主体。
func serverActorCtx(ctx context.Context, projectID string) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{ActorKind: shared.ActorKindAdmin, ProjectID: projectID})
}

// TestGroups_Prefs_CRUD：空 prefs → 写入 → 整体替换（旧键消失）。
func TestGroups_Prefs_CRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	uc := NewGroups(bunrepo.NewProjectRepository(db), bunrepo.NewUserRepository(db), bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db))
	serverCtx := serverActorCtx(ctx, projectID)
	group, err := uc.CreateGroup(serverCtx, projectID, "Design", nil)
	require.NoError(t, err)

	keys := databases.Principal{Roles: []string{"keys"}}

	prefs, err := uc.GetGroupPrefs(ctx, projectID, group.ID, keys)
	require.NoError(t, err)
	require.Empty(t, prefs, "从未设置时返回空对象")

	updated, err := uc.UpdateGroupPrefs(serverCtx, projectID, group.ID, map[string]any{"theme": "dark", "notifications": true}, keys)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"theme": "dark", "notifications": true}, updated)

	got, err := uc.GetGroupPrefs(ctx, projectID, group.ID, keys)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"theme": "dark", "notifications": true}, got)

	// 整体替换：旧键消失。
	updated, err = uc.UpdateGroupPrefs(serverCtx, projectID, group.ID, map[string]any{"locale": "zh-CN"}, keys)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"locale": "zh-CN"}, updated)

	got, err = uc.GetGroupPrefs(ctx, projectID, group.ID, keys)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"locale": "zh-CN"}, got)
}

// TestGroups_Prefs_Errors：NotFound / InvalidArgument。
func TestGroups_Prefs_Errors(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	uc := NewGroups(bunrepo.NewProjectRepository(db), bunrepo.NewUserRepository(db), bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db))
	serverCtx := serverActorCtx(ctx, projectID)
	keys := databases.Principal{Roles: []string{"keys"}}

	_, err := uc.GetGroupPrefs(ctx, projectID, "no-such-group", keys)
	require.Equal(t, codes.NotFound, status.Code(err))

	_, err = uc.UpdateGroupPrefs(serverCtx, projectID, "no-such-group", map[string]any{"a": 1}, keys)
	require.Equal(t, codes.NotFound, status.Code(err))

	group, err := uc.CreateGroup(serverCtx, projectID, "Errors Group", nil)
	require.NoError(t, err)

	_, err = uc.UpdateGroupPrefs(serverCtx, projectID, group.ID, nil, keys)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.ErrorContains(t, err, "prefs is required")
}

// TestGroups_Prefs_PermissionMatrix：keys / admin / 已接受成员（group:{id}）可写；
// 无用户组角色的 users 主体 → PermissionDenied（HTTP 403 语义）。
func TestGroups_Prefs_PermissionMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	uc := NewGroups(bunrepo.NewProjectRepository(db), bunrepo.NewUserRepository(db), bunrepo.NewGroupRepository(db), bunrepo.NewMembershipRepository(db))
	serverCtx := serverActorCtx(ctx, projectID)
	group, err := uc.CreateGroup(serverCtx, projectID, "Perm Group", nil)
	require.NoError(t, err)

	writeOK := []databases.Principal{
		{Roles: []string{"keys"}},
		{Roles: []string{"admin"}},
		{Roles: []string{"users", "user:member-1", "group:" + group.ID}},
	}
	for _, p := range writeOK {
		updated, err := uc.UpdateGroupPrefs(serverCtx, projectID, group.ID, map[string]any{"by": p.Roles[0]}, p)
		require.NoError(t, err, "principal %v 应可写 prefs", p.Roles)
		require.Equal(t, map[string]any{"by": p.Roles[0]}, updated)
	}

	// 静态表无文档 ACE：写权限由拦截器把关，use-case 层不按 Principal 拒写。
	// 入口双面守卫对端用户放行（RequireEndUser），bystander 以端用户主体写入。
	bystander := databases.Principal{Roles: []string{"users", "user:bystander"}}
	bystanderCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser, ProjectID: projectID, UserID: "bystander",
	})
	prefs, err := uc.GetGroupPrefs(ctx, projectID, group.ID, bystander)
	require.NoError(t, err)
	require.NotNil(t, prefs)

	updated, err := uc.UpdateGroupPrefs(bystanderCtx, projectID, group.ID, map[string]any{"by": "bystander"}, bystander)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"by": "bystander"}, updated)
}
