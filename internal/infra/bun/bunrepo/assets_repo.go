package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/assets"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/uptrace/bun/driver/pgdriver"
)

// assetDefRepo 实现 assets.DefRepo（项目 schema asset_defs）。
type assetDefRepo struct {
	db *clients.Database
}

// NewAssetDefRepository 构造资产定义仓储。
func NewAssetDefRepository(db *clients.Database) assets.DefRepo {
	return &assetDefRepo{db: db}
}

func (r *assetDefRepo) Insert(ctx context.Context, def *assets.Def) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, def.ProjectID, "asset_defs", "ad")
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(mapDefToModel(def)).ModelTableExpr(expr, sch).Exec(ctx2)
	if isAssetUniqueViolation(err) {
		return assets.ErrDuplicateCode
	}
	return err
}

func (r *assetDefRepo) GetByID(ctx context.Context, projectID, defID string) (*assets.Def, error) {
	return r.selectDef(ctx, projectID, "ad.id = ?", defID, "")
}

func (r *assetDefRepo) GetByCode(ctx context.Context, projectID, code string) (*assets.Def, error) {
	return r.selectDef(ctx, projectID, "ad.code = ?", code, "")
}

func (r *assetDefRepo) GetByCodeForShare(ctx context.Context, projectID, code string) (*assets.Def, error) {
	return r.selectDef(ctx, projectID, "ad.code = ?", code, "SHARE")
}

func (r *assetDefRepo) GetByIDForShare(ctx context.Context, projectID, defID string) (*assets.Def, error) {
	return r.selectDef(ctx, projectID, "ad.id = ?", defID, "SHARE")
}

func (r *assetDefRepo) selectDef(ctx context.Context, projectID, pred string, arg any, lock string) (*assets.Def, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_defs", "ad")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.AssetDef)(nil)).ModelTableExpr(expr, sch).
		Where("ad.project_id = ?", projectID).
		Where(pred, arg)
	if lock != "" {
		q = q.For(lock)
	}
	m := new(model.AssetDef)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapDefToDomain(m), nil
}

func (r *assetDefRepo) List(ctx context.Context, projectID string, includeArchived bool, limit int, before time.Time) ([]assets.Def, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_defs", "ad")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetDef
	q := conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ad.project_id = ?", projectID).
		Where("ad.created_at < ?", before)
	if !includeArchived {
		q = q.Where("ad.status = ?", string(assets.DefStatusActive))
	}
	err = q.Order("ad.created_at DESC").Limit(limit).Scan(ctx2)
	if err != nil {
		return nil, err
	}
	out := make([]assets.Def, len(rows))
	for i := range rows {
		out[i] = *mapDefToDomain(&rows[i])
	}
	return out, nil
}

func (r *assetDefRepo) Update(ctx context.Context, def *assets.Def) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, def.ProjectID, "asset_defs", "ad")
	if err != nil {
		return err
	}
	// 列白名单（bun 更新写规范）：id/project_id/code/class/created_at 不可变。
	// 指针列 nil 经 DefaultPlaceholder 渲染为 DEFAULT（= NULL，各指针列无
	// 非平凡默认），语义与显式 NULL 一致；新可变列在此显式登记。
	_, err = conn.NewUpdate().Model(mapDefToModel(def)).ModelTableExpr(expr, sch).
		Column("name", "decimals", "max_quantity", "expires_in",
			"tradable", "unique_per_owner", "upgradeable", "metadata", "status", "updated_at").
		WherePK().
		Where("ad.project_id = ?", def.ProjectID).
		Exec(ctx2)
	return err
}

// assetHoldingRepo 实现 assets.HoldingRepo。
type assetHoldingRepo struct {
	db *clients.Database
}

// NewAssetHoldingRepository 构造持有仓储。
func NewAssetHoldingRepository(db *clients.Database) assets.HoldingRepo {
	return &assetHoldingRepo{db: db}
}

func (r *assetHoldingRepo) Insert(ctx context.Context, h *assets.Holding) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, h.ProjectID, "asset_holdings", "ah")
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(mapHoldingToModel(h)).ModelTableExpr(expr, sch).Exec(ctx2)
	return err
}

func (r *assetHoldingRepo) GetByID(ctx context.Context, projectID, holdingID string) (*assets.Holding, error) {
	return r.selectHolding(ctx, projectID, holdingID, "")
}

