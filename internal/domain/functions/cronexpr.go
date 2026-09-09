package functions

import (
	"fmt"
	"time"
)

// cronExpr 是 5 字段 UTC cron 表达式的解析结果（分 时 日 月 周；P1 触发器
// K10：一期 UTC，触发器级时区后置）。自实现不引第三方依赖（仓库零依赖惯例）：
// 支持 `*`、`,`、`-`、`/` 与纯数字；周字段 0-7 且 7 ≡ 0（周日）；越界一律
// 解析报错（fail-closed），不做静默取模。
//
// 语义遵循 POSIX crontab 主流实现（Next 与 vixie cron 一致）：
//   - 日(月内)与周两个字段均受限（非纯 *）时取并集——任一命中即视为命中日；
//   - 仅一个受限时按该字段判定。
type cronExpr struct {
	raw         string
	minutes     [60]bool
	hours       [60]bool // 复用 60 容量位集（小时仅用 0..23）
	days        [32]bool // 下标 1..31
	months      [13]bool // 下标 1..12
	weekdays    [7]bool  // 0..6（7 归一为 0）
	dayLimited  bool     // 日字段是否受限（非纯 *）
	wdayLimited bool
}

// CronExprNextHorizon 是 Next 的搜索上限（4 年：覆盖「仅 2/29 匹配」的最坏
// 情形两轮）。
const CronExprNextHorizon = 4

// ParseCronExpr 解析 5 字段 UTC cron 表达式。
func ParseCronExpr(expr string) (*cronExpr, error) {
	fields := splitFields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expr must have 5 fields (minute hour day month weekday), got %d", len(fields))
	}
	c := &cronExpr{raw: expr}
	var err error
	if c.minutes, err = parseCronField60(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if c.hours, err = parseCronField60(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	days, err := parseCronField32(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("day field: %w", err)
	}
	copy(c.days[:], days[:])
	months, err := parseCronField32(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	copy(c.months[:], months[:])
	weekdays, err := parseCronField32(fields[4], 0, 7)
	if err != nil {
		return nil, fmt.Errorf("weekday field: %w", err)
	}
	// 7 ≡ 0（周日）：vixie cron 兼容语义。
	weekdays[0] = weekdays[0] || weekdays[7]
	copy(c.weekdays[:], weekdays[:7])
	c.dayLimited = fields[2] != "*"
	c.wdayLimited = fields[4] != "*"
	return c, nil
}

// Next 返回 from 之后第一个匹配时刻（UTC 语义，不含 from 本刻）。
func (c *cronExpr) Next(from time.Time) (time.Time, error) {
	t := from.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(CronExprNextHorizon, 0, 0)
	for t.Before(limit) {
		if !c.months[int(t.Month())] {
			// 跳到下月 1 日 00:00。
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
			continue
		}
		if !c.matchesDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
			continue
		}
		if !c.hours[t.Hour()] {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.UTC).Add(time.Hour)
			continue
		}
		if !c.minutes[t.Minute()] {
			t = t.Add(time.Minute)
			continue
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cron expr %q has no matching time within %d years", c.raw, CronExprNextHorizon)
}

// Validate 暴露给 app 层做创建期校验（解析失败即拒绝）。
func ValidateCronExpr(expr string) error {
	_, err := ParseCronExpr(expr)
	return err
}

// matchesDay 判定日期命中：两字段均受限取并集（vixie cron 语义），否则按
// 受限的一方。Go 的 Weekday 与 cron 同构（Sunday=0 .. Saturday=6）。
func (c *cronExpr) matchesDay(t time.Time) bool {
	dayOK := c.days[t.Day()]
	wdayOK := c.weekdays[int(t.Weekday())]
	switch {
	case c.dayLimited && c.wdayLimited:
		return dayOK || wdayOK
	case c.dayLimited:
		return dayOK
	case c.wdayLimited:
		return wdayOK
	default:
		return true
	}
}

// splitFields 按空白切分表达式（容忍多余空白）。
func splitFields(expr string) []string {
	var out []string
	start := -1
	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		isSpace := ch == ' ' || ch == '\t'
		if !isSpace && start < 0 {
			start = i
		}
		if isSpace && start >= 0 {
			out = append(out, expr[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, expr[start:])
	}
	return out
}

// parseCronField60 解析分钟/小时字段为位集（分钟 0-59、小时 0-23，共用
// 60 容量位集）。
func parseCronField60(field string, min, max int) ([60]bool, error) {
	var set [60]bool
	err := forEachCronValue(field, min, max, func(v int) { set[v] = true })
	return set, err
}

// parseCronField32 解析日/月/周字段为位集（周字段允许 7，归一由调用方处理）。
func parseCronField32(field string, min, max int) ([32]bool, error) {
	var set [32]bool
	err := forEachCronValue(field, min, max, func(v int) { set[v] = true })
	return set, err
}

// forEachCronValue 解析单个 cron 字段（`,` 列表 / `-` 区间 / `/` 步进 /
// `*` 全域）并逐值回调；越界报错。
func forEachCronValue(field string, min, max int, fn func(int)) error {
	for _, part := range splitList(field) {
		step := 1
		if idx := indexByte(part, '/'); idx >= 0 {
			s := part[idx+1:]
			part = part[:idx]
			v, ok := parseUint(s)
			if !ok || v <= 0 || v > max-min+1 {
				return fmt.Errorf("invalid step %q", s)
			}
			step = v
			if part == "" {
				// 「/n」裸步进视为全域步进（*/n 的省略形式）。
				part = "*"
			}
		}
		lo, hi := min, max
		if part != "*" {
			a, b, ok := splitRange(part)
			if !ok {
				return fmt.Errorf("invalid value %q", part)
			}
			lo, hi = a, b
		}
		if lo < min || hi > max || lo > hi {
			return fmt.Errorf("value %q out of range [%d,%d]", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			fn(v)
		}
	}
	return nil
}

func splitList(field string) []string {
	var out []string
	start := 0
	for i := 0; i < len(field); i++ {
		if field[i] == ',' {
			out = append(out, field[start:i])
			start = i + 1
		}
	}
	return append(out, field[start:])
}

// splitRange 解析「a」或「a-b」。
func splitRange(part string) (int, int, bool) {
	dash := -1
	for i := 0; i < len(part); i++ {
		if part[i] == '-' {
			dash = i
			break
		}
	}
	if dash < 0 {
		v, ok := parseUint(part)
		if !ok {
			return 0, 0, false
		}
		return v, v, true
	}
	if dash == 0 || dash == len(part)-1 {
		return 0, 0, false
	}
	av, okA := parseUint(part[:dash])
	bv, okB := parseUint(part[dash+1:])
	if !okA || !okB {
		return 0, 0, false
	}
	return av, bv, true
}

func parseUint(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	v := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		v = v*10 + int(s[i]-'0')
		if v > 1_000_000 {
			return 0, false
		}
	}
	return v, true
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
