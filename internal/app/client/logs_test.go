package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAccount_ListLogs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccountWithRedis(mfaTestConfig(), projectRepo, db, nil)

	user, _, _, _, err := account.SignUp(ctx, SignUpCommand{
		ProjectID: projectID,
		Email:     "logs-user@torchwood.local",
		Password:  "User@123",
	})
	require.NoError(t, err)

	auditRepo := bunrepo.NewAuditRepository(db)
	account.auditRepo = auditRepo

	userCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		ProjectID: projectID,
		UserID:    user.ID,
		Email:     user.Email,
		Roles:     []string{"users", "user:" + user.ID},
	})

	base := time.Now()
	insert := func(actorID, action string, offset time.Duration) {
		require.NoError(t, auditRepo.Insert(ctx, &audit.Entry{
			ProjectID: projectID,
			ActorID:   actorID,
			ActorKind: "end_user",
			Action:    action,
			Status:    "success",
			IP:        "127.0.0.1",
			UserAgent: "go-test",
			CreatedAt: base.Add(offset),
		}))
	}
	for i := 0; i < 4; i++ {
		insert(user.ID, "/torchwood.client.v1.AccountService/Me", time.Duration(i)*time.Second)
	}
	insert("other-user", "/torchwood.client.v1.AccountService/Me", 10*time.Second)

	logs, err := account.ListLogs(userCtx, 0)
	require.NoError(t, err)
	require.Len(t, logs, 4)
	// DESC 排序。
	for i := 1; i < len(logs); i++ {
		require.True(t, logs[i-1].CreatedAt.After(logs[i].CreatedAt))
	}
	// 只含本用户数据。
	for _, e := range logs {
		require.Equal(t, user.ID, e.ActorID)
	}
	require.Equal(t, "/torchwood.client.v1.AccountService/Me", logs[0].Action)
	require.Equal(t, "127.0.0.1", logs[0].IP)
	require.Equal(t, "go-test", logs[0].UserAgent)

	// limit 归一化。
	require.Len(t, mustListLogs(t, account, userCtx, 2), 2)
	require.Len(t, mustListLogs(t, account, userCtx, -5), 4)   // 默认 50 > 4 条
	require.Len(t, mustListLogs(t, account, userCtx, 1000), 4) // 上限 100
}

func mustListLogs(t *testing.T, account *Account, ctx context.Context, limit int32) []audit.Entry {
	t.Helper()
	logs, err := account.ListLogs(ctx, limit)
	require.NoError(t, err)
	return logs
}

func TestAccount_ListLogs_Unauthenticated(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)

	projectRepo := bunrepo.NewProjectRepository(db)

	account := NewTestAccountWithRedis(mfaTestConfig(), projectRepo, db, nil)

	_, err := account.ListLogs(ctx, 10)
	require.Error(t, err)
	st, _ := status.FromError(err)
	require.Equal(t, codes.Unauthenticated, st.Code())
}
