package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun/driver/pgdriver"
)

// boardRepo 实现 leaderboards.BoardRepo。
type boardRepo struct {
	db *clients.Database
}

// NewLeaderboardBoardRepository 构造榜配置仓储。
func NewLeaderboardBoardRepository(db *clients.Database) leaderboards.BoardRepo {
	return &boardRepo{db: db}
}

func (r *boardRepo) Insert(ctx context.Context, b *leaderboards.Board) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, b.ProjectID, "leaderboard_boards", "lb")
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(mapBoardToModel(b)).ModelTableExpr(expr, sch).Exec(ctx2)
	if isLeaderboardUniqueViolation(err) {
		return leaderboards.ErrBoardExists
	}
	return err
}

func (r *boardRepo) Get(ctx context.Context, projectID, boardID string) (*leaderboards.Board, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_boards", "lb")
	if err != nil {
		return nil, err
	}
	m := &model.LeaderboardBoard{}
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("lb.project_id = ?", projectID).
		Where("lb.id = ?", boardID).
		Limit(1).
		Scan(ctx2)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mapBoardToDomain(m), nil
}

// leaderboardBoardUpdateColumns 是榜配置更新白名单（bun 更新写规范）。
// id/project_id/created_at 永不更新；period/sort/tiebreak/policy/tie_break
// 的"有条目后不可改"由 app 用例层经 CountInBoard 判定（无条目时允许改）。
var leaderboardBoardUpdateColumns = []string{
	"sort", "tiebreak_order", "tie_break", "period_kind", "period_tz", "policy",
	"value_min", "value_max", "client_submit", "per_subject_limit",
	"retention_periods", "subject_kind", "updated_at",
}

func (r *boardRepo) Update(ctx context.Context, b *leaderboards.Board) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, b.ProjectID, "leaderboard_boards", "lb")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model(mapBoardToModel(b)).ModelTableExpr(expr, sch).
		Column(leaderboardBoardUpdateColumns...).
		WherePK().
		Where("lb.project_id = ?", b.ProjectID).
		Exec(ctx2)
	return err
}

func (r *boardRepo) Delete(ctx context.Context, projectID, boardID string) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_boards", "lb")
	if err != nil {
		return err
	}
	// 条目经 FK ON DELETE CASCADE 级联删除（console 二次确认）。
	_, err = conn.NewDelete().ModelTableExpr(expr, sch).
		Where("lb.project_id = ?", projectID).
		Where("lb.id = ?", boardID).
		Exec(ctx2)
	return err
}

func (r *boardRepo) List(ctx context.Context, projectID string) ([]leaderboards.Board, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_boards", "lb")
	if err != nil {
		return nil, err
	}
	var rows []model.LeaderboardBoard
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("lb.project_id = ?", projectID).
		Order("lb.created_at DESC").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	out := make([]leaderboards.Board, len(rows))
	for i := range rows {
		out[i] = *mapBoardToDomain(&rows[i])
	}
	return out, nil
}

// entryRepo 实现 leaderboards.EntryRepo。
type entryRepo struct {
	db *clients.Database
}

// NewLeaderboardEntryRepository 构造条目仓储。
func NewLeaderboardEntryRepository(db *clients.Database) leaderboards.EntryRepo {
	return &entryRepo{db: db}
}

func (r *entryRepo) GetForUpdate(ctx context.Context, projectID, boardID, periodKey, subjectID string) (*leaderboards.Entry, error) {
	return r.get(ctx, projectID, boardID, periodKey, subjectID, true)
}

func (r *entryRepo) Get(ctx context.Context, projectID, boardID, periodKey, subjectID string) (*leaderboards.Entry, error) {
	return r.get(ctx, projectID, boardID, periodKey, subjectID, false)
}

