package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
)

func TestAuditRepository_ListByActor(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewAuditRepository(db)

	insert := func(projectID, actorID, action string, createdAt time.Time) {
		require.NoError(t, repo.Insert(ctx, &audit.Entry{
			ProjectID: projectID,
			ActorID:   actorID,
			ActorKind: "end_user",
			Action:    action,
			Status:    "success",
			IP:        "127.0.0.1",
			CreatedAt: createdAt,
		}))
	}

	base := time.Now()
	for i := 0; i < 5; i++ {
		insert(projectID, "user-1", "/torchwood.client.v1.AccountService/Me", base.Add(time.Duration(i)*time.Second))
	}
	// 另一用户的数据不可见。
	insert(projectID, "user-2", "/torchwood.client.v1.AccountService/Me", base.Add(100*time.Second))
	// 另一项目的数据不可见。
	insert("proj-other", "user-1", "/torchwood.client.v1.AccountService/Me", base.Add(200*time.Second))

	entries, err := repo.ListByActor(ctx, projectID, "user-1", 0)
	require.NoError(t, err)
	require.Len(t, entries, 5)
	// DESC 排序。
	for i := 1; i < len(entries); i++ {
		require.True(t, entries[i-1].CreatedAt.After(entries[i].CreatedAt))
	}
	require.Equal(t, "127.0.0.1", entries[0].IP)

	// limit 生效。
	limited, err := repo.ListByActor(ctx, projectID, "user-1", 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)

	// limit 上限 100。
	many, err := repo.ListByActor(ctx, projectID, "user-1", 1000)
	require.NoError(t, err)
	require.Len(t, many, 5)
}

func TestAuditRepository_List(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	repo := bunrepo.NewAuditRepository(db)

	base := time.Now()
	insert := func(projectID, actorID, action, status, resourceID string, createdAt time.Time) {
		require.NoError(t, repo.Insert(ctx, &audit.Entry{
			ProjectID:  projectID,
			ActorID:    actorID,
			ActorKind:  "service",
			Action:     action,
			Status:     status,
			ResourceID: resourceID,
			CreatedAt:  createdAt,
		}))
	}
	updateAction := "/torchwood.server.v1.FunctionsService/UpdateFunction"
	// 本项目：3 条 update + 1 条 denied。
	insert(projectID, "key-1", updateAction, "success", "fn-a", base.Add(1*time.Second))
	insert(projectID, "key-1", updateAction, "success", "fn-a", base.Add(2*time.Second))
	insert(projectID, "key-2", updateAction, "success", "fn-b", base.Add(3*time.Second))
	insert(projectID, "key-2", updateAction, "PermissionDenied", "fn-b", base.Add(4*time.Second))
	// 平台级行（project_id NULL）。
	insert("", "admin-1", "/torchwood.console.v1.AdminsService/CreateAdmin", "success", "", base.Add(5*time.Second))
	// 其他项目行。
	insert("proj-other", "key-9", updateAction, "success", "fn-x", base.Add(6*time.Second))

	// 项目作用域：只见本项目 4 条，DESC 排序。
	entries, total, err := repo.List(ctx, audit.ListFilter{ProjectID: projectID, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Len(t, entries, 4)
	for i := 1; i < len(entries); i++ {
		require.True(t, entries[i-1].CreatedAt.After(entries[i].CreatedAt))
	}

	// action 过滤 + status 过滤叠加。
	entries, total, err = repo.List(ctx, audit.ListFilter{
		ProjectID: projectID, Action: updateAction, Status: "PermissionDenied", PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, entries, 1)
	require.Equal(t, "fn-b", entries[0].ResourceID)

	// actor 过滤。
	_, total, err = repo.List(ctx, audit.ListFilter{ProjectID: projectID, ActorID: "key-1", PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 2, total)

	// include_platform：本项目 + 平台级（不含其他项目）。
	_, total, err = repo.List(ctx, audit.ListFilter{ProjectID: projectID, IncludePlatform: true, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 5, total)

	// all_projects：全部 6 条。
	_, total, err = repo.List(ctx, audit.ListFilter{AllProjects: true, PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 6, total)

	// 时间范围（闭区间，取 [t+2s, t+5s]）。
	after := base.Add(2 * time.Second)
	before := base.Add(5 * time.Second)
	_, total, err = repo.List(ctx, audit.ListFilter{
		ProjectID: projectID, CreatedAfter: &after, CreatedBefore: &before, PageSize: 50,
	})
	require.NoError(t, err)
	require.Equal(t, 3, total)

	// 分页：PageSize=2 → 两页 + Offset 翻页。
	page1, total, err := repo.List(ctx, audit.ListFilter{ProjectID: projectID, PageSize: 2})
	require.NoError(t, err)
	require.Equal(t, 4, total)
	require.Len(t, page1, 2)
	page2, _, err := repo.List(ctx, audit.ListFilter{ProjectID: projectID, PageSize: 2, Offset: 2})
	require.NoError(t, err)
	require.Len(t, page2, 2)
	require.NotEqual(t, page1[0].ID, page2[0].ID)

	// resource_id 过滤。
	_, total, err = repo.List(ctx, audit.ListFilter{ProjectID: projectID, ResourceID: "fn-a", PageSize: 50})
	require.NoError(t, err)
	require.Equal(t, 2, total)
}
