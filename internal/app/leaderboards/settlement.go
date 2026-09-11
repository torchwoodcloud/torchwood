package leaderboards

import (
	"context"
	"fmt"
	"time"

	domainleaderboards "github.com/torchwoodcloud/torchwood/internal/domain/leaderboards"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// settleScanLimit 是单轮单榜认领的期数预算（状态扫描非定时投放，
// 停机积压靠后续轮次自然补算）。
const settleScanLimit = 50

// winnerLimit 是单规则获奖者的硬上限（护栏，正常远达不到）。
const winnerLimit = 100000

// SettleDue 是结榜发奖扫描入口（worker 低频 ticker 驱动）：active 项目 ×
// 配置了 rewards 的榜 × 已封榜且无结算行的期 → 逐期结算。
// 单期失败不阻断其他期（记 error 状态，重跑入口兜底）。
func (a *Leaderboards) SettleDue(ctx context.Context) (int, error) {
	if a.projects == nil || a.settlements == nil {
		return 0, nil
	}
	all, err := a.projects.ListProjects(ctx)
	if err != nil {
		return 0, err
	}
	now := a.ts()
	settled := 0
	for i := range all {
		if all[i].Status != "active" {
			continue
		}
		boards, err := a.boards.List(ctx, all[i].ID)
		if err != nil {
			a.logger.Warn("leaderboards settle: list boards failed", "project_id", all[i].ID, "error", err)
			continue
		}
		for j := range boards {
			b := &boards[j]
			if len(b.Rewards) == 0 || b.PeriodKind == domainleaderboards.PeriodNone {
				continue
			}
			previous := domainleaderboards.PreviousPeriodKey(b.PeriodKind, b.Location(), now)
			periods, err := a.entries.ListSettlablePeriods(ctx, all[i].ID, b.ID, previous, settleScanLimit)
			if err != nil {
				a.logger.Warn("leaderboards settle: list periods failed", "project_id", all[i].ID, "board_id", b.ID, "error", err)
				continue
			}
			for _, periodKey := range periods {
				ok, err := a.settleOne(ctx, b, periodKey, now)
				if err != nil {
					a.logger.Error("leaderboards settle failed", "project_id", all[i].ID, "board_id", b.ID, "period", periodKey, "error", err)
					continue
				}
				if ok {
					settled++
				}
			}
		}
	}
	return settled, nil
}

// settleOne 结算单期：认领（唯一键仲裁）→ 规则快照 → 逐规则算获奖者 →
// 逐 (rule, subject) 幂等发放 → 落 settled。单笔发放失败不阻断整期：
// 该笔记 failed（重跑只补非 granted 的），期照常 settled——一个坏 subject
// 不应卡住整期奖励（幂等键保证重跑不双发）。
func (a *Leaderboards) settleOne(ctx context.Context, b *domainleaderboards.Board, periodKey string, now time.Time) (bool, error) {
	if a.granter == nil {
		return false, status.Error(codes.FailedPrecondition, "leaderboards: reward granter not configured")
	}
	total, err := a.entries.CountTotal(ctx, b.ProjectID, b.ID, periodKey)
	if err != nil {
		return false, err
	}
	s := &domainleaderboards.Settlement{
		ProjectID:     b.ProjectID,
		ID:            idgen.ULID().String(),
		BoardID:       b.ID,
		PeriodKey:     periodKey,
		Status:        domainleaderboards.SettlementStatusSettling,
		SealedAt:      now,
		RulesSnapshot: domainleaderboards.MarshalRewardRules(b.Rewards),
		EntryCount:    total,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	claimed, err := a.settlements.ClaimPending(ctx, s)
	if err != nil {
		return false, err
	}
	if !claimed {
		return false, nil
	}

	grants, err := a.planGrants(ctx, b, periodKey, s.ID)
	if err != nil {
		_ = a.settlements.Fail(ctx, b.ProjectID, s.ID, err.Error())
		return false, err
	}
	if err := a.settlements.SaveGrants(ctx, grants); err != nil {
		_ = a.settlements.Fail(ctx, b.ProjectID, s.ID, err.Error())
		return false, err
	}

	granted := int32(0)
	for _, g := range grants {
		now2 := a.ts()
		if gerr := a.granter.GrantReward(ctx, g.ProjectID, g.SubjectID, g.AssetCode, g.Amount, g.IdempotencyKey); gerr != nil {
			g.Status = "failed"
			g.Error = gerr.Error()
			g.UpdatedAt = now2
			continue
		}
		g.Status = "granted"
		g.Error = ""
		g.UpdatedAt = now2
		granted++
	}
	// 状态回写（含失败明细，重跑只补非 granted）。
	if err := a.settlements.SaveGrants(ctx, grants); err != nil {
		_ = a.settlements.Fail(ctx, b.ProjectID, s.ID, err.Error())
		return false, err
	}
	if err := a.settlements.Complete(ctx, b.ProjectID, s.ID, a.ts(), granted); err != nil {
		return false, err
	}
	a.logger.Info("leaderboard period settled",
		"project_id", b.ProjectID, "board_id", b.ID, "period", periodKey,
		"entries", total, "granted", granted, "failed", int32(len(grants))-granted)
	return true, nil
}

// planGrants 展开规则 → 获奖者 → 发放明细（幂等键派生
// lbsettle:{board}:{period}:{rule_index}:{subject}——重跑/崩溃续跑天然去重）。
func (a *Leaderboards) planGrants(ctx context.Context, b *domainleaderboards.Board, periodKey, settlementID string) ([]*domainleaderboards.SettlementGrant, error) {
	var grants []*domainleaderboards.SettlementGrant
	for i, rule := range b.Rewards {
		winners, err := a.entries.ListRewardWinners(ctx, b, periodKey, rule, winnerLimit)
		if err != nil {
			return nil, fmt.Errorf("rule %d winners: %w", i, err)
		}
		now := a.ts()
		for _, w := range winners {
			grants = append(grants, &domainleaderboards.SettlementGrant{
				ProjectID:      b.ProjectID,
				ID:             idgen.ULID().String(),
				SettlementID:   settlementID,
				RuleIndex:      int32(i),
				SubjectID:      w.SubjectID,
				AssetCode:      rule.AssetCode,
				Amount:         rule.Amount,
				IdempotencyKey: fmt.Sprintf("lbsettle:%s:%s:%d:%s", b.ID, periodKey, i, w.SubjectID),
				Status:         "pending",
				CreatedAt:      now,
				UpdatedAt:      now,
			})
		}
	}
	return grants, nil
}

// RerunSettlement 重跑结算（console）：settled/error → settling，只补发
// 非 granted 的明细（已发放的靠幂等键重放也不会双发，跳过是省额度）；
// 全部已发放时为幂等空操作。voided 拒绝（弃奖不可复活）。
func (a *Leaderboards) RerunSettlement(ctx context.Context, boardID, periodKey string) error {
	projectID, err := projectScope(ctx)
	if err != nil {
		return err
	}
	if _, err := a.loadBoard(ctx, projectID, boardID); err != nil {
		return mapLeaderboardError(err)
	}
	s, err := a.settlements.Get(ctx, projectID, boardID, periodKey)
	if err != nil {
		return err
	}
	if s == nil {
		return mapLeaderboardError(domainleaderboards.ErrSettlementNotFound)
	}
	if s.Status == domainleaderboards.SettlementStatusVoided {
		return mapLeaderboardError(domainleaderboards.ErrSettlementVoided)
	}
	if err := a.settlements.ResetToPending(ctx, projectID, s.ID); err != nil {
		return err
	}
	grants, err := a.settlements.ListGrants(ctx, projectID, s.ID)
	if err != nil {
		return err
	}
	granted := int32(0)
	for i := range grants {
		g := &grants[i]
		if g.Status == "granted" {
			granted++
			continue
		}
		if gerr := a.granter.GrantReward(ctx, g.ProjectID, g.SubjectID, g.AssetCode, g.Amount, g.IdempotencyKey); gerr != nil {
			g.Status = "failed"
			g.Error = gerr.Error()
			g.UpdatedAt = a.ts()
			continue
		}
		g.Status = "granted"
		g.Error = ""
		g.UpdatedAt = a.ts()
		granted++
	}
	if err := a.settlements.SaveGrants(ctx, filterGrantsForSave(grants)); err != nil {
		return err
	}
	return a.settlements.Complete(ctx, projectID, s.ID, a.ts(), granted)
}

func filterGrantsForSave(grants []domainleaderboards.SettlementGrant) []*domainleaderboards.SettlementGrant {
	out := make([]*domainleaderboards.SettlementGrant, 0, len(grants))
	for i := range grants {
		out = append(out, &grants[i])
	}
	return out
}

// VoidSettlement 弃奖（console，作弊调查）：无行则直接落 voided（预弃奖）；
// settling/error → voided（未发放明细标 voided）；settled 拒绝——已发放
// 只能手工 consume 追回。
func (a *Leaderboards) VoidSettlement(ctx context.Context, boardID, periodKey string) error {
	projectID, err := projectScope(ctx)
	if err != nil {
		return err
	}
	if _, err := a.loadBoard(ctx, projectID, boardID); err != nil {
		return mapLeaderboardError(err)
	}
	s, err := a.settlements.Get(ctx, projectID, boardID, periodKey)
	if err != nil {
		return err
	}
	if s == nil {
		now := a.ts()
		pre := &domainleaderboards.Settlement{
			ProjectID:     projectID,
			ID:            idgen.ULID().String(),
			BoardID:       boardID,
			PeriodKey:     periodKey,
			Status:        domainleaderboards.SettlementStatusVoided,
			SealedAt:      now,
			RulesSnapshot: domainleaderboards.MarshalRewardRules(nil),
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		claimed, err := a.settlements.ClaimPending(ctx, pre)
		if err != nil {
			return err
		}
		if !claimed {
			// 竞态：他人已结算——重读校验。
			return a.voidExisting(ctx, projectID, boardID, periodKey)
		}
		return nil
	}
	return a.voidExisting(ctx, projectID, boardID, periodKey)
}

func (a *Leaderboards) voidExisting(ctx context.Context, projectID, boardID, periodKey string) error {
	s, err := a.settlements.Get(ctx, projectID, boardID, periodKey)
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	if s.Status == domainleaderboards.SettlementStatusSettled {
		return mapLeaderboardError(domainleaderboards.ErrSettlementAlreadySettled)
	}
	if s.Status == domainleaderboards.SettlementStatusVoided {
		return nil
	}
	if err := a.settlements.Void(ctx, projectID, s.ID); err != nil {
		return err
	}
	grants, err := a.settlements.ListGrants(ctx, projectID, s.ID)
	if err != nil {
		return err
	}
	for i := range grants {
		if grants[i].Status == "granted" {
			continue
		}
		grants[i].Status = "voided"
		grants[i].UpdatedAt = a.ts()
	}
	return a.settlements.SaveGrants(ctx, filterGrantsForSave(grants))
}

// GetSettlement 读取结算 + 明细（console / server 面）。
func (a *Leaderboards) GetSettlement(ctx context.Context, boardID, periodKey string) (*domainleaderboards.Settlement, []domainleaderboards.SettlementGrant, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if _, err := a.loadBoard(ctx, projectID, boardID); err != nil {
		return nil, nil, mapLeaderboardError(err)
	}
	s, err := a.settlements.Get(ctx, projectID, boardID, periodKey)
	if err != nil {
		return nil, nil, err
	}
	if s == nil {
		return nil, nil, mapLeaderboardError(domainleaderboards.ErrSettlementNotFound)
	}
	grants, err := a.settlements.ListGrants(ctx, projectID, s.ID)
	if err != nil {
		return nil, nil, err
	}
	return s, grants, nil
}

// ListSettlements 按榜列结算（console / server 面）。
func (a *Leaderboards) ListSettlements(ctx context.Context, boardID string, limit int) ([]domainleaderboards.Settlement, error) {
	projectID, err := projectScope(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := a.loadBoard(ctx, projectID, boardID); err != nil {
		return nil, mapLeaderboardError(err)
	}
	return a.settlements.ListByBoard(ctx, projectID, boardID, limit)
}