func (r *entryRepo) get(ctx context.Context, projectID, boardID, periodKey, subjectID string, forUpdate bool) (*leaderboards.Entry, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_entries", "le")
	if err != nil {
		return nil, err
	}
	m := &model.LeaderboardEntry{}
	q := conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("le.project_id = ?", projectID).
		Where("le.board_id = ?", boardID).
		Where("le.period_key = ?", periodKey).
		Where("le.subject_id = ?", subjectID).
		Limit(1)
	if forUpdate {
		q = q.For("UPDATE")
	}
	err = q.Scan(ctx2)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return mapEntryToDomain(m), nil
}

func (r *entryRepo) Insert(ctx context.Context, e *leaderboards.Entry) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, e.ProjectID, "leaderboard_entries", "le")
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(mapEntryToModel(e)).ModelTableExpr(expr, sch).Exec(ctx2)
	if isLeaderboardUniqueViolation(err) {
		// 同键两个首插并发，唯一约束仲裁落败：调用方重读行锁合并。
		return leaderboards.ErrConcurrentInsert
	}
	return err
}

func (r *entryRepo) SaveMerged(ctx context.Context, e *leaderboards.Entry) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, e.ProjectID, "leaderboard_entries", "le")
	if err != nil {
		return err
	}
	// 列白名单：主键与 created_at 不可变；updated_at 仅在位置改善时推进
	//（未改善时写回原值，等值无害）。
	_, err = conn.NewUpdate().Model(mapEntryToModel(e)).ModelTableExpr(expr, sch).
		Column("value", "tiebreak_value", "submit_count", "updated_at").
		WherePK().
		Where("le.project_id = ?", e.ProjectID).
		Exec(ctx2)
	return err
}

// entryStatsRow 是 Stats 原生查询的扫描目标（列别名对齐）。
type entryStatsRow struct {
	Total int64 `bun:"total"`
	Above int64 `bun:"above"`
	Ahead int64 `bun:"ahead"`
	Below int64 `bun:"below"`
}

func (r *entryRepo) Stats(ctx context.Context, b *leaderboards.Board, periodKey string, e *leaderboards.Entry) (leaderboards.EntryStats, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, _, err := Scoped(ctx2, r.db, b.ProjectID, "leaderboard_entries", "le")
	if err != nil {
		return leaderboards.EntryStats{}, err
	}
	table, err := ProjectQuoted(b.ProjectID)
	if err != nil {
		return leaderboards.EntryStats{}, err
	}
	table += ".leaderboard_entries"

	if e == nil {
		var total int64
		q := fmt.Sprintf("SELECT count(*) FROM %s WHERE project_id = ? AND board_id = ? AND period_key = ?", table)
		if err := conn.NewRaw(q, b.ProjectID, b.ID, periodKey).Scan(ctx2, &total); err != nil {
			return leaderboards.EntryStats{}, err
		}
		return leaderboards.EntryStats{Total: total}, nil
	}

	betterOp, worseOp := ">", "<"
	if b.Sort == leaderboards.SortAsc {
		betterOp, worseOp = "<", ">"
	}
	ahead, aheadParams := aheadFilter(b, e)
	// rank-1 = 值严格优于我；below = 值严格劣于我（并列不计入两侧）；
	// ahead = 全序上领先于我（position-1）。单条 count FILTER 聚合一次扫描。
	q := fmt.Sprintf(
		"SELECT count(*) AS total, count(*) FILTER (WHERE value %s ?) AS above, "+
			"count(*) FILTER (WHERE value %s ?) AS below, count(*) FILTER (WHERE %s) AS ahead "+
			"FROM %s WHERE project_id = ? AND board_id = ? AND period_key = ?",
		betterOp, worseOp, ahead, table,
	)
	params := make([]any, 0, 2+len(aheadParams)+3)
	params = append(params, e.Value, e.Value)
	params = append(params, aheadParams...)
	params = append(params, b.ProjectID, b.ID, periodKey)

	var row entryStatsRow
	if err := conn.NewRaw(q, params...).Scan(ctx2, &row); err != nil {
		return leaderboards.EntryStats{}, err
	}
	return leaderboards.EntryStats{
		Total:    row.Total,
		Rank:     row.Above + 1,
		Position: row.Ahead + 1,
		Below:    row.Below,
	}, nil
}

