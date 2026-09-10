package bunrepo_test

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
)

// newLiveSub 构造一张订阅行（Status 决定是否占 live unique 位）。
func newLiveSub(projectID, userID, planID, key string, status domainsubs.Status, seq int) *domainsubs.Subscription {
	now := time.Now().UTC()
	return &domainsubs.Subscription{
		ID:                 fmt.Sprintf("sub_j4_%d", seq),
		ProjectID:          projectID,
		UserID:             userID,
		PlanID:             planID,
		Mode:               domainsubs.ModePlatform,
		Status:             status,
		CurrentPeriodStart: now,
		CurrentPeriodEnd:   now.Add(30 * 24 * time.Hour),
		Benefits: domainsubs.Benefits{
			Grants:       []domainsubs.BenefitGrant{{AssetCode: "gold", Quantity: 1}},
			Entitlements: []domainsubs.BenefitEntitlement{{AssetCode: "vip", Tier: 1}},
		},
		IdempotencyKey: key,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
}

// TestSubscriptionRepo_LiveUniqueIndex（E-P2-5 / J4-3）：partial unique index
// 保证同 (project, user, plan) 至多一条活跃（trialing/active/past_due）订阅；
// repo 把 23505 映射为 ErrAlreadySubscribed；终态行不占位，取消后可重订。
func TestSubscriptionRepo_LiveUniqueIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	plansRepo := bunrepo.NewSubscriptionPlanRepository(db)
	now := time.Now().UTC()
	plan := &domainsubs.Plan{
		ID: "plan_j4", ProjectID: projectID, Code: "pro", Name: "Pro",
		Amount: 1000, Currency: "USD", Interval: domainsubs.IntervalMonth,
		Benefits: domainsubs.Benefits{
			Grants:       []domainsubs.BenefitGrant{{AssetCode: "gold", Quantity: 1}},
			Entitlements: []domainsubs.BenefitEntitlement{{AssetCode: "vip", Tier: 1}},
		},
		Status: domainsubs.PlanStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, plansRepo.Insert(ctx, plan))

	repo := bunrepo.NewSubscriptionRepository(db)

	// 首条活跃订阅成功。
	_, inserted, err := repo.Insert(ctx, newLiveSub(projectID, "u1", plan.ID, "k1", domainsubs.StatusActive, 1))
	require.NoError(t, err)
	require.True(t, inserted)

	// 同 user+plan 第二条活跃（不同幂等键）：live unique 直接拒绝。
	_, _, err = repo.Insert(ctx, newLiveSub(projectID, "u1", plan.ID, "k2", domainsubs.StatusActive, 2))
	require.ErrorIs(t, err, domainsubs.ErrAlreadySubscribed)

	// past_due 同样占位（与 ListNonTerminalByUserPlan 语义一致）。
	_, _, err = repo.Insert(ctx, newLiveSub(projectID, "u1", plan.ID, "k3", domainsubs.StatusPastDue, 3))
	require.ErrorIs(t, err, domainsubs.ErrAlreadySubscribed)

	// 不同 user 不冲突。
	_, inserted, err = repo.Insert(ctx, newLiveSub(projectID, "u2", plan.ID, "k4", domainsubs.StatusActive, 4))
	require.NoError(t, err)
	require.True(t, inserted)

	// 取消后（终态不占位）可重新订阅。
	first, err := repo.GetByID(ctx, projectID, "sub_j4_1")
	require.NoError(t, err)
	first.Status = domainsubs.StatusCanceled
	first.UpdatedAt = time.Now().UTC()
	require.NoError(t, repo.Update(ctx, first, domainsubs.StatusActive))
	_, inserted, err = repo.Insert(ctx, newLiveSub(projectID, "u1", plan.ID, "k5", domainsubs.StatusActive, 5))
	require.NoError(t, err)
	require.True(t, inserted)
}

// TestSubscriptionRepo_ConcurrentInsertSingleLive（E-P2-5 / J4-3）：并发两个
// 不同幂等键 Insert 同 user+plan 活跃订阅，DB 约束保证恰一个 inserted。
func TestSubscriptionRepo_ConcurrentInsertSingleLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()
	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	defer cleanup()

	plansRepo := bunrepo.NewSubscriptionPlanRepository(db)
	now := time.Now().UTC()
	plan := &domainsubs.Plan{
		ID: "plan_j4c", ProjectID: projectID, Code: "pro", Name: "Pro",
		Amount: 1000, Currency: "USD", Interval: domainsubs.IntervalMonth,
		Benefits: domainsubs.Benefits{
			Grants:       []domainsubs.BenefitGrant{{AssetCode: "gold", Quantity: 1}},
			Entitlements: []domainsubs.BenefitEntitlement{{AssetCode: "vip", Tier: 1}},
		},
		Status: domainsubs.PlanStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, plansRepo.Insert(ctx, plan))

	repo := bunrepo.NewSubscriptionRepository(db)
	start := make(chan struct{})
	insertedCh := make(chan bool, 2)
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 1; i <= 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub := newLiveSub(projectID, "u1", plan.ID, fmt.Sprintf("kc-%d", i), domainsubs.StatusActive, 10+i)
			<-start
			_, inserted, err := repo.Insert(ctx, sub)
			insertedCh <- inserted
			errCh <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(insertedCh)
	close(errCh)

	insertedCount := 0
	for inserted := range insertedCh {
		if inserted {
			insertedCount++
		}
	}
	for err := range errCh {
		if err == nil {
			continue // 赢者
		}
		require.ErrorIs(t, err, domainsubs.ErrAlreadySubscribed, "败者必须映射 ErrAlreadySubscribed")
	}
	require.Equal(t, 1, insertedCount)
}
