package leaderboards

import (
	"context"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
)

// CreateBoard 创建榜配置（console 面）。缺省值在此收敛：
// per_subject_limit 未填 = 100，sort 未填 = desc，policy 未填 = best，
// tie_break 未填 = parallel，retention 未填 = 0（永久保留）。
func (a *Leaderboards) CreateBoard(ctx context.Context, in *domainleaderboards.Board) (*domainleaderboards.Board, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	in.ProjectID = projectID
	if in.Sort == "" {
		in.Sort = domainleaderboards.SortDesc
	}
	if in.TieBreak == "" {
		in.TieBreak = domainleaderboards.TieBreakParallel
	}
	if in.Policy == "" {
		in.Policy = domainleaderboards.PolicyBest
	}
	if in.PeriodKind == "" {
		in.PeriodKind = domainleaderboards.PeriodNone
	}
	if in.PerSubjectLimit == 0 {
		in.PerSubjectLimit = domainleaderboards.DefaultPerSubjectLimit
	}
	if in.SubjectKind == "" {
		in.SubjectKind = "user"
	}
	if err := domainleaderboards.ValidateBoard(in); err != nil {
		return nil, mapLeaderboardError(err)
	}
	now := a.ts()
	in.CreatedAt = now
	in.UpdatedAt = now
	if err := a.boards.Insert(ctx, in); err != nil {
		return nil, mapLeaderboardError(err)
	}
	return in, nil
}

// GetBoard 读取榜配置（console 面）。
func (a *Leaderboards) GetBoard(ctx context.Context, boardID string) (*domainleaderboards.Board, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	b, err := a.loadBoard(ctx, projectID, boardID)
	if err != nil {
		return nil, mapLeaderboardError(err)
	}
	return b, nil
}

// ListBoards 列出项目内全部榜（console 面；每项目 boards 上限 100，整列
// 返回不分页）。
func (a *Leaderboards) ListBoards(ctx context.Context) ([]domainleaderboards.Board, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	return a.boards.List(ctx, projectID)
}

// UpdateBoardCommand 用指针表达可选字段（未设置 = 不修改）；显式清空走
// Clear* 布尔。period/sort/tiebreak 声明/policy/tie_break 在榜内已有条目
// 时不可改（封榜前才允许调整排序语义）。
type UpdateBoardCommand struct {
	BoardID          string
	Sort             *domainleaderboards.SortDirection
	TiebreakOrder    *domainleaderboards.SortDirection // 与 ClearTiebreak 互斥
	ClearTiebreak    bool
	TieBreak         *domainleaderboards.TieBreak
	PeriodKind       *domainleaderboards.PeriodKind
	PeriodTZ         *string
	Policy           *domainleaderboards.Policy
	ValueMin         *int64
	ValueMax         *int64
	ClearValueBounds bool
	ClientSubmit     *bool
	PerSubjectLimit  *int32
	RetentionPeriods *int32
	SubjectKind      *string
}

func (a *Leaderboards) UpdateBoard(ctx context.Context, cmd UpdateBoardCommand) (*domainleaderboards.Board, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	b, err := a.loadBoard(ctx, projectID, cmd.BoardID)
	if err != nil {
		return nil, mapLeaderboardError(err)
	}

	tiebreakDeclChanged := cmd.ClearTiebreak || cmd.TiebreakOrder != nil
	sortChanged := cmd.Sort != nil && *cmd.Sort != b.Sort
	tieBreakChanged := cmd.TieBreak != nil && *cmd.TieBreak != b.TieBreak
	kindChanged := cmd.PeriodKind != nil && *cmd.PeriodKind != b.PeriodKind
	tzChanged := cmd.PeriodTZ != nil && *cmd.PeriodTZ != b.PeriodTZ
	policyChanged := cmd.Policy != nil && *cmd.Policy != b.Policy

	if sortChanged || tiebreakDeclChanged || tieBreakChanged || kindChanged || tzChanged || policyChanged {
		count, err := a.entries.CountInBoard(ctx, projectID, cmd.BoardID)
		if err != nil {
			return nil, err
		}
		if count > 0 {
			return nil, mapLeaderboardError(domainleaderboards.ErrImmutableField)
		}
	}

	if cmd.Sort != nil {
		b.Sort = *cmd.Sort
	}
	if cmd.ClearTiebreak {
		b.TiebreakOrder = nil
	} else if cmd.TiebreakOrder != nil {
		b.TiebreakOrder = cmd.TiebreakOrder
	}
	if cmd.TieBreak != nil {
		b.TieBreak = *cmd.TieBreak
	}
	if cmd.PeriodKind != nil {
		b.PeriodKind = *cmd.PeriodKind
	}
	if cmd.PeriodTZ != nil {
		b.PeriodTZ = *cmd.PeriodTZ
	}
	if cmd.Policy != nil {
		b.Policy = *cmd.Policy
	}
	if cmd.ClearValueBounds {
		b.ValueMin = nil
		b.ValueMax = nil
	}
	if cmd.ValueMin != nil {
		b.ValueMin = cmd.ValueMin
	}
	if cmd.ValueMax != nil {
		b.ValueMax = cmd.ValueMax
	}
	if cmd.ClientSubmit != nil {
		b.ClientSubmit = *cmd.ClientSubmit
	}
	if cmd.PerSubjectLimit != nil {
		b.PerSubjectLimit = *cmd.PerSubjectLimit
	}
	if cmd.RetentionPeriods != nil {
		b.RetentionPeriods = *cmd.RetentionPeriods
	}
	if cmd.SubjectKind != nil {
		b.SubjectKind = *cmd.SubjectKind
	}

	if err := domainleaderboards.ValidateBoard(b); err != nil {
		return nil, mapLeaderboardError(err)
	}
	b.UpdatedAt = a.ts()
	if err := a.boards.Update(ctx, b); err != nil {
		return nil, mapLeaderboardError(err)
	}
	return b, nil
}

// DeleteBoard 删除榜（条目经 FK 级联删除；console 二次确认）。
func (a *Leaderboards) DeleteBoard(ctx context.Context, boardID string) error {
	projectID, err := projectScope(ctx)
	if err != nil {
		return err
	}
	if _, err := a.loadBoard(ctx, projectID, boardID); err != nil {
		return mapLeaderboardError(err)
	}
	if err := a.boards.Delete(ctx, projectID, boardID); err != nil {
		return mapLeaderboardError(err)
	}
	return nil
}