// aheadFilter 构造"全序上严格领先于我"的布尔表达式，并按占位符出现顺序
// 返回参数。键栈逐层展开（前缀相等 AND 当前键更优）：value → 可选
// tiebreak_value → updated_at → subject_id（最后一层保证全序必然分出）。
// updated_at 截断到微秒对齐 PG timestamptz 精度，避免等值比较失配。
func aheadFilter(b *leaderboards.Board, e *leaderboards.Entry) (string, []any) {
	type orderKey struct {
		col   string
		desc  bool
		param any
	}
	keys := []orderKey{{"value", b.Sort == leaderboards.SortDesc, e.Value}}
	if b.HasTiebreak() && e.TiebreakValue != nil {
		keys = append(keys, orderKey{"tiebreak_value", *b.TiebreakOrder == leaderboards.SortDesc, *e.TiebreakValue})
	}
	keys = append(keys, orderKey{"updated_at", b.TieBreak == leaderboards.TieBreakLatest, e.UpdatedAt.Truncate(time.Microsecond)})
	keys = append(keys, orderKey{"subject_id", false, e.SubjectID})

	var terms []string
	var params []any
	for i := range keys {
		parts := make([]string, 0, i+1)
		for j := 0; j <= i; j++ {
			if j == i {
				op := "<"
				if keys[j].desc {
					op = ">"
				}
				parts = append(parts, fmt.Sprintf("%s %s ?", keys[j].col, op))
			} else {
				parts = append(parts, fmt.Sprintf("%s = ?", keys[j].col))
			}
			params = append(params, keys[j].param)
		}
		terms = append(terms, "("+strings.Join(parts, " AND ")+")")
	}
	return strings.Join(terms, " OR "), params
}

// leaderboardTopRow 是 ListTop 的扫描目标（rank/position 为窗口列别名）。
type leaderboardTopRow struct {
	SubjectID     string    `bun:"subject_id"`
	Value         int64     `bun:"value"`
	TiebreakValue *int64    `bun:"tiebreak_value"`
	SubmitCount   int32     `bun:"submit_count"`
	CreatedAt     time.Time `bun:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at"`
	Rank          int64     `bun:"rank_num"`
	Position      int64     `bun:"position_num"`
}

func (r *entryRepo) ListTop(ctx context.Context, b *leaderboards.Board, periodKey string, limit, offset int) ([]leaderboards.TopEntry, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, _, err := Scoped(ctx2, r.db, b.ProjectID, "leaderboard_entries", "le")
	if err != nil {
		return nil, err
	}
	table, err := ProjectQuoted(b.ProjectID)
	if err != nil {
		return nil, err
	}
	table += ".leaderboard_entries"

	full := fullOrderExpr(b)
	// RANK() = competition（并列同名次，按 value 单键）；ROW_NUMBER() =
	// position（全序位次）。两个窗口共用同一复合索引前缀。
	q := fmt.Sprintf(
		"SELECT le.subject_id, le.value, le.tiebreak_value, le.submit_count, le.created_at, le.updated_at, "+
			"RANK() OVER (ORDER BY le.value %s) AS rank_num, "+
			"ROW_NUMBER() OVER (ORDER BY %s) AS position_num "+
			"FROM %s le WHERE le.project_id = ? AND le.board_id = ? AND le.period_key = ? "+
			"ORDER BY %s LIMIT ? OFFSET ?",
		sortDirSQL(b.Sort), full, table, full,
	)
	var rows []leaderboardTopRow
	if err := conn.NewRaw(q, b.ProjectID, b.ID, periodKey, limit, offset).Scan(ctx2, &rows); err != nil {
		return nil, err
	}
	out := make([]leaderboards.TopEntry, len(rows))
	for i := range rows {
		out[i] = leaderboards.TopEntry{
			Entry: leaderboards.Entry{
				ProjectID:     b.ProjectID,
				BoardID:       b.ID,
				PeriodKey:     periodKey,
				SubjectID:     rows[i].SubjectID,
				Value:         rows[i].Value,
				TiebreakValue: rows[i].TiebreakValue,
				SubmitCount:   rows[i].SubmitCount,
				CreatedAt:     rows[i].CreatedAt,
				UpdatedAt:     rows[i].UpdatedAt,
			},
			Rank:     rows[i].Rank,
			Position: rows[i].Position,
		}
	}
	return out, nil
}

