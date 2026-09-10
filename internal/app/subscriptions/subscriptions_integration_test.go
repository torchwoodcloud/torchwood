package subscriptions

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainsubs "github.com/torchwoodcloud/torchwood/internal/domain/subscriptions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// setupIntegrationUC 用真实 bunrepo（PG）装配 Subscribe 用例：Amount=0 的
// platform 计划不触发支付渠道，路径覆盖 Insert → live unique / 互斥检查 →
// benefits 履约（stub）→ publish（nil，no-op）。返回用例与项目 id。
func setupIntegrationUC(t *testing.T) (*Subscriptions, string) {
	t.Helper()
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	pid, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	plansRepo := bunrepo.NewSubscriptionPlanRepository(db)
	subsRepo := bunrepo.NewSubscriptionRepository(db)
	now := time.Now().UTC()
	plan := &domainsubs.Plan{
		ID:        "plan_j4_free",
		ProjectID: pid,
		Code:      "free",
		Name:      "Free",
		Amount:    0,
		Currency:  "USD",
		Interval:  domainsubs.IntervalMonth,
		Benefits: domainsubs.Benefits{
			Grants:       []domainsubs.BenefitGrant{{AssetCode: "gold", Quantity: 1}},
			Entitlements: []domainsubs.BenefitEntitlement{{AssetCode: "vip", Tier: 1}},
		},
		Status:    domainsubs.PlanStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, plansRepo.Insert(ctx, plan))
	uc := newSubscriptions(nil, db, plansRepo, subsRepo, &stubAssets{}, nil, nil, nil, nil, nil, nil, nil)
	return uc, pid
}

// TestIntegration_ConcurrentSubscribeSingleLive（E-P2-5 / J4-3）：并发两个
// 不同幂等键的 Subscribe 同 user + plan，必须恰一个成功、另一个
// ErrAlreadySubscribed（partial unique index 或事务内互斥检查兜底）。
func TestIntegration_ConcurrentSubscribeSingleLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, projectID := setupIntegrationUC(t)
	ctx := userCtx(projectID, "user_j4")

	const workers = 2
	start := make(chan struct{})
	results := make([]*SubscribeResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = uc.Subscribe(ctx, SubscribeCommand{
				PlanCode:       "free",
				Mode:           domainsubs.ModePlatform,
				IdempotencyKey: fmt.Sprintf("k-j4-%d", i),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	ok := 0
	for i := range errs {
		if errs[i] == nil {
			ok++
			require.NotNil(t, results[i].Subscription)
			continue
		}
		st, valid := status.FromError(errs[i])
		require.True(t, valid)
		require.Equal(t, codes.FailedPrecondition, st.Code(), "败者必须是 ErrAlreadySubscribed 语义，got %v", errs[i])
		require.Contains(t, st.Message(), "already subscribed")
	}
	require.Equal(t, 1, ok, "并发双开必须恰有一个成功")
}

// TestIntegration_TerminalReplayAndResubscribe（E-P2-5 附带 / J4-3）：
// 取消后同幂等键重放返回 FailedPrecondition（终态不作为成功重放返回），
// 换新键可重新订阅（终态行不占 live unique 位）。
func TestIntegration_TerminalReplayAndResubscribe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	uc, projectID := setupIntegrationUC(t)
	ctx := userCtx(projectID, "user_j4")

	r1, err := uc.Subscribe(ctx, SubscribeCommand{
		PlanCode: "free", Mode: domainsubs.ModePlatform, IdempotencyKey: "k-j4-term",
	})
	require.NoError(t, err)

	_, _, err = uc.ForceCancel(adminCtx(projectID), r1.Subscription.ID)
	require.NoError(t, err)

	_, err = uc.Subscribe(ctx, SubscribeCommand{
		PlanCode: "free", Mode: domainsubs.ModePlatform, IdempotencyKey: "k-j4-term",
	})
	require.Error(t, err)
	st, valid := status.FromError(err)
	require.True(t, valid)
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "terminated subscription")

	r2, err := uc.Subscribe(ctx, SubscribeCommand{
		PlanCode: "free", Mode: domainsubs.ModePlatform, IdempotencyKey: "k-j4-term-2",
	})
	require.NoError(t, err)
	require.False(t, r2.IdempotentReplay)
	require.Equal(t, domainsubs.StatusActive, r2.Subscription.Status)
}
