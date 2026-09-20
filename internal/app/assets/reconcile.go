package assets

import (
	"context"
	"fmt"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
)

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

// replayAgg 是单个 holding 的流水重放聚合。
type replayAgg struct {
	sum  int64
	last *domainassets.LedgerEntry // 时间序最后一条（删行后 drift 归因用）
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

	// 重放按 holding 维度聚合（S5 缺陷 1）：每个 holding_id 唯一映射到其
	// 当前桶，Mutate 迁移 expires_at 落的 delta=0 流水天然不破坏等式。
	// 旧实现按 (owner,def,bucket,expires_at) 分桶累加，续期把桶从 T1 迁到
	// T2 后被撕成两个假桶，产生双重假 drift 持续污染 assetLedgerDriftTotal。
	// 消耗/过期删行后 Σdelta 应为 0；所有写动词的流水均带 HoldingID。
	replayed := map[string]*replayAgg{}
	type step struct {
		delta, after int64
	}
	chains := map[string][]step{} // holding_id → 按时间序

	for i := range entries {
		e := entries[i]
		if e.HoldingID == "" {
			continue
		}
		agg := replayed[e.HoldingID]
		if agg == nil {
			agg = &replayAgg{}
			replayed[e.HoldingID] = agg
		}
		agg.sum += e.Delta
		agg.last = &entries[i]
		chains[e.HoldingID] = append(chains[e.HoldingID], step{delta: e.Delta, after: e.QuantityAfter})
	}

	var drifts []domainassets.Drift
	byID := make(map[string]struct{}, len(holdings))
	for i := range holdings {
		h := holdings[i]
		byID[h.ID] = struct{}{}
		var want int64
		if agg := replayed[h.ID]; agg != nil {
			want = agg.sum
		}
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
	for id, agg := range replayed {
		if _, ok := byID[id]; ok {
			continue
		}
		if agg.sum == 0 {
			continue // 消耗/过期删行后重放归零，属正常
		}
		last := agg.last
		drifts = append(drifts, domainassets.Drift{
			ProjectID:   projectID,
			OwnerType:   last.OwnerType,
			OwnerID:     last.OwnerID,
			DefID:       last.DefID,
			ExpiresAt:   last.ExpiresAt,
			BucketKey:   last.BucketKey,
			HoldingQty:  0,
			ReplayedQty: agg.sum,
			HoldingID:   id,
			Detail:      fmt.Sprintf("replayed qty %d but holding missing", agg.sum),
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
