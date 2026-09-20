package console_test

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/app/console"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestAdmins_LastOwnerGuard_ConcurrentOwnerChanges（缺陷修复验收）：剩 2 个
// owner 时并发降级/删除。修复前 ensureNotLastOwner 只做无锁 CountAdminsByRole，
// 与 Update/Delete 非同事务，两个并发变更可双双通过 count 检查 → 平台失去
// 全部管理入口；修复后 owner 变更收进 pg_advisory_xact_lock（bootstrapLockKey，
// setup.go SignUp 同款串行化），后到者在锁内 count 看到前者已提交的结果被拒。
func TestAdmins_LastOwnerGuard_ConcurrentOwnerChanges(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	repo := bunrepo.NewAdminRepository(db)
	uc := console.NewAdmins(repo, nil, nil)
	adminCtx := contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:   "bootstrap",
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "bootstrap",
	})

	created1, err := uc.Create(adminCtx, console.CreateAdminCommand{Email: "conc-owner-a@torchwood.local", Password: "Passw0rd!", Role: console.AdminRoleOwner})
	require.NoError(t, err)
	created2, err := uc.Create(adminCtx, console.CreateAdminCommand{Email: "conc-owner-b@torchwood.local", Password: "Passw0rd!", Role: console.AdminRoleOwner})
	require.NoError(t, err)

	// 并发对冲：A 降级 owner-a → member；B 删除 owner-b。
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, errs[0] = uc.Update(adminCtx, console.UpdateAdminCommand{
			ID: created1.ID, CallerID: "bootstrap", Role: console.AdminRoleMember,
		})
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = uc.Delete(adminCtx, created2.ID, "bootstrap")
	}()
	close(start)
	wg.Wait()

	var successes int
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	require.LessOrEqual(t, successes, 1, "并发 owner 变更至多一个能提交（advisory lock 串行化）：%v / %v", errs[0], errs[1])
	if errs[0] != nil {
		require.Equal(t, codes.FailedPrecondition, status.Code(errs[0]))
	}
	if errs[1] != nil {
		require.Equal(t, codes.FailedPrecondition, status.Code(errs[1]))
	}

	// 平台必须保留至少一个 owner。
	remaining, err := repo.CountAdminsByRole(ctx, console.AdminRoleOwner)
	require.NoError(t, err)
	require.GreaterOrEqual(t, remaining, int64(1), "平台必须保留至少一个 owner")
}
