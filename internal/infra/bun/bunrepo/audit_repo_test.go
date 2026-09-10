package bunrepo_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
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
