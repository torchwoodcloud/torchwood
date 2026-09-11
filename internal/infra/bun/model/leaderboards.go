package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// LeaderboardBoard 是榜配置行（项目数据面 leaderboard_boards）。
type LeaderboardBoard struct {
	bun.BaseModel `bun:"table:leaderboard_boards,alias:lb"`

	ID               string          `bun:"id,pk"`
	ProjectID        string          `bun:"project_id,notnull"`
	Sort             string          `bun:"sort,notnull"`
	TiebreakOrder    *string         `bun:"tiebreak_order"`
	TieBreak         string          `bun:"tie_break,notnull"`
	PeriodKind       string          `bun:"period_kind,notnull"`
	PeriodTZ         string          `bun:"period_tz,notnull"`
	Policy           string          `bun:"policy,notnull"`
	ValueMin         *int64          `bun:"value_min"`
	ValueMax         *int64          `bun:"value_max"`
	ClientSubmit     bool            `bun:"client_submit,notnull"`
	PerSubjectLimit  int32           `bun:"per_subject_limit,notnull"`
	RetentionPeriods int32           `bun:"retention_periods,notnull"`
	SubjectKind      string          `bun:"subject_kind,notnull"`
	Rewards          json.RawMessage `bun:"rewards,type:jsonb"`
	CreatedAt        time.Time       `bun:"created_at,notnull"`
	UpdatedAt        time.Time       `bun:"updated_at,notnull"`
}

// LeaderboardEntry 是排行榜条目行（项目数据面 leaderboard_entries）。
// 复合主键 (board_id, period_key, subject_id) 即模型内建去重。
type LeaderboardEntry struct {
	bun.BaseModel `bun:"table:leaderboard_entries,alias:le"`

	BoardID       string    `bun:"board_id,pk"`
	PeriodKey     string    `bun:"period_key,pk"`
	SubjectID     string    `bun:"subject_id,pk"`
	ProjectID     string    `bun:"project_id,notnull"`
	Value         int64     `bun:"value,notnull"`
	TiebreakValue *int64    `bun:"tiebreak_value"`
	SubmitCount   int32     `bun:"submit_count,notnull"`
	CreatedAt     time.Time `bun:"created_at,notnull"`
	UpdatedAt     time.Time `bun:"updated_at,notnull"`
}

// LeaderboardSettlement 是结榜发奖行（settlements，Phase 2）。
type LeaderboardSettlement struct {
	bun.BaseModel `bun:"table:leaderboard_settlements,alias:ls"`

	ID            string          `bun:"id,pk"`
	ProjectID     string          `bun:"project_id,notnull"`
	BoardID       string          `bun:"board_id,notnull"`
	PeriodKey     string          `bun:"period_key,notnull"`
	Status        string          `bun:"status,notnull"`
	SealedAt      time.Time       `bun:"sealed_at,notnull"`
	SettledAt     *time.Time      `bun:"settled_at"`
	RulesSnapshot json.RawMessage `bun:"rules_snapshot,notnull"`
	EntryCount    int64           `bun:"entry_count,notnull"`
	GrantCount    int32           `bun:"grant_count,notnull"`
	Error         *string         `bun:"error"`
	CreatedAt     time.Time       `bun:"created_at,notnull"`
	UpdatedAt     time.Time       `bun:"updated_at,notnull"`
}

// LeaderboardSettlementGrant 是发放明细行（重跑账本）。
type LeaderboardSettlementGrant struct {
	bun.BaseModel `bun:"table:leaderboard_settlement_grants,alias:lsg"`

	ID             string    `bun:"id,pk"`
	ProjectID      string    `bun:"project_id,notnull"`
	SettlementID   string    `bun:"settlement_id,notnull"`
	RuleIndex      int32     `bun:"rule_index,notnull"`
	SubjectID      string    `bun:"subject_id,notnull"`
	AssetCode      string    `bun:"asset_code,notnull"`
	Amount         int64     `bun:"amount,notnull"`
	IdempotencyKey string    `bun:"idempotency_key,notnull"`
	Status         string    `bun:"status,notnull"`
	Error          *string   `bun:"error"`
	CreatedAt      time.Time `bun:"created_at,notnull"`
	UpdatedAt      time.Time `bun:"updated_at,notnull"`
}
