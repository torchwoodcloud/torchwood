// Package leaderboards 是排行榜领域（dogfooding 提案 2026-09-11，Phase 1）：
// 榜配置、期派生、tie-break 全序与重复提交合并语义。
package leaderboards

import (
	"fmt"
	"regexp"
	"time"
)

// SortDirection 是主值排序方向。
type SortDirection string

const (
	SortDesc SortDirection = "desc"
	SortAsc  SortDirection = "asc"
)

// TieBreak 是并列裁决模式：parallel=并列等同（奖励边界按 rank 含端点）；
// earliest/latest=先/后达到者靠前（updated_at 进排序键）。
type TieBreak string

const (
	TieBreakParallel TieBreak = "parallel"
	TieBreakEarliest TieBreak = "earliest"
	TieBreakLatest   TieBreak = "latest"
)

// PeriodKind 是期类型；period_key 由服务端按 board 时区派生（纯函数，
// season / 自定义 interval 是 v2 接缝，滚动窗口是非目标）。
type PeriodKind string

const (
	PeriodNone    PeriodKind = "none"
	PeriodDaily   PeriodKind = "daily"
	PeriodWeekly  PeriodKind = "weekly"
	PeriodMonthly PeriodKind = "monthly"
)

// Policy 是同一 (board, period, subject) 重复提交的合并规则；
// best 的方向跟随 board.Sort（desc→取大，asc→取小）。
type Policy string

const (
	PolicyBest   Policy = "best"
	PolicyLatest Policy = "latest"
	PolicySum    Policy = "sum"
)

const (
	// MaxBoardIDLen 与 collection ID 规则对齐：^[a-z_][a-z0-9_]*$ ≤ 40。
	MaxBoardIDLen = 40
	// MaxSubjectIDLen 是 subject_id 上限（不透明字符串，v1 语义上多为 uid）。
	MaxSubjectIDLen = 64
	// MaxValueAbs 是 value 硬上限（JSON 安全整数界）。
	MaxValueAbs = int64(1)<<53 - 1
	// MaxPerSubjectLimit / DefaultPerSubjectLimit 是每期每主体提交次数上限。
	MaxPerSubjectLimit     = 10000
	DefaultPerSubjectLimit = 100
	maxSubjectKindLen      = 32
)

var boardIDPattern = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Board 是榜配置（project 内配置对象，console CRUD）。
type Board struct {
	ProjectID string
	ID        string
	Sort      SortDirection
	// TiebreakOrder 声明单列 tiebreak 及其方向；nil = 未声明（submit 不得携带
	// tiebreak_value）。与 sort/policy 同为"有条目后不可改"字段。
	TiebreakOrder   *SortDirection
	TieBreak        TieBreak
	PeriodKind      PeriodKind
	PeriodTZ        string
	Policy          Policy
	ValueMin        *int64
	ValueMax        *int64
	ClientSubmit    bool
	PerSubjectLimit int32
	// RetentionPeriods 保留最近多少期（0 = 永久，默认不删用户数据）。
	RetentionPeriods int32
	// SubjectKind 纯展示提示（console 决定是否链到用户详情），无语义。
	SubjectKind string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// HasTiebreak 报告榜是否声明了 tiebreak 列。
func (b *Board) HasTiebreak() bool { return b.TiebreakOrder != nil }

// Location 解析 board 时区；none 期或空 tz 回落 UTC。
func (b *Board) Location() *time.Location {
	if b.PeriodKind == PeriodNone || b.PeriodTZ == "" {
		return time.UTC
	}
	loc, err := time.LoadLocation(b.PeriodTZ)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ValidateBoard 校验榜配置的全部不变量（console 写入路径调用）。
func ValidateBoard(b *Board) error {
	if b == nil {
		return fmt.Errorf("%w: board is nil", ErrInvalidConfig)
	}
	if len(b.ID) == 0 || len(b.ID) > MaxBoardIDLen || !boardIDPattern.MatchString(b.ID) {
		return fmt.Errorf("%w: id must match ^[a-z_][a-z0-9_]*$ (max %d)", ErrInvalidConfig, MaxBoardIDLen)
	}
	switch b.Sort {
	case SortDesc, SortAsc:
	default:
		return fmt.Errorf("%w: sort must be asc or desc", ErrInvalidConfig)
	}
	if b.TiebreakOrder != nil {
		switch *b.TiebreakOrder {
		case SortDesc, SortAsc:
		default:
			return fmt.Errorf("%w: tiebreak order must be asc or desc", ErrInvalidConfig)
		}
	}
	switch b.TieBreak {
	case TieBreakParallel, TieBreakEarliest, TieBreakLatest:
	default:
		return fmt.Errorf("%w: tie_break must be parallel, earliest or latest", ErrInvalidConfig)
	}
	switch b.PeriodKind {
	case PeriodNone, PeriodDaily, PeriodWeekly, PeriodMonthly:
	default:
		return fmt.Errorf("%w: period kind must be none, daily, weekly or monthly", ErrInvalidConfig)
	}
	switch b.Policy {
	case PolicyBest, PolicyLatest, PolicySum:
	default:
		return fmt.Errorf("%w: policy must be best, latest or sum", ErrInvalidConfig)
	}
	if b.PeriodKind != PeriodNone {
		if _, err := time.LoadLocation(b.PeriodTZ); err != nil {
			return fmt.Errorf("%w: period_tz must be a valid IANA time zone", ErrInvalidConfig)
		}
	}
	if err := validateBounds(b.ValueMin, b.ValueMax); err != nil {
		return err
	}
	if b.PerSubjectLimit < 1 || b.PerSubjectLimit > MaxPerSubjectLimit {
		return fmt.Errorf("%w: per_subject_limit must be 1..%d", ErrInvalidConfig, MaxPerSubjectLimit)
	}
	if b.RetentionPeriods < 0 {
		return fmt.Errorf("%w: retention_periods must be >= 0", ErrInvalidConfig)
	}
	if len(b.SubjectKind) > maxSubjectKindLen {
		return fmt.Errorf("%w: subject_kind exceeds %d characters", ErrInvalidConfig, maxSubjectKindLen)
	}
	return nil
}

func validateBounds(minV, maxV *int64) error {
	for _, v := range []*int64{minV, maxV} {
		if v != nil && (*v > MaxValueAbs || *v < -MaxValueAbs) {
			return fmt.Errorf("%w: bound exceeds |%d|", ErrInvalidConfig, MaxValueAbs)
		}
	}
	if minV != nil && maxV != nil && *minV > *maxV {
		return fmt.Errorf("%w: value_min must be <= value_max", ErrInvalidConfig)
	}
	return nil
}

// ValidateValue 校验提交值：硬上限 + 声明式区间（越界拒收，不做 clamp）。
func (b *Board) ValidateValue(v int64) error {
	if v > MaxValueAbs || v < -MaxValueAbs {
		return ErrValueTooLarge
	}
	if b.ValueMin != nil && v < *b.ValueMin {
		return ErrValueOutOfRange
	}
	if b.ValueMax != nil && v > *b.ValueMax {
		return ErrValueOutOfRange
	}
	return nil
}