func (r *assetHoldingRepo) GetByIDForUpdate(ctx context.Context, projectID, holdingID string) (*assets.Holding, error) {
	return r.selectHolding(ctx, projectID, holdingID, "UPDATE")
}

func (r *assetHoldingRepo) selectHolding(ctx context.Context, projectID, holdingID, lock string) (*assets.Holding, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_holdings", "ah")
	if err != nil {
		return nil, err
	}
	q := conn.NewSelect().Model((*model.AssetHolding)(nil)).ModelTableExpr(expr, sch).
		Where("ah.project_id = ?", projectID).
		Where("ah.id = ?", holdingID)
	if lock != "" {
		q = q.For(lock)
	}
	m := new(model.AssetHolding)
	if err := q.Scan(ctx2, m); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapHoldingToDomain(m), nil
}

func (r *assetHoldingRepo) ListForUpdate(ctx context.Context, projectID string, ownerType assets.OwnerType, ownerID, defID string) ([]assets.Holding, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_holdings", "ah")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetHolding
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ah.project_id = ?", projectID).
		Where("ah.owner_type = ?", string(ownerType)).
		Where("ah.owner_id = ?", ownerID).
		Where("ah.def_id = ?", defID).
		OrderExpr("ah.expires_at ASC NULLS LAST, ah.id ASC").
		For("UPDATE").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapHoldingsToDomain(rows), nil
}

func (r *assetHoldingRepo) ListByOwner(ctx context.Context, projectID string, ownerType assets.OwnerType, ownerID string, limit int, before time.Time) ([]assets.Holding, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_holdings", "ah")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetHolding
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ah.project_id = ?", projectID).
		Where("ah.owner_type = ?", string(ownerType)).
		Where("ah.owner_id = ?", ownerID).
		Where("ah.created_at < ?", before).
		Order("ah.created_at DESC").
		Limit(limit).
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapHoldingsToDomain(rows), nil
}

func (r *assetHoldingRepo) Update(ctx context.Context, h *assets.Holding, expectVersion int64) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, h.ProjectID, "asset_holdings", "ah")
	if err != nil {
		return err
	}
	res, err := conn.NewUpdate().Model((*model.AssetHolding)(nil)).ModelTableExpr(expr, sch).
		Set("quantity = ?", h.Quantity).
		Set("expires_at = ?", h.ExpiresAt).
		Set("level = ?", h.Level).
		Set("metadata = ?", h.Metadata).
		Set("bucket_key = ?", h.BucketKey).
		Set("version = ?", h.Version).
		Set("updated_at = ?", h.UpdatedAt).
		Where("ah.id = ?", h.ID).
		Where("ah.project_id = ?", h.ProjectID).
		Where("ah.version = ?", expectVersion).
		Exec(ctx2)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return assets.ErrConcurrent
	}
	return nil
}

func (r *assetHoldingRepo) Delete(ctx context.Context, projectID, holdingID string, expectVersion int64) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_holdings", "ah")
	if err != nil {
		return err
	}
	res, err := conn.NewDelete().Model((*model.AssetHolding)(nil)).ModelTableExpr(expr, sch).
		Where("ah.id = ?", holdingID).
		Where("ah.project_id = ?", projectID).
		Where("ah.version = ?", expectVersion).
		Exec(ctx2)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return assets.ErrConcurrent
	}
	return nil
}

