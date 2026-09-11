package leaderboards

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxRewardRules 是单榜奖励规则数上限。
const MaxRewardRules = 20

// RewardRule 是声明式奖励规则：名次区间（含端点）与 value 门槛两种判据
// 可叠加；多规则独立评估、可叠加命中。结算边界跟随 tie_break：
// parallel → rank 含端点（并列第 rank_max 也发）；earliest/latest →
// position 截断（先达到者占位）。
type RewardRule struct {
	RankMin   *int32 `json:"rank_min,omitempty"`
	RankMax   *int32 `json:"rank_max,omitempty"`
	ValueMin  *int64 `json:"value_min,omitempty"`
	AssetCode string `json:"asset_code"`
	Amount    int64  `json:"amount"`
}

// ValidateRewardRules 校验规则集（board 配置路径）。
func ValidateRewardRules(kind PeriodKind, rules []RewardRule) error {
	if len(rules) == 0 {
		return nil
	}
	if kind == PeriodNone {
		return fmt.Errorf("%w: rewards require a period kind (none/all-time boards never settle)", ErrInvalidConfig)
	}
	if len(rules) > MaxRewardRules {
		return fmt.Errorf("%w: at most %d reward rules", ErrInvalidConfig, MaxRewardRules)
	}
	for i, r := range rules {
		if r.AssetCode == "" {
			return fmt.Errorf("%w: rewards[%d].asset_code is required", ErrInvalidConfig, i)
		}
		if r.Amount <= 0 {
			return fmt.Errorf("%w: rewards[%d].amount must be > 0", ErrInvalidConfig, i)
		}
		if r.RankMin == nil && r.RankMax == nil && r.ValueMin == nil {
			return fmt.Errorf("%w: rewards[%d] needs at least one of rank range / value_min", ErrInvalidConfig, i)
		}
		if r.RankMin != nil && *r.RankMin < 1 {
			return fmt.Errorf("%w: rewards[%d].rank_min must be >= 1", ErrInvalidConfig, i)
		}
		if r.RankMin != nil && r.RankMax != nil && *r.RankMin > *r.RankMax {
			return fmt.Errorf("%w: rewards[%d].rank_min must be <= rank_max", ErrInvalidConfig, i)
		}
		if r.RankMax != nil && *r.RankMax < 1 {
			return fmt.Errorf("%w: rewards[%d].rank_max must be >= 1", ErrInvalidConfig, i)
		}
	}
	return nil
}

// MarshalRewardRules 序列化规则快照（settlements.rules_snapshot）。
func MarshalRewardRules(rules []RewardRule) json.RawMessage {
	if len(rules) == 0 {
		return json.RawMessage("[]")
	}
	b, err := json.Marshal(rules)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

// UnmarshalRewardRules 反序列化规则快照。
func UnmarshalRewardRules(raw json.RawMessage) ([]RewardRule, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var rules []RewardRule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// 结算状态机：open（无行）→ settling（认领）→ settled / error（可重跑）；
// 旁路 voided（运营弃奖，仅未 settled 可 void）。
const (
	SettlementStatusSettling = "settling"
	SettlementStatusSettled  = "settled"
	SettlementStatusError    = "error"
	SettlementStatusVoided   = "voided"
)

// Settlement 是结榜发奖记录（期粒度；不随条目 retention 清理）。
type Settlement struct {
	ProjectID     string
	ID            string
	BoardID       string
	PeriodKey     string
	Status        string
	SealedAt      time.Time
	SettledAt     *time.Time
	RulesSnapshot json.RawMessage
	EntryCount    int64
	GrantCount    int32
	Error         string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// SettlementGrant 是发放明细（重跑账本：已发行按幂等键重放不重复发）。
type SettlementGrant struct {
	ProjectID      string
	ID             string
	SettlementID   string
	RuleIndex      int32
	SubjectID      string
	AssetCode      string
	Amount         int64
	IdempotencyKey string
	Status         string
	Error          string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

var (
	ErrSettlementNotFound       = errors.New("leaderboards: settlement not found")
	ErrSettlementVoided         = errors.New("leaderboards: settlement is voided")
	ErrSettlementAlreadySettled = errors.New("leaderboards: settlement already settled (revoke via manual consume)")
)

// RewardGranter 是发奖端口（适配 Assets Grant，幂等键语义由实现保证——
// 同键重放返回首次结果，不重复发放）。
type RewardGranter interface {
	GrantReward(ctx context.Context, projectID, subjectID, assetCode string, amount int64, idempotencyKey string) error
}

// SettlementRepo 持久化结算与发放明细。写方法在调用方 uow.Run 内。
type SettlementRepo interface {
	// ClaimPending 原子认领：无行则插入 settling 行（唯一键仲裁），
	// 返回 claimed=false 表示该期已有结算行（settled/error/voided 等）。
	ClaimPending(ctx context.Context, s *Settlement) (claimed bool, err error)
	Get(ctx context.Context, projectID, boardID, periodKey string) (*Settlement, error)
	ListByBoard(ctx context.Context, projectID, boardID string, limit int) ([]Settlement, error)
	ListGrants(ctx context.Context, projectID, settlementID string) ([]SettlementGrant, error)
	// SaveGrants 逐行 upsert（同 (settlement, rule, subject) 更新状态与错误）。
	SaveGrants(ctx context.Context, grants []*SettlementGrant) error
	// Complete 落 settled（settling/error → settled）。
	Complete(ctx context.Context, projectID, id string, settledAt time.Time, grantCount int32) error
	// Fail 落 error（结算过程失败，可重跑）。
	Fail(ctx context.Context, projectID, id string, msg string) error
	// Void 弃奖：settling/error → voided；settled 拒绝（已发放只能手工追回）。
	Void(ctx context.Context, projectID, id string) error
	// ResetToPending error/settled → settling（重跑入口）。
	ResetToPending(ctx context.Context, projectID, id string) error
}
