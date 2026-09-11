package leaderboards

import (
	"testing"
	"time"
)

func shanghai(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestPeriodKeyDerivation(t *testing.T) {
	loc := shanghai(t)
	// 2026-09-11 18:30 +08（UTC 10:30）——派生必须在 board 时区做。
	at := time.Date(2026, 9, 11, 10, 30, 0, 0, time.UTC)
	if got := PeriodKey(PeriodDaily, loc, at); got != "20260911" {
		t.Errorf("daily = %q, want 20260911", got)
	}
	if got := PeriodKey(PeriodMonthly, loc, at); got != "202609" {
		t.Errorf("monthly = %q, want 202609", got)
	}
	// 2026-09-11 是周五，ISO 周 2026-W37。
	if got := PeriodKey(PeriodWeekly, loc, at); got != "2026-W37" {
		t.Errorf("weekly = %q, want 2026-W37", got)
	}
	if got := PeriodKey(PeriodNone, loc, at); got != "all" {
		t.Errorf("none = %q, want all", got)
	}
	// UTC 晚于上海跨日：UTC 2026-09-11 17:00 = 上海 09-12 凌晨。
	edge := time.Date(2026, 9, 11, 17, 0, 0, 0, time.UTC)
	if got := PeriodKey(PeriodDaily, loc, edge); got != "20260912" {
		t.Errorf("daily tz edge = %q, want 20260912", got)
	}
}

func TestPeriodKeyMinusMonthEndTrap(t *testing.T) {
	loc := shanghai(t)
	// 3 月 31 日的上一期必须是 2 月（AddDate(0,-1,0) 会归一化成 3 月）。
	at := time.Date(2026, 3, 31, 12, 0, 0, 0, loc)
	if got := PeriodKeyMinus(PeriodMonthly, loc, at, 1); got != "202602" {
		t.Errorf("previous monthly of 202603 = %q, want 202602", got)
	}
	// 跨年：1 月的上一期是去年 12 月。
	jan := time.Date(2026, 1, 5, 12, 0, 0, 0, loc)
	if got := PeriodKeyMinus(PeriodMonthly, loc, jan, 1); got != "202512" {
		t.Errorf("previous monthly of 202601 = %q, want 202512", got)
	}
	// 多期回退。
	if got := PeriodKeyMinus(PeriodMonthly, loc, jan, 13); got != "202412" {
		t.Errorf("monthly -13 = %q, want 202412", got)
	}
	// 周期跨年界（2027-01-01 周五属 2026-W53；回退一期 = 2026-W52）。
	ny := time.Date(2027, 1, 1, 12, 0, 0, 0, loc)
	if got := PeriodKey(PeriodWeekly, loc, ny); got != "2026-W53" {
		t.Errorf("weekly of 2027-01-01 = %q, want 2026-W53", got)
	}
	if got := PeriodKeyMinus(PeriodWeekly, loc, ny, 1); got != "2026-W52" {
		t.Errorf("previous weekly = %q, want 2026-W52", got)
	}
	// 日界跨月。
	d := time.Date(2026, 10, 1, 0, 30, 0, 0, loc)
	if got := PeriodKeyMinus(PeriodDaily, loc, d, 1); got != "20260930" {
		t.Errorf("previous daily = %q, want 20260930", got)
	}
}

func TestValidatePeriodKey(t *testing.T) {
	cases := []struct {
		kind PeriodKind
		key  string
		want bool
	}{
		{PeriodDaily, "20260911", true},
		{PeriodDaily, "20260931", false}, // 不存在的日期
		{PeriodDaily, "2026911", false},
		{PeriodWeekly, "2026-W37", true},
		{PeriodWeekly, "2026-W54", false},
		{PeriodWeekly, "2026W37", false},
		{PeriodMonthly, "202609", true},
		{PeriodMonthly, "202613", false},
		{PeriodMonthly, "2026091", false},
		{PeriodNone, "all", true},
		{PeriodNone, "20260911", false},
	}
	for _, c := range cases {
		if got := ValidatePeriodKey(c.kind, c.key); got != c.want {
			t.Errorf("ValidatePeriodKey(%s, %q) = %v, want %v", c.kind, c.key, got, c.want)
		}
	}
}

func TestResolveSubmittablePeriod(t *testing.T) {
	loc := shanghai(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, loc)
	board := &Board{PeriodKind: PeriodDaily, PeriodTZ: "Asia/Shanghai"}

	if got, err := ResolveSubmittablePeriod(board, "", now); err != nil || got != "20260911" {
		t.Errorf("default = %q err=%v, want 20260911", got, err)
	}
	if got, err := ResolveSubmittablePeriod(board, "20260910", now); err != nil || got != "20260910" {
		t.Errorf("previous = %q err=%v, want 20260910（宽限）", got, err)
	}
	if _, err := ResolveSubmittablePeriod(board, "20260909", now); err != ErrPeriodNotSubmittable {
		t.Errorf("two-ago = %v, want ErrPeriodNotSubmittable（封榜）", err)
	}
	if _, err := ResolveSubmittablePeriod(board, "20260912", now); err != ErrPeriodNotSubmittable {
		t.Errorf("future = %v, want ErrPeriodNotSubmittable", err)
	}
	if _, err := ResolveSubmittablePeriod(board, "2026-09-11", now); err != ErrPeriodInvalid {
		t.Errorf("bad format = %v, want ErrPeriodInvalid", err)
	}

	noneBoard := &Board{PeriodKind: PeriodNone}
	if got, err := ResolveSubmittablePeriod(noneBoard, "", now); err != nil || got != "all" {
		t.Errorf("none default = %q err=%v", got, err)
	}
	if _, err := ResolveSubmittablePeriod(noneBoard, "20260911", now); err == nil {
		t.Error("none 显式日期应拒收")
	}
}

func TestRetentionCutoffKey(t *testing.T) {
	loc := shanghai(t)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, loc)
	b := &Board{PeriodKind: PeriodDaily, PeriodTZ: "Asia/Shanghai", RetentionPeriods: 90}
	got, ok := RetentionCutoffKey(b, now)
	if !ok || got != "20260614" {
		t.Errorf("cutoff = %q ok=%v, want 20260614（当前期往前 89 天）", got, ok)
	}
	if _, ok := RetentionCutoffKey(&Board{PeriodKind: PeriodDaily, RetentionPeriods: 0}, now); ok {
		t.Error("retention=0 不清理")
	}
	if _, ok := RetentionCutoffKey(&Board{PeriodKind: PeriodNone, RetentionPeriods: 90}, now); ok {
		t.Error("none 期不清理")
	}
}

