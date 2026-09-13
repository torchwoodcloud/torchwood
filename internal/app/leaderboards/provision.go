package leaderboards

import (
	"context"
	"errors"
	"fmt"
	"strings"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// applyCreateDefaults 收敛建榜缺省值（console 与 server provisioning 共用；
// 幂等比较集也按缺省归一后的形态计算）。
func applyCreateDefaults(in *domainleaderboards.Board) {
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
}

// enforceBoardCap 把守每项目榜配置数量上限（文档承诺值的机械执行点；
// server 面脚本化批量建榜使约定必须变成校验）。
func (a *Leaderboards) enforceBoardCap(ctx context.Context, projectID string) error {
	count, err := a.boards.Count(ctx, projectID)
	if err != nil {
		return err
	}
	if count >= domainleaderboards.MaxBoardsPerProject {
		return domainleaderboards.ErrBoardLimit
	}
	return nil
}

// boardConfigDiff 比较 want（请求，缺省归一后）与 got（存量榜）在 server 面
// Create 可见字段集上的差异。rewards / created_at / updated_at / project 不在
// 比较集：server Create 消息不含 rewards，console 侧的 rewards 编辑与时间戳
// 不影响预置脚本重放。返回空切片 = 相等（"相等即 200"的相等定义）。
func boardConfigDiff(want, got *domainleaderboards.Board) []string {
	var diffs []string
	appendDiff := func(format string, args ...any) {
		diffs = append(diffs, fmt.Sprintf(format, args...))
	}
	if want.Sort != got.Sort {
		appendDiff("sort: requested %s, existing %s", want.Sort, got.Sort)
	}
	switch {
	case want.TiebreakOrder == nil && got.TiebreakOrder == nil:
	case want.TiebreakOrder == nil:
		appendDiff("tiebreak_order: requested <unset>, existing %s", *got.TiebreakOrder)
	case got.TiebreakOrder == nil:
		appendDiff("tiebreak_order: requested %s, existing <unset>", *want.TiebreakOrder)
	case *want.TiebreakOrder != *got.TiebreakOrder:
		appendDiff("tiebreak_order: requested %s, existing %s", *want.TiebreakOrder, *got.TiebreakOrder)
	}
	if want.TieBreak != got.TieBreak {
		appendDiff("tie_break: requested %s, existing %s", want.TieBreak, got.TieBreak)
	}
	if want.PeriodKind != got.PeriodKind {
		appendDiff("period_kind: requested %s, existing %s", want.PeriodKind, got.PeriodKind)
	}
	if want.PeriodTZ != got.PeriodTZ {
		appendDiff("period_tz: requested %s, existing %s", want.PeriodTZ, got.PeriodTZ)
	}
	if want.Policy != got.Policy {
		appendDiff("policy: requested %s, existing %s", want.Policy, got.Policy)
	}
	diffBound := func(name string, w, g *int64) {
		switch {
		case w == nil && g == nil:
		case w == nil:
			appendDiff("%s: requested <unset>, existing %d", name, *g)
		case g == nil:
			appendDiff("%s: requested %d, existing <unset>", name, *w)
		case *w != *g:
			appendDiff("%s: requested %d, existing %d", name, *w, *g)
		}
	}
	diffBound("value_min", want.ValueMin, got.ValueMin)
	diffBound("value_max", want.ValueMax, got.ValueMax)
	if want.ClientSubmit != got.ClientSubmit {
		appendDiff("client_submit: requested %t, existing %t", want.ClientSubmit, got.ClientSubmit)
	}
	if want.PerSubjectLimit != got.PerSubjectLimit {
		appendDiff("per_subject_submit_limit: requested %d, existing %d", want.PerSubjectLimit, got.PerSubjectLimit)
	}
	if want.RetentionPeriods != got.RetentionPeriods {
		appendDiff("retention_periods: requested %d, existing %d", want.RetentionPeriods, got.RetentionPeriods)
	}
	if want.SubjectKind != got.SubjectKind {
		appendDiff("subject_kind: requested %s, existing %s", want.SubjectKind, got.SubjectKind)
	}
	return diffs
}

// resolveExisting 是"相等即 200"裁决：存量榜与请求在比较集上逐字段相等 →
// 返回现状（重放 200）；不等 → ALREADY_EXISTS 附字段 diff（既是错误也是
// 漂移信号：console 或脚本任一侧改动 desired 字段都会被对端发现）。
func resolveExisting(boardID string, want, existing *domainleaderboards.Board) (*domainleaderboards.Board, error) {
	if diff := boardConfigDiff(want, existing); len(diff) > 0 {
		return nil, status.Errorf(codes.AlreadyExists,
			"leaderboards: board %q already exists with different config: %s", boardID, strings.Join(diff, "; "))
	}
	return existing, nil
}

// CreateBoardProvisioning 是 provisioning 语义的幂等创建（server 面预置
// 管线专用；console 的 CreateBoard 保持纯 ALREADY_EXISTS 语义不变）：
//   - 榜不存在 → 校验 + 上限把守 + 插入；
//   - 已存在且配置逐字段相等（缺省归一后）→ 返回现状（重放安全）；
//   - 已存在但不等 → ALREADY_EXISTS（附字段 diff）。
//
// 并发同配置首建：唯一约束落败方重读并走相等比较，自然收敛为 200。
// rewards 恒为空（server 消息不含该字段），无需资产校验。
func (a *Leaderboards) CreateBoardProvisioning(ctx context.Context, in *domainleaderboards.Board) (*domainleaderboards.Board, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	in.ProjectID = projectID
	applyCreateDefaults(in)
	if err := domainleaderboards.ValidateBoard(in); err != nil {
		return nil, mapLeaderboardError(err)
	}
	// 重放路径先于上限判定：满员项目重放既有榜必须继续成功。
	if existing, err := a.boards.Get(ctx, projectID, in.ID); err != nil {
		return nil, err
	} else if existing != nil {
		return resolveExisting(in.ID, in, existing)
	}
	if err := a.enforceBoardCap(ctx, projectID); err != nil {
		return nil, mapLeaderboardError(err)
	}
	now := a.ts()
	in.CreatedAt = now
	in.UpdatedAt = now
	if err := a.boards.Insert(ctx, in); err != nil {
		if !errors.Is(err, domainleaderboards.ErrBoardExists) {
			return nil, mapLeaderboardError(err)
		}
		// 首建并发竞争：重读走相等比较（同配置 → 重放；异配置 → 漂移错误）。
		existing, gerr := a.loadBoard(ctx, projectID, in.ID)
		if gerr != nil {
			return nil, mapLeaderboardError(gerr)
		}
		return resolveExisting(in.ID, in, existing)
	}
	return in, nil
}
