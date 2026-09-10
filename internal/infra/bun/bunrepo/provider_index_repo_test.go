package bunrepo_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	domainpayments "github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestProviderIndex_KindsDoNotCollide 锁设计 §9.2 / PR6 验收「index 分 kind
// 不碰撞」：PK 是 (provider, kind, provider_ref)——同一 provider 下同一 ref
// 可按不同 kind 并存（payment_session 与 payment_order 指向不同项目），
// Lookup 严格按 kind 命中，互不串线。
func TestProviderIndex_KindsDoNotCollide(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	defer func() { _ = db.Close() }()

	p1, _, c1 := testutil.CreateTestProject(ctx, db)
	defer c1()
	p2, _, c2 := testutil.CreateTestProject(ctx, db)
	defer c2()

	repo := bunrepo.NewProviderIndexRepository(db)

	const sharedRef = "shared_ref_1"
	require.NoError(t, repo.Upsert(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentSession, sharedRef, p1))
	// 同 provider 同 ref、不同 kind：不视为冲突，可指向另一项目。
	require.NoError(t, repo.Upsert(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentOrder, sharedRef, p2))

	got, err := repo.Lookup(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentSession, sharedRef)
	require.NoError(t, err)
	require.Equal(t, p1, got)

	got, err = repo.Lookup(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentOrder, sharedRef)
	require.NoError(t, err)
	require.Equal(t, p2, got)

	// 未登记的 kind 未命中（Lookup 的 miss 语义由调用方映射 ErrProviderIndexMiss）。
	got, err = repo.Lookup(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindSubscription, sharedRef)
	require.NoError(t, err)
	require.Empty(t, got)

	// 同 (provider, kind, ref) 已指向另一项目时改指：PermissionDenied
	// （防跨项目劫持既有渠道引用——iOS VerifyReceipt 并发领取依赖此语义）。
	err = repo.Upsert(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentSession, sharedRef, p2)
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// 幂等：同项目重复 Upsert 不报错、不改另一 kind。
	require.NoError(t, repo.Upsert(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentSession, sharedRef, p1))
	got, err = repo.Lookup(ctx, domainpayments.ProviderStripe, domainpayments.IndexKindPaymentOrder, sharedRef)
	require.NoError(t, err)
	require.Equal(t, p2, got)
}