func int64Ptr(v int64) *int64               { return &v }
func dirPtr(d SortDirection) *SortDirection { return &d }

func TestMergeSubmitBest(t *testing.T) {
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	b := &Board{Sort: SortDesc, Policy: PolicyBest}
	cur := &Entry{Value: 900, SubmitCount: 1, UpdatedAt: base}

	// 更高分：取新值，improved=true。
	v, tb, improved := MergeSubmit(b, cur, 1500, nil, base.Add(time.Minute))
	if v != 1500 || tb != nil || !improved {
		t.Errorf("best higher = (%d,%v,%v)", v, tb, improved)
	}
	// 更低分：保留旧值，improved=false（updated_at 不推进）。
	v, tb, improved = MergeSubmit(b, cur, 100, nil, base.Add(time.Minute))
	if v != 900 || improved {
		t.Errorf("best lower = (%d,%v)", v, improved)
	}
	// 同分无 tiebreak：不推进。
	v, _, improved = MergeSubmit(b, cur, 900, nil, base.Add(time.Minute))
	if v != 900 || improved {
		t.Errorf("best equal = (%d,%v)", v, improved)
	}
	// asc 榜 best = 取小。
	ascBoard := &Board{Sort: SortAsc, Policy: PolicyBest}
	v, _, improved = MergeSubmit(ascBoard, cur, 100, nil, base.Add(time.Minute))
	if v != 100 || !improved {
		t.Errorf("best asc lower = (%d,%v)", v, improved)
	}
}

func TestMergeSubmitBestTiebreak(t *testing.T) {
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	b := &Board{Sort: SortDesc, Policy: PolicyBest, TiebreakOrder: dirPtr(SortAsc)}
	cur := &Entry{Value: 900, TiebreakValue: int64Ptr(200), SubmitCount: 1, UpdatedAt: base}

	// 同分 + 更快 tiebreak：仅 tiebreak 更新，improved=true。
	v, tb, improved := MergeSubmit(b, cur, 900, int64Ptr(150), base.Add(time.Minute))
	if v != 900 || tb == nil || *tb != 150 || !improved {
		t.Errorf("tiebreak better = (%d,%v,%v)", v, tb, improved)
	}
	// 同分 + 更慢 tiebreak：保持原样。
	v, tb, improved = MergeSubmit(b, cur, 900, int64Ptr(300), base.Add(time.Minute))
	if v != 900 || tb == nil || *tb != 200 || improved {
		t.Errorf("tiebreak worse = (%d,%v,%v)", v, tb, improved)
	}
}

