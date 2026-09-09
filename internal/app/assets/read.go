package assets

import (
	"context"
	"time"

	domainassets "github.com/torchwoodcloud/torchwood/internal/domain/assets"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// HoldingView 是读路径持有投影（带 def_code / class）。
type HoldingView struct {
	Holding domainassets.Holding
	DefCode string
	Class   domainassets.Class
}

// LedgerView 是读路径流水投影。
type LedgerView struct {
	Entry   domainassets.LedgerEntry
	DefCode string
}

// ListMyAssets 返回本人未过期持有（读路径懒过滤）。
func (a *Assets) ListMyAssets(ctx context.Context, limit int, before time.Time) ([]HoldingView, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, err
	}
	return a.listOwnerAssets(ctx, projectID, userID, limit, before)
}

// ListUserAssets 返回指定用户未过期持有（Server / Console 只读查询）。
func (a *Assets) ListUserAssets(ctx context.Context, ownerID string, limit int, before time.Time) ([]HoldingView, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if ownerID == "" {
		return nil, status.Error(codes.InvalidArgument, "owner_id is required")
	}
	return a.listOwnerAssets(ctx, projectID, ownerID, limit, before)
}

func (a *Assets) listOwnerAssets(ctx context.Context, projectID, ownerID string, limit int, before time.Time) ([]HoldingView, error) {
	limit, before = normalizeList(limit, before)
	rows, err := a.holdings.ListByOwner(ctx, projectID, domainassets.OwnerTypeUser, ownerID, limit, before)
	if err != nil {
		return nil, err
	}
	now := a.ts()
	defs, err := a.defsByIDs(ctx, projectID, holdingDefIDs(rows))
	if err != nil {
		return nil, err
	}
	out := make([]HoldingView, 0, len(rows))
	for i := range rows {
		if rows[i].Expired(now) {
			continue
		}
		d := defs[rows[i].DefID]
		v := HoldingView{Holding: rows[i]}
		if d != nil {
			v.DefCode = d.Code
			v.Class = d.Class
		}
		out = append(out, v)
	}
	return out, nil
}

// ListMyLedger 返回本人流水。
func (a *Assets) ListMyLedger(ctx context.Context, defCode string, limit int, before time.Time) ([]LedgerView, error) {
	projectID, userID, err := endUser(ctx)
	if err != nil {
		return nil, err
	}
	return a.listOwnerLedger(ctx, projectID, userID, defCode, limit, before)
}

// ListUserLedger 返回指定用户流水（Server / Console 只读查询）。
func (a *Assets) ListUserLedger(ctx context.Context, ownerID, defCode string, limit int, before time.Time) ([]LedgerView, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if ownerID == "" {
		return nil, status.Error(codes.InvalidArgument, "owner_id is required")
	}
	return a.listOwnerLedger(ctx, projectID, ownerID, defCode, limit, before)
}

func (a *Assets) listOwnerLedger(ctx context.Context, projectID, ownerID, defCode string, limit int, before time.Time) ([]LedgerView, error) {
	limit, before = normalizeList(limit, before)
	var defID string
	if defCode != "" {
		code, err := validateCode(defCode)
		if err != nil {
			return nil, err
		}
		def, err := a.defs.GetByCode(ctx, projectID, code)
		if err != nil {
			return nil, err
		}
		if def == nil {
			return nil, status.Error(codes.NotFound, "asset def not found")
		}
		defID = def.ID
	}
	rows, err := a.ledger.ListByOwner(ctx, projectID, domainassets.OwnerTypeUser, ownerID, defID, limit, before)
	if err != nil {
		return nil, err
	}
	defs, err := a.defsByIDs(ctx, projectID, ledgerDefIDs(rows))
	if err != nil {
		return nil, err
	}
	out := make([]LedgerView, len(rows))
	for i := range rows {
		out[i] = LedgerView{Entry: rows[i]}
		if d := defs[rows[i].DefID]; d != nil {
			out[i].DefCode = d.Code
		}
	}
	return out, nil
}

// ListClientDefs 返回项目内 active 定义（Client 面）。
func (a *Assets) ListClientDefs(ctx context.Context, limit int, before time.Time) ([]domainassets.Def, error) {
	_, _, err := endUser(ctx)
	if err != nil {
		return nil, err
	}
	return a.ListDefs(ctx, false, limit, before)
}

func (a *Assets) defsByIDs(ctx context.Context, projectID string, ids []string) (map[string]*domainassets.Def, error) {
	out := make(map[string]*domainassets.Def, len(ids))
	for _, id := range ids {
		if id == "" || out[id] != nil {
			continue
		}
		d, err := a.defs.GetByID(ctx, projectID, id)
		if err != nil {
			return nil, err
		}
		if d != nil {
			out[id] = d
		}
	}
	return out, nil
}

func holdingDefIDs(rows []domainassets.Holding) []string {
	ids := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for i := range rows {
		if _, ok := seen[rows[i].DefID]; ok {
			continue
		}
		seen[rows[i].DefID] = struct{}{}
		ids = append(ids, rows[i].DefID)
	}
	return ids
}

func ledgerDefIDs(rows []domainassets.LedgerEntry) []string {
	ids := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for i := range rows {
		if _, ok := seen[rows[i].DefID]; ok {
			continue
		}
		seen[rows[i].DefID] = struct{}{}
		ids = append(ids, rows[i].DefID)
	}
	return ids
}
