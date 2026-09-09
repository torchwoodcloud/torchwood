package assets

import (
	"context"
	"fmt"
	"time"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
)

type replayKey struct {
	ownerType string
	ownerID   string
	defID     string
	bucketKey string
	expUnix   int64 // 0 = nil expires_at
}

func expUnix(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UTC().UnixMicro()
}

// Reconcile 校验流水重放 = holdings 快照（含 quantity_after 链路）。
// 一期手动触发（Server RPC / 测试）。
func (a *Assets) Reconcile(ctx context.Context) (*domainassets.ReconcileReport, error) {
	if err := requireAssetWrite(ctx); err != nil {
		return nil, err
	}
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	return a.reconcileProject(ctx, projectID)
}

func (a *Assets) reconcileProject(ctx context.Context, projectID string) (*domainassets.ReconcileReport, error) {
	now := a.ts()
	holdings, err := a.holdings.ListAllInProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	entries, err := a.ledger.ListAllInProject(ctx, projectID)
	if err != nil {
		return nil, err
	}

	replayed := map[replayKey]int64{}
	type step struct {
		delta, after int64
	}
	chains := map[string][]step{} // holding_id → 按时间序

	for i := range entries {
		e := entries[i]
		k := replayKey{
			ownerType: string(e.OwnerType),
			ownerID:   e.OwnerID,
			defID:     e.DefID,
			bucketKey: e.BucketKey,
			expUnix:   expUnix(e.ExpiresAt),
		}
		replayed[k] += e.Delta
		if e.HoldingID != "" {
			chains[e.HoldingID] = append(chains[e.HoldingID], step{delta: e.Delta, after: e.QuantityAfter})
		}
	}

	var drifts []domainassets.Drift
	seen := map[replayKey]struct{}{}
	for i := range holdings {
		h := holdings[i]
		k := replayKey{
			ownerType: string(h.OwnerType),
			ownerID:   h.OwnerID,
			defID:     h.DefID,
			bucketKey: h.BucketKey,
			expUnix:   expUnix(h.ExpiresAt),
		}
		seen[k] = struct{}{}
		want := replayed[k]
		if want != h.Quantity {
			drifts = append(drifts, domainassets.Drift{
				ProjectID:   projectID,
				OwnerType:   h.OwnerType,
				OwnerID:     h.OwnerID,
				DefID:       h.DefID,
				ExpiresAt:   h.ExpiresAt,
				BucketKey:   h.BucketKey,
				HoldingQty:  h.Quantity,
				ReplayedQty: want,
				HoldingID:   h.ID,
				Detail:      fmt.Sprintf("holding qty %d != replayed %d", h.Quantity, want),
			})
		}
	}
	for k, qty := range replayed {
		if _, ok := seen[k]; ok {
			continue
		}
		if qty == 0 {
			continue // 消耗/过期删行后重放为 0，属正常
		}
		var exp *time.Time
		if k.expUnix != 0 {
			t := time.UnixMicro(k.expUnix).UTC()
			exp = &t
		}
		drifts = append(drifts, domainassets.Drift{
			ProjectID:   projectID,
			OwnerType:   domainassets.OwnerType(k.ownerType),
			OwnerID:     k.ownerID,
			DefID:       k.defID,
			ExpiresAt:   exp,
			BucketKey:   k.bucketKey,
			HoldingQty:  0,
			ReplayedQty: qty,
			Detail:      fmt.Sprintf("replayed qty %d but holding missing", qty),
		})
	}

	var afterBreaks int
	for holdingID, chain := range chains {
		var running int64
		for _, s := range chain {
			running += s.delta
			if running != s.after {
				afterBreaks++
				drifts = append(drifts, domainassets.Drift{
					ProjectID:     projectID,
					HoldingID:     holdingID,
					HoldingQty:    s.after,
					ReplayedQty:   running,
					QuantityAfter: true,
					Detail:        fmt.Sprintf("quantity_after chain break on holding %s: running %d != after %d", holdingID, running, s.after),
				})
				break
			}
		}
	}

	for range drifts {
		assetLedgerDriftTotal.Inc()
	}

	return &domainassets.ReconcileReport{
		ProjectID:     projectID,
		Holdings:      len(holdings),
		Entries:       len(entries),
		Drifts:        drifts,
		CheckedAt:     now,
		ZeroDrift:     len(drifts) == 0,
		QuantityAfter: afterBreaks,
	}, nil
}
