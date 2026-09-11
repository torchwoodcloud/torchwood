package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// settlementRepo 实现 leaderboards.SettlementRepo（Phase 2：结榜发奖）。
type settlementRepo struct {
	db *clients.Database
}

// NewLeaderboardSettlementRepository 构造结算仓储。
func NewLeaderboardSettlementRepository(db *clients.Database) leaderboards.SettlementRepo {
	return &settlementRepo{db: db}
}

func (r *settlementRepo) ClaimPending(ctx context.Context, s *leaderboards.Settlement) (bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, s.ProjectID, "leaderboard_settlements", "ls")
	if err != nil {
		return false, err
	}
	res, err := conn.NewInsert().Model(mapSettlementToModel(s)).ModelTableExpr(expr, sch).
		On("CONFLICT (board_id, period_key) DO NOTHING").
		Exec(ctx2)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	return true, nil
}

func (r *settlementRepo) Get(ctx context.Context, projectID, boardID, periodKey string) (*leaderboards.Settlement, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_settlements", "ls")
	if err != nil {
		return nil, err
	}
	m := &model.LeaderboardSettlement{}
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("ls.project_id = ?", projectID).
		Where("ls.board_id = ?", boardID).
		Where("ls.period_key = ?", periodKey).
		Limit(1).
		Scan(ctx2)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mapSettlementToDomain(m), nil
}

func (r *settlementRepo) ListByBoard(ctx context.Context, projectID, boardID string, limit int) ([]leaderboards.Settlement, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_settlements", "ls")
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	var rows []model.LeaderboardSettlement
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("ls.project_id = ?", projectID).
		Where("ls.board_id = ?", boardID).
		Order("ls.period_key DESC").
		Limit(limit).
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	out := make([]leaderboards.Settlement, len(rows))
	for i := range rows {
		out[i] = *mapSettlementToDomain(&rows[i])
	}
	return out, nil
}

func (r *settlementRepo) ListGrants(ctx context.Context, projectID, settlementID string) ([]leaderboards.SettlementGrant, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_settlement_grants", "lsg")
	if err != nil {
		return nil, err
	}
	var rows []model.LeaderboardSettlementGrant
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("lsg.project_id = ?", projectID).
		Where("lsg.settlement_id = ?", settlementID).
		Order("lsg.rule_index ASC", "lsg.subject_id ASC").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	out := make([]leaderboards.SettlementGrant, len(rows))
	for i := range rows {
		out[i] = *mapSettlementGrantToDomain(&rows[i])
	}
	return out, nil
}

func (r *settlementRepo) SaveGrants(ctx context.Context, grants []*leaderboards.SettlementGrant) error {
	if len(grants) == 0 {
		return nil
	}
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, grants[0].ProjectID, "leaderboard_settlement_grants", "lsg")
	if err != nil {
		return err
	}
	for _, g := range grants {
		_, err := conn.NewInsert().Model(mapSettlementGrantToModel(g)).ModelTableExpr(expr, sch).
			On("CONFLICT (settlement_id, rule_index, subject_id) DO UPDATE SET status = EXCLUDED.status, error = EXCLUDED.error, updated_at = EXCLUDED.updated_at").
			Exec(ctx2)
		if err != nil {
			return err
		}
	}
	return nil
}

// statusSet 是状态更新的单条赋值：expr 带占位符时必须携带 value（bun 的
// Set 不带参数时 ? 会原样进 SQL——渲染护栏见 settlement 集成测试）。
type statusSet struct {
	expr  string
	value any // nil = 纯字面量（如 error = NULL）
}

func (r *settlementRepo) Complete(ctx context.Context, projectID, id string, settledAt time.Time, grantCount int32) error {
	return r.updateStatus(ctx, projectID, id, []statusSet{
		{"status = ?", leaderboards.SettlementStatusSettled},
		{"settled_at = ?", settledAt},
		{"grant_count = ?", grantCount},
		{"error = NULL", nil},
	})
}