func (r *entryRepo) CountTotal(ctx context.Context, projectID, boardID, periodKey string) (int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, _, err := Scoped(ctx2, r.db, projectID, "leaderboard_entries", "le")
	if err != nil {
		return 0, err
	}
	table, err := ProjectQuoted(projectID)
	if err != nil {
		return 0, err
	}
	var total int64
	q := fmt.Sprintf("SELECT count(*) FROM %s.leaderboard_entries WHERE project_id = ? AND board_id = ? AND period_key = ?", table)
	if err := conn.NewRaw(q, projectID, boardID, periodKey).Scan(ctx2, &total); err != nil {
		return 0, err
	}
	return total, nil
}

func (r *entryRepo) CountInBoard(ctx context.Context, projectID, boardID string) (int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, _, err := Scoped(ctx2, r.db, projectID, "leaderboard_entries", "le")
	if err != nil {
		return 0, err
	}
	table, err := ProjectQuoted(projectID)
	if err != nil {
		return 0, err
	}
	var total int64
	q := fmt.Sprintf("SELECT count(*) FROM %s.leaderboard_entries WHERE project_id = ? AND board_id = ?", table)
	if err := conn.NewRaw(q, projectID, boardID).Scan(ctx2, &total); err != nil {
		return 0, err
	}
	return total, nil
}