func (r *assetHoldingRepo) ListExpiredInProject(ctx context.Context, projectID string, now time.Time, limit int) ([]assets.Holding, error) {
	if limit <= 0 {
		return nil, nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, _, _, err := Scoped(ctx2, r.db, projectID, "asset_holdings", "ah"); err != nil {
		return nil, err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return nil, err
	}
	var rows []model.AssetHolding
	err = r.db.Conn(ctx2).NewRaw(fmt.Sprintf(`
SELECT id, project_id, owner_type, owner_id, def_id, quantity, expires_at,
       level, metadata, bucket_key, version, created_at, updated_at
FROM %s.asset_holdings
WHERE expires_at IS NOT NULL AND expires_at <= ?
ORDER BY expires_at
LIMIT ?
FOR UPDATE SKIP LOCKED`, quoted), now, limit).Scan(ctx2, &rows)
	if err != nil {
		return nil, err
	}
	return mapHoldingsToDomain(rows), nil
}

func (r *assetHoldingRepo) ListAllInProject(ctx context.Context, projectID string) ([]assets.Holding, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_holdings", "ah")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetHolding
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ah.project_id = ?", projectID).
		Order("ah.owner_id", "ah.def_id", "ah.id").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapHoldingsToDomain(rows), nil
}

// assetLedgerRepo 实现 assets.LedgerRepo。
type assetLedgerRepo struct {
	db *clients.Database
}

// NewAssetLedgerRepository 构造流水仓储。
func NewAssetLedgerRepository(db *clients.Database) assets.LedgerRepo {
	return &assetLedgerRepo{db: db}
}

func (r *assetLedgerRepo) InsertIfAbsent(ctx context.Context, e *assets.LedgerEntry) (*assets.LedgerEntry, bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, e.ProjectID, "asset_ledger_entries", "ale")
	if err != nil {
		return nil, false, err
	}
	res, err := conn.NewInsert().Model(mapLedgerToModel(e)).ModelTableExpr(expr, sch).
		On("CONFLICT (project_id, idempotency_key) DO NOTHING").
		Exec(ctx2)
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil, true, nil
	}
	existing, err := r.GetByIdempotencyKey(ctx2, e.ProjectID, e.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	return existing, false, nil
}

func (r *assetLedgerRepo) GetByIdempotencyKey(ctx context.Context, projectID, key string) (*assets.LedgerEntry, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_ledger_entries", "ale")
	if err != nil {
		return nil, err
	}
	m := new(model.AssetLedgerEntry)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("ale.project_id = ?", projectID).
		Where("ale.idempotency_key = ?", key).
		Scan(ctx2)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapLedgerToDomain(m), nil
}

func (r *assetLedgerRepo) ListByRef(ctx context.Context, projectID, refType, refID string) ([]assets.LedgerEntry, error) {
	if refID == "" {
		return nil, nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_ledger_entries", "ale")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetLedgerEntry
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ale.project_id = ?", projectID).
		Where("ale.ref_type = ?", refType).
		Where("ale.ref_id = ?", refID).
		Order("ale.created_at ASC", "ale.id ASC").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapLedgersToDomain(rows), nil
}

func (r *assetLedgerRepo) ListByOwner(ctx context.Context, projectID string, ownerType assets.OwnerType, ownerID, defID string, limit int, before time.Time) ([]assets.LedgerEntry, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_ledger_entries", "ale")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetLedgerEntry
	q := conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ale.project_id = ?", projectID).
		Where("ale.owner_type = ?", string(ownerType)).
		Where("ale.owner_id = ?", ownerID).
		Where("ale.created_at < ?", before)
	if defID != "" {
		q = q.Where("ale.def_id = ?", defID)
	}
	err = q.Order("ale.created_at DESC").Limit(limit).Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapLedgersToDomain(rows), nil
}

func (r *assetLedgerRepo) ListAllInProject(ctx context.Context, projectID string) ([]assets.LedgerEntry, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "asset_ledger_entries", "ale")
	if err != nil {
		return nil, err
	}
	var rows []model.AssetLedgerEntry
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ale.project_id = ?", projectID).
		Order("ale.created_at ASC", "ale.id ASC").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	return mapLedgersToDomain(rows), nil
}

func mapDefToModel(d *assets.Def) *model.AssetDef {
	return &model.AssetDef{
		ID:             d.ID,
		ProjectID:      d.ProjectID,
		Code:           d.Code,
		Name:           d.Name,
		Class:          string(d.Class),
		Decimals:       d.Decimals,
		MaxQuantity:    d.MaxQuantity,
		ExpiresIn:      d.ExpiresIn,
		Tradable:       d.Tradable,
		UniquePerOwner: d.UniquePerOwner,
		Upgradeable:    d.Upgradeable,
		Metadata:       d.Metadata,
		Status:         string(d.Status),
		CreatedAt:      d.CreatedAt,
		UpdatedAt:      d.UpdatedAt,
	}
}

