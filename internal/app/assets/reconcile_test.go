package assets

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
)

// TestReconcile_MutateExpiryMigrationNoDrift（S5 缺陷 1）：订阅续期经 Mutate
// 把 entitlement 到期时刻迁到新周期，流水按新桶落 delta=0 行。旧实现按
// (owner,def,bucket,expires_at) 分桶重放，会把同一 holding 撕成 T1/T2 两个
// 假桶产生双重假 drift；改按 holding 维度聚合后迁移前后均零 drift。
func TestReconcile_MutateExpiryMigrationNoDrift(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassEntitlement, "vip")
	t1 := env.now.Add(24 * time.Hour)
	t2 := env.now.Add(48 * time.Hour)
	g, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "vip", Quantity: 1, ExpiresAt: &t1, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	holdingID := g.Entries[0].HoldingID

	_, err = env.assets.Mutate(adminCtx("p1"), MutateCommand{
		HoldingID: holdingID, ExpiresAt: &t2, IdempotencyKey: "m1",
	})
	require.NoError(t, err)

	report, err := env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.True(t, report.ZeroDrift)
	require.Empty(t, report.Drifts)
	require.Equal(t, 1, report.Holdings)
	require.Equal(t, 2, report.Entries)

	// 连续第二期续期（T2 → T3）同样零 drift。
	t3 := env.now.Add(72 * time.Hour)
	_, err = env.assets.Mutate(adminCtx("p1"), MutateCommand{
		HoldingID: holdingID, ExpiresAt: &t3, IdempotencyKey: "m2",
	})
	require.NoError(t, err)
	report, err = env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.True(t, report.ZeroDrift)
	require.Empty(t, report.Drifts)
}

// TestReconcile_TamperedQuantityReportedWithHoldingID（S5 缺陷 1）：
// 真 drift（手工篡改持有数量）仍被报出，且归因到具体 holding。
func TestReconcile_TamperedQuantityReportedWithHoldingID(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	g, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 7, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	holdingID := g.Entries[0].HoldingID

	anyHolding(env.store).Quantity = 99
	report, err := env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.False(t, report.ZeroDrift)
	require.Len(t, report.Drifts, 1)
	d := report.Drifts[0]
	require.Equal(t, holdingID, d.HoldingID)
	require.Equal(t, int64(99), d.HoldingQty)
	require.Equal(t, int64(7), d.ReplayedQty)
	require.False(t, d.QuantityAfter)
}

// TestReconcile_OrphanLedgerSumReported：持有行被删但流水 Σdelta 未归零的
// 损坏现场仍报 drift（holding missing），归因字段取自该 holding 最后一条流水。
func TestReconcile_OrphanLedgerSumReported(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	g, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 5, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	holdingID := g.Entries[0].HoldingID

	for id := range env.store.holdings {
		delete(env.store.holdings, id)
	}
	report, err := env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.False(t, report.ZeroDrift)
	require.Len(t, report.Drifts, 1)
	d := report.Drifts[0]
	require.Equal(t, holdingID, d.HoldingID)
	require.Equal(t, int64(0), d.HoldingQty)
	require.Equal(t, int64(5), d.ReplayedQty)
	require.Contains(t, d.Detail, "holding missing")
}

// TestReconcile_ConsumeToZeroLeavesNoDrift：整桶消耗删行后 Σdelta 归零，
// 不报 orphan drift。
func TestReconcile_ConsumeToZeroLeavesNoDrift(t *testing.T) {
	env := setupAssets(t)
	env.createDef(t, domainassets.ClassCurrency, "gold")
	_, err := env.assets.Grant(adminCtx("p1"), GrantCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 3, IdempotencyKey: "g1",
	})
	require.NoError(t, err)
	_, err = env.assets.Consume(adminCtx("p1"), ConsumeCommand{
		OwnerID: "u1", DefCode: "gold", Quantity: 3, IdempotencyKey: "c1",
	})
	require.NoError(t, err)
	require.Empty(t, env.store.holdings)

	report, err := env.assets.Reconcile(adminCtx("p1"))
	require.NoError(t, err)
	require.True(t, report.ZeroDrift)
	require.Empty(t, report.Drifts)
	require.Equal(t, 0, report.Holdings)
	require.Equal(t, 2, report.Entries)
}