func TestMergeSubmitLatestAndSum(t *testing.T) {
	base := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	latest := &Board{Policy: PolicyLatest}
	cur := &Entry{Value: 900, SubmitCount: 1, UpdatedAt: base}
	v, _, improved := MergeSubmit(latest, cur, 100, nil, base.Add(time.Minute))
	if v != 100 || !improved {
		t.Errorf("latest = (%d,%v)", v, improved)
	}

	sum := &Board{Policy: PolicySum}
	cur = &Entry{Value: 900, SubmitCount: 1, UpdatedAt: base}
	v, _, improved = MergeSubmit(sum, cur, 1500, nil, base.Add(time.Minute))
	if v != 2400 || !improved {
		t.Errorf("sum = (%d,%v), want 2400", v, improved)
	}
}

func TestValidateBoard(t *testing.T) {
	ok := &Board{
		ID: "daily_final", Sort: SortDesc, TieBreak: TieBreakParallel, Policy: PolicyBest,
		PeriodKind: PeriodDaily, PeriodTZ: "Asia/Shanghai",
		ValueMin: int64Ptr(0), ValueMax: int64Ptr(3080),
		PerSubjectLimit: 20, RetentionPeriods: 90, SubjectKind: "user",
	}
	if err := ValidateBoard(ok); err != nil {
		t.Fatalf("valid board rejected: %v", err)
	}
	bad := func(mutate func(*Board)) bool { return ValidateBoard(mutateClone(ok, mutate)) != nil }
	if !bad(func(b *Board) { b.ID = "Daily-Final" }) {
		t.Error("id 大写/连字符应拒收")
	}
	if !bad(func(b *Board) { b.ID = "" }) {
		t.Error("空 id 应拒收")
	}
	if !bad(func(b *Board) { b.Sort = "up" }) {
		t.Error("非法 sort 应拒收")
	}
	if !bad(func(b *Board) { b.PeriodTZ = "Mars/Olympus" }) {
		t.Error("非法时区应拒收")
	}
	if !bad(func(b *Board) { b.ValueMin, b.ValueMax = int64Ptr(100), int64Ptr(50) }) {
		t.Error("min>max 应拒收")
	}
	if !bad(func(b *Board) { b.PerSubjectLimit = 0 }) {
		t.Error("limit=0 应拒收")
	}
	if !bad(func(b *Board) { b.RetentionPeriods = -1 }) {
		t.Error("负 retention 应拒收")
	}
	if !bad(func(b *Board) { b.TiebreakOrder = dirPtr("diag") }) {
		t.Error("非法 tiebreak 方向应拒收")
	}
	// none 期不校验 tz。
	none := mutateClone(ok, func(b *Board) { b.PeriodKind = PeriodNone; b.PeriodTZ = "" })
	if err := ValidateBoard(none); err != nil {
		t.Errorf("none board without tz rejected: %v", err)
	}
}

func mutateClone(b *Board, f func(*Board)) *Board {
	c := *b
	f(&c)
	return &c
}

func TestValidateValue(t *testing.T) {
	b := &Board{ValueMin: int64Ptr(0), ValueMax: int64Ptr(3080)}
	if err := b.ValidateValue(0); err != nil {
		t.Errorf("0 应合法（value_min 边界）: %v", err)
	}
	if err := b.ValidateValue(3080); err != nil {
		t.Errorf("3080 应合法: %v", err)
	}
	if err := b.ValidateValue(3081); err != ErrValueOutOfRange {
		t.Errorf("3081 = %v, want ErrValueOutOfRange", err)
	}
	if err := b.ValidateValue(-1); err != ErrValueOutOfRange {
		t.Errorf("-1 = %v, want ErrValueOutOfRange", err)
	}
	unbounded := &Board{}
	if err := unbounded.ValidateValue(1 << 53); err != ErrValueTooLarge {
		t.Errorf("2^53 = %v, want ErrValueTooLarge", err)
	}
}
