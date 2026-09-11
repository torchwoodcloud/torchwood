package model

import (
	"time"

	"github.com/uptrace/bun"
)

// LeaderboardBoard 是榜配置行（项目数据面 leaderboard_boards）。
type LeaderboardBoard struct {
	bun.BaseModel `bun:"table:leaderboard_boards,alias:lb"`

	ID               string    `bun:"id,pk"`
	ProjectID        string    `bun:"project_id,notnull"`
	Sort             string    `bun:"sort,notnull"`
	TiebreakOrder    *string   `bun:"tiebreak_order"`
	TieBreak         string    `bun:"tie_break,notnull"`
	PeriodKind       string    `bun:"period_kind,notnull"`
	PeriodTZ         string    `bun:"period_tz,notnull"`
	Policy           string    `bun:"policy,notnull"`
	ValueMin         *int64    `bun:"value_min"`
	ValueMax         *int64    `bun:"value_max"`
	ClientSubmit     bool      `bun:"client_submit,notnull"`
	PerSubjectLimit  int32     `bun:"per_subject_limit,notnull"`
	RetentionPeriods int32     `bun:"retention_periods,notnull"`
	SubjectKind      string    `bun:"subject_kind,notnull"`
	CreatedAt        time.Time `bun:"created_at,notnull"`
	UpdatedAt        time.Time `bun:"updated_at,notnull"`
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