func mapDefToDomain(m *model.AssetDef) *assets.Def {
	return &assets.Def{
		ID:             m.ID,
		ProjectID:      m.ProjectID,
		Code:           m.Code,
		Name:           m.Name,
		Class:          assets.Class(m.Class),
		Decimals:       m.Decimals,
		MaxQuantity:    m.MaxQuantity,
		ExpiresIn:      m.ExpiresIn,
		Tradable:       m.Tradable,
		UniquePerOwner: m.UniquePerOwner,
		Upgradeable:    m.Upgradeable,
		Metadata:       m.Metadata,
		Status:         assets.DefStatus(m.Status),
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
}

func mapHoldingToModel(h *assets.Holding) *model.AssetHolding {
	return &model.AssetHolding{
		ID:        h.ID,
		ProjectID: h.ProjectID,
		OwnerType: string(h.OwnerType),
		OwnerID:   h.OwnerID,
		DefID:     h.DefID,
		Quantity:  h.Quantity,
		ExpiresAt: h.ExpiresAt,
		Level:     h.Level,
		Metadata:  h.Metadata,
		BucketKey: h.BucketKey,
		Version:   h.Version,
		CreatedAt: h.CreatedAt,
		UpdatedAt: h.UpdatedAt,
	}
}

func mapHoldingToDomain(m *model.AssetHolding) *assets.Holding {
	return &assets.Holding{
		ID:        m.ID,
		ProjectID: m.ProjectID,
		OwnerType: assets.OwnerType(m.OwnerType),
		OwnerID:   m.OwnerID,
		DefID:     m.DefID,
		Quantity:  m.Quantity,
		ExpiresAt: m.ExpiresAt,
		Level:     m.Level,
		Metadata:  m.Metadata,
		BucketKey: m.BucketKey,
		Version:   m.Version,
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}

func mapHoldingsToDomain(rows []model.AssetHolding) []assets.Holding {
	out := make([]assets.Holding, len(rows))
	for i := range rows {
		out[i] = *mapHoldingToDomain(&rows[i])
	}
	return out
}

func mapLedgerToModel(e *assets.LedgerEntry) *model.AssetLedgerEntry {
	return &model.AssetLedgerEntry{
		ID:             e.ID,
		ProjectID:      e.ProjectID,
		HoldingID:      nullIfEmpty(e.HoldingID),
		OwnerType:      string(e.OwnerType),
		OwnerID:        e.OwnerID,
		DefID:          e.DefID,
		Kind:           string(e.Kind),
		Delta:          e.Delta,
		QuantityAfter:  e.QuantityAfter,
		ExpiresAt:      e.ExpiresAt,
		BucketKey:      e.BucketKey,
		RefType:        nullIfEmpty(e.RefType),
		RefID:          nullIfEmpty(e.RefID),
		IdempotencyKey: e.IdempotencyKey,
		TxID:           nullIfEmpty(e.TxID),
		Operator:       e.Operator,
		CreatedAt:      e.CreatedAt,
	}
}

func mapLedgerToDomain(m *model.AssetLedgerEntry) *assets.LedgerEntry {
	return &assets.LedgerEntry{
		ID:             m.ID,
		ProjectID:      m.ProjectID,
		HoldingID:      derefString(m.HoldingID),
		OwnerType:      assets.OwnerType(m.OwnerType),
		OwnerID:        m.OwnerID,
		DefID:          m.DefID,
		Kind:           assets.EntryKind(m.Kind),
		Delta:          m.Delta,
		QuantityAfter:  m.QuantityAfter,
		ExpiresAt:      m.ExpiresAt,
		BucketKey:      m.BucketKey,
		RefType:        derefString(m.RefType),
		RefID:          derefString(m.RefID),
		IdempotencyKey: m.IdempotencyKey,
		TxID:           derefString(m.TxID),
		Operator:       m.Operator,
		CreatedAt:      m.CreatedAt,
	}
}

func mapLedgersToDomain(rows []model.AssetLedgerEntry) []assets.LedgerEntry {
	out := make([]assets.LedgerEntry, len(rows))
	for i := range rows {
		out[i] = *mapLedgerToDomain(&rows[i])
	}
	return out
}

// isAssetUniqueViolation 报告是否为 PG unique_violation（建 def code 冲突等）。
func isAssetUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23505"
	}
	s := err.Error()
	return strings.Contains(s, "SQLSTATE 23505") || strings.Contains(s, "unique constraint")
}