func (r *settlementRepo) Fail(ctx context.Context, projectID, id string, msg string) error {
	return r.updateStatus(ctx, projectID, id, []statusSet{
		{"status = ?", leaderboards.SettlementStatusError},
		{"error = ?", msg},
	})
}

func (r *settlementRepo) Void(ctx context.Context, projectID, id string) error {
	return r.updateStatus(ctx, projectID, id, []statusSet{
		{"status = ?", leaderboards.SettlementStatusVoided},
	})
}

func (r *settlementRepo) ResetToPending(ctx context.Context, projectID, id string) error {
	return r.updateStatus(ctx, projectID, id, []statusSet{
		{"status = ?", leaderboards.SettlementStatusSettling},
		{"error = NULL", nil},
	})
}

func (r *settlementRepo) updateStatus(ctx context.Context, projectID, id string, sets []statusSet) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_settlements", "ls")
	if err != nil {
		return err
	}
	q := conn.NewUpdate().ModelTableExpr(expr, sch).Where("ls.id = ?", id)
	for _, kv := range sets {
		if kv.value == nil {
			q = q.Set(kv.expr)
		} else {
			q = q.Set(kv.expr, kv.value)
		}
	}
	_, err = q.Exec(ctx2)
	return err
}

func mapSettlementToModel(s *leaderboards.Settlement) *model.LeaderboardSettlement {
	m := &model.LeaderboardSettlement{
		ID:            s.ID,
		ProjectID:     s.ProjectID,
		BoardID:       s.BoardID,
		PeriodKey:     s.PeriodKey,
		Status:        s.Status,
		SealedAt:      s.SealedAt,
		SettledAt:     s.SettledAt,
		RulesSnapshot: s.RulesSnapshot,
		EntryCount:    s.EntryCount,
		GrantCount:    s.GrantCount,
		CreatedAt:     s.CreatedAt,
		UpdatedAt:     s.UpdatedAt,
	}
	if s.Error != "" {
		v := s.Error
		m.Error = &v
	}
	return m
}

func mapSettlementToDomain(m *model.LeaderboardSettlement) *leaderboards.Settlement {
	s := &leaderboards.Settlement{
		ProjectID:     m.ProjectID,
		ID:            m.ID,
		BoardID:       m.BoardID,
		PeriodKey:     m.PeriodKey,
		Status:        m.Status,
		SealedAt:      m.SealedAt,
		SettledAt:     m.SettledAt,
		RulesSnapshot: m.RulesSnapshot,
		EntryCount:    m.EntryCount,
		GrantCount:    m.GrantCount,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
	if m.Error != nil {
		s.Error = *m.Error
	}
	return s
}

func mapSettlementGrantToModel(g *leaderboards.SettlementGrant) *model.LeaderboardSettlementGrant {
	m := &model.LeaderboardSettlementGrant{
		ID:             g.ID,
		ProjectID:      g.ProjectID,
		SettlementID:   g.SettlementID,
		RuleIndex:      g.RuleIndex,
		SubjectID:      g.SubjectID,
		AssetCode:      g.AssetCode,
		Amount:         g.Amount,
		IdempotencyKey: g.IdempotencyKey,
		Status:         g.Status,
		CreatedAt:      g.CreatedAt,
		UpdatedAt:      g.UpdatedAt,
	}
	if g.Error != "" {
		v := g.Error
		m.Error = &v
	}
	return m
}

func mapSettlementGrantToDomain(m *model.LeaderboardSettlementGrant) *leaderboards.SettlementGrant {
	g := &leaderboards.SettlementGrant{
		ProjectID:      m.ProjectID,
		ID:             m.ID,
		SettlementID:   m.SettlementID,
		RuleIndex:      m.RuleIndex,
		SubjectID:      m.SubjectID,
		AssetCode:      m.AssetCode,
		Amount:         m.Amount,
		IdempotencyKey: m.IdempotencyKey,
		Status:         m.Status,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
	if m.Error != nil {
		g.Error = *m.Error
	}
	return g
}