func (r *entryRepo) Delete(ctx context.Context, projectID, boardID, periodKey, subjectID string) (bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_entries", "le")
	if err != nil {
		return false, err
	}
	res, err := conn.NewDelete().ModelTableExpr(expr, sch).
		Where("le.project_id = ?", projectID).
		Where("le.board_id = ?", boardID).
		Where("le.period_key = ?", periodKey).
		Where("le.subject_id = ?", subjectID).
		Exec(ctx2)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *entryRepo) ListPeriods(ctx context.Context, projectID, boardID string, limit int) ([]string, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, _, err := Scoped(ctx2, r.db, projectID, "leaderboard_entries", "le")
	if err != nil {
		return nil, err
	}
	table, err := ProjectQuoted(projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	q := fmt.Sprintf(
		"SELECT DISTINCT period_key FROM %s.leaderboard_entries WHERE project_id = ? AND board_id = ? ORDER BY period_key DESC LIMIT ?",
		table,
	)
	var keys []string
	if err := conn.NewRaw(q, projectID, boardID, limit).Scan(ctx2, &keys); err != nil {
		return nil, err
	}
	return keys, nil
}

func (r *entryRepo) PruneOlderThan(ctx context.Context, projectID, boardID, cutoff string) (int64, error) {
	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, sch, expr, err := Scoped(ctx2, r.db, projectID, "leaderboard_entries", "le")
	if err != nil {
		return 0, err
	}
	res, err := conn.NewDelete().ModelTableExpr(expr, sch).
		Where("le.project_id = ?", projectID).
		Where("le.board_id = ?", boardID).
		Where("le.period_key < ?", cutoff).
		Exec(ctx2)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// fullOrderExpr 返回全序 ORDER BY 表达式：value → 可选 tiebreak →
// updated_at（parallel/earliest 升序、latest 降序）→ subject_id 兜底。
// parallel 的 updated_at 只保证分页稳定，不承载语义。
func fullOrderExpr(b *leaderboards.Board) string {
	parts := []string{"le.value " + sortDirSQL(b.Sort)}
	if b.HasTiebreak() {
		parts = append(parts, "le.tiebreak_value "+sortDirSQL(*b.TiebreakOrder))
	}
	if b.TieBreak == leaderboards.TieBreakLatest {
		parts = append(parts, "le.updated_at DESC")
	} else {
		parts = append(parts, "le.updated_at ASC")
	}
	parts = append(parts, "le.subject_id ASC")
	return strings.Join(parts, ", ")
}

func sortDirSQL(d leaderboards.SortDirection) string {
	if d == leaderboards.SortAsc {
		return "ASC"
	}
	return "DESC"
}

func isLeaderboardUniqueViolation(err error) bool {
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

func mapBoardToModel(b *leaderboards.Board) *model.LeaderboardBoard {
	m := &model.LeaderboardBoard{
		ID:               b.ID,
		ProjectID:        b.ProjectID,
		Sort:             string(b.Sort),
		TieBreak:         string(b.TieBreak),
		PeriodKind:       string(b.PeriodKind),
		PeriodTZ:         b.PeriodTZ,
		Policy:           string(b.Policy),
		ValueMin:         b.ValueMin,
		ValueMax:         b.ValueMax,
		ClientSubmit:     b.ClientSubmit,
		PerSubjectLimit:  b.PerSubjectLimit,
		RetentionPeriods: b.RetentionPeriods,
		SubjectKind:      b.SubjectKind,
		CreatedAt:        b.CreatedAt,
		UpdatedAt:        b.UpdatedAt,
	}
	if b.TiebreakOrder != nil {
		v := string(*b.TiebreakOrder)
		m.TiebreakOrder = &v
	}
	return m
}

func mapBoardToDomain(m *model.LeaderboardBoard) *leaderboards.Board {
	b := &leaderboards.Board{
		ProjectID:        m.ProjectID,
		ID:               m.ID,
		Sort:             leaderboards.SortDirection(m.Sort),
		TieBreak:         leaderboards.TieBreak(m.TieBreak),
		PeriodKind:       leaderboards.PeriodKind(m.PeriodKind),
		PeriodTZ:         m.PeriodTZ,
		Policy:           leaderboards.Policy(m.Policy),
		ValueMin:         m.ValueMin,
		ValueMax:         m.ValueMax,
		ClientSubmit:     m.ClientSubmit,
		PerSubjectLimit:  m.PerSubjectLimit,
		RetentionPeriods: m.RetentionPeriods,
		SubjectKind:      m.SubjectKind,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
	if m.TiebreakOrder != nil {
		v := leaderboards.SortDirection(*m.TiebreakOrder)
		b.TiebreakOrder = &v
	}
	return b
}

func mapEntryToModel(e *leaderboards.Entry) *model.LeaderboardEntry {
	return &model.LeaderboardEntry{
		BoardID:       e.BoardID,
		PeriodKey:     e.PeriodKey,
		SubjectID:     e.SubjectID,
		ProjectID:     e.ProjectID,
		Value:         e.Value,
		TiebreakValue: e.TiebreakValue,
		SubmitCount:   e.SubmitCount,
		CreatedAt:     e.CreatedAt,
		UpdatedAt:     e.UpdatedAt,
	}
}

func mapEntryToDomain(m *model.LeaderboardEntry) *leaderboards.Entry {
	return &leaderboards.Entry{
		ProjectID:     m.ProjectID,
		BoardID:       m.BoardID,
		PeriodKey:     m.PeriodKey,
		SubjectID:     m.SubjectID,
		Value:         m.Value,
		TiebreakValue: m.TiebreakValue,
		SubmitCount:   m.SubmitCount,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}
