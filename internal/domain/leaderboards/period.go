package leaderboards

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var (
	ErrBoardNotFound        = errors.New("leaderboards: board not found")
	ErrBoardExists          = errors.New("leaderboards: board already exists")
	ErrInvalidConfig        = errors.New("leaderboards: invalid board configuration")
	ErrSubjectRequired      = errors.New("leaderboards: subject_id is required")
	ErrSubjectTooLong       = errors.New("leaderboards: subject_id exceeds maximum length")
	ErrValueOutOfRange      = errors.New("leaderboards: value out of declared range")
	ErrValueTooLarge        = errors.New("leaderboards: value exceeds JSON-safe bound")
	ErrPeriodInvalid        = errors.New("leaderboards: period format is invalid for this board")
	ErrPeriodNotSubmittable = errors.New("leaderboards: period is not submittable (must be current or previous period; sealed periods are final)")
	ErrTiebreakMismatch     = errors.New("leaderboards: tiebreak_value presence must match board tiebreak declaration")
	ErrSubmitLimitExceeded  = errors.New("leaderboards: per-subject submit limit exceeded for this period")
	ErrClientSubmitDisabled = errors.New("leaderboards: board does not allow client submission")
	ErrEntryNotFound        = errors.New("leaderboards: entry not found")
	ErrImmutableField       = errors.New("leaderboards: board field is immutable while entries exist")
	ErrPeriodRequired       = errors.New("leaderboards: period kind requires period_tz")
	// ErrConcurrentInsert 是同键首插并发竞争（唯一约束仲裁落败）；
	// 调用方重读行锁合并，不是终态错误。
	ErrConcurrentInsert = errors.New("leaderboards: entry concurrently inserted")
)

var (
	periodDailyPattern   = regexp.MustCompile(`^\d{8}$`)
	periodWeeklyPattern  = regexp.MustCompile(`^\d{4}-W\d{2}$`)
	periodMonthlyPattern = regexp.MustCompile(`^\d{6}$`)
)

// PeriodKey 按期类型在 loc 时区派生 at 时刻的 period_key：
// daily→YYYYMMDD，weekly→ISO 周 YYYY-Www，monthly→YYYYMM，none→all。
func PeriodKey(kind PeriodKind, loc *time.Location, at time.Time) string {
	if kind == PeriodNone {
		return "all"
	}
	if loc == nil {
		loc = time.UTC
	}
	local := at.In(loc)
	switch kind {
	case PeriodDaily:
		return local.Format("20060102")
	case PeriodWeekly:
		y, w := local.ISOWeek()
		return fmt.Sprintf("%04d-W%02d", y, w)
	case PeriodMonthly:
		return local.Format("200601")
	default:
		return "all"
	}
}

// PeriodKeyMinus 返回 at 往前 n 个期的 period_key。按分量递减而不是
// AddDate 归一化（月末日 AddDate(-1 month) 会溢出到下月，如 3-31 → 2-31 → 3-03）。
func PeriodKeyMinus(kind PeriodKind, loc *time.Location, at time.Time, n int) string {
	if kind == PeriodNone || n < 0 {
		return PeriodKey(kind, loc, at)
	}
	if loc == nil {
		loc = time.UTC
	}
	local := at.In(loc)
	y, m, d := local.Date()
	h, min, s := local.Clock()
	switch kind {
	case PeriodDaily:
		return time.Date(y, m, d-n, h, min, s, local.Nanosecond(), loc).Format("20060102")
	case PeriodWeekly:
		return isoWeekKey(time.Date(y, m, d-7*n, h, min, s, local.Nanosecond(), loc))
	case PeriodMonthly:
		idx := int(m) - 1 - n
		for idx < 0 {
			idx += 12
			y--
		}
		return fmt.Sprintf("%04d%02d", y, idx+1)
	default:
		return "all"
	}
}

func isoWeekKey(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// PreviousPeriodKey 返回 at 的上一期 key（none 期返回空——没有可补传的上一期）。
func PreviousPeriodKey(kind PeriodKind, loc *time.Location, at time.Time) string {
	if kind == PeriodNone {
		return ""
	}
	return PeriodKeyMinus(kind, loc, at, 1)
}

// ValidatePeriodKey 校验显式 period 参数的格式合法性（读路径与写路径共用）。
func ValidatePeriodKey(kind PeriodKind, key string) bool {
	switch kind {
	case PeriodNone:
		return key == "all"
	case PeriodDaily:
		if !periodDailyPattern.MatchString(key) {
			return false
		}
		_, err := time.Parse("20060102", key)
		return err == nil
	case PeriodWeekly:
		if !periodWeeklyPattern.MatchString(key) {
			return false
		}
		week, err := strconv.Atoi(key[6:])
		return err == nil && week >= 1 && week <= 53
	case PeriodMonthly:
		if !periodMonthlyPattern.MatchString(key) {
			return false
		}
		month, err := strconv.Atoi(key[4:])
		return err == nil && month >= 1 && month <= 12
	default:
		return false
	}
}

// ResolveSubmittablePeriod 解析写路径的 period 参数：缺省 = 当前期；
// 显式时必须命中 {当前期, 上一期}（离线补传宽限），其余（未来 / 已封榜期）
// 拒收——封榜即终局，对所有人（含 server 面）一致。
func ResolveSubmittablePeriod(b *Board, explicit string, now time.Time) (string, error) {
	loc := b.Location()
	current := PeriodKey(b.PeriodKind, loc, now)
	if explicit == "" {
		return current, nil
	}
	if !ValidatePeriodKey(b.PeriodKind, explicit) {
		return "", ErrPeriodInvalid
	}
	previous := PreviousPeriodKey(b.PeriodKind, loc, now)
	if explicit == current || (previous != "" && explicit == previous) {
		return explicit, nil
	}
	return "", ErrPeriodNotSubmittable
}

// ResolveReadPeriod 解析读路径的 period 参数：缺省 = 当前期，显式时只要求
// 格式合法（任意保留期可读，分享卡复走历史期）。
func ResolveReadPeriod(b *Board, explicit string, now time.Time) (string, error) {
	if explicit == "" {
		return PeriodKey(b.PeriodKind, b.Location(), now), nil
	}
	if !ValidatePeriodKey(b.PeriodKind, explicit) {
		return "", ErrPeriodInvalid
	}
	return explicit, nil
}

// RetentionCutoffKey 返回应保留的最早一期（含）；比它更早的期可整期删除。
// 返回 ok=false 表示不需要清理（none 期或 retention=0）。
func RetentionCutoffKey(b *Board, now time.Time) (string, bool) {
	if b.PeriodKind == PeriodNone || b.RetentionPeriods <= 0 {
		return "", false
	}
	return PeriodKeyMinus(b.PeriodKind, b.Location(), now, int(b.RetentionPeriods)-1), true
}
