package functions

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// atUTC 是测试时刻构造辅助（秒位清零）。
func atUTC(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return v
}

// TestParseCronExpr_Next_TableDriven 表驱动覆盖：常规节拍、`*/n` 步进、
// 逗号列表、区间、区间+步进、2 月末（闰年边界）、月末进位（31 号在 30 天
// 月份跳过）、周六=6 / 周日 0 与 7 等价、DOM×DOW 并集、月/年进位。
func TestParseCronExpr_Next_TableDriven(t *testing.T) {
	cases := []struct {
		name string
		expr string
		from string
		want string
	}{
		{
			name: "每分钟",
			expr: "* * * * *",
			from: "2026-09-09T10:30:00Z",
			want: "2026-09-09T10:31:00Z",
		},
		{
			name: "每小时整点",
			expr: "0 * * * *",
			from: "2026-09-09T10:30:07Z",
			want: "2026-09-09T11:00:00Z",
		},
		{
			name: "每日零点跨日",
			expr: "0 0 * * *",
			from: "2026-09-09T23:59:59Z",
			want: "2026-09-10T00:00:00Z",
		},
		{
			name: "步进 */15 分钟",
			expr: "*/15 * * * *",
			from: "2026-09-09T10:16:00Z",
			want: "2026-09-09T10:30:00Z",
		},
		{
			name: "步进对齐回绕到下一小时",
			expr: "5-30/25 * * * *",
			from: "2026-09-09T10:31:00Z",
			want: "2026-09-09T11:05:00Z",
		},
		{
			name: "逗号列表",
			expr: "0,30 1 * * *",
			from: "2026-09-09T01:30:01Z",
			want: "2026-09-10T01:00:00Z",
		},
		{
			name: "区间",
			expr: "5-10 2 * * *",
			from: "2026-09-09T02:07:00Z",
			want: "2026-09-09T02:08:00Z",
		},
		{
			name: "2 月 28 日（平年月尾）",
			expr: "0 0 28 2 *",
			from: "2026-09-09T00:00:00Z",
			want: "2027-02-28T00:00:00Z",
		},
		{
			name: "2 月 29 日（闰年唯一日，跨 4 年搜索）",
			expr: "0 0 29 2 *",
			from: "2026-09-09T00:00:00Z",
			want: "2028-02-29T00:00:00Z",
		},
		{
			name: "31 号在 30 天月份跳过（月进位）",
			expr: "0 0 31 * *",
			from: "2026-04-30T00:00:01Z",
			want: "2026-05-31T00:00:00Z",
		},
		{
			name: "12 月 31 日跨年",
			expr: "59 23 31 12 *",
			from: "2026-12-31T23:59:30Z",
			want: "2027-12-31T23:59:00Z",
		},
		{
			name: "周日 0",
			expr: "0 12 * * 0",
			from: "2026-09-09T00:00:00Z", // 周三
			want: "2026-09-13T12:00:00Z",
		},
		{
			name: "周日 7 与 0 等价",
			expr: "0 12 * * 7",
			from: "2026-09-09T00:00:00Z",
			want: "2026-09-13T12:00:00Z",
		},
		{
			name: "周六 6",
			expr: "0 12 * * 6",
			from: "2026-09-09T00:00:00Z",
			want: "2026-09-12T12:00:00Z",
		},
		{
			name: "DOM×DOW 并集（13 号或周五）",
			expr: "0 0 13 * 5",
			from: "2026-09-09T00:00:00Z", // 周三；9/13 是周日 → 命中周五 9/11
			want: "2026-09-11T00:00:00Z",
		},
		{
			name: "DOW 不受限时仅 DOM 生效",
			expr: "0 0 13 * *",
			from: "2026-09-09T00:00:00Z",
			want: "2026-09-13T00:00:00Z",
		},
		{
			name: "小时区间",
			expr: "0 9-17 * * *",
			from: "2026-09-09T18:00:00Z",
			want: "2026-09-10T09:00:00Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseCronExpr(tc.expr)
			require.NoError(t, err)
			got, err := c.Next(atUTC(t, tc.from))
			require.NoError(t, err)
			require.Equal(t, tc.want, got.Format(time.RFC3339))
		})
	}
}

// TestParseCronExpr_Rejects 表驱动覆盖越界/非法表达式拒绝（fail-closed，
// 不做静默取模）。
func TestParseCronExpr_Rejects(t *testing.T) {
	cases := []struct {
		name string
		expr string
	}{
		{"分钟越界", "60 * * * *"},
		{"小时越界", "* 24 * * *"},
		{"日越界", "* * 32 * *"},
		{"月越界", "* * * 13 *"},
		{"周越界", "* * * * 8"},
		{"区间反序", "10-5 * * * *"},
		{"字母", "a * * * *"},
		{"负数", "-1 * * * *"},
		{"字段数不足", "* * * *"},
		{"字段数过多", "* * * * * *"},
		{"空表达式", ""},
		{"零步进", "*/0 * * * *"},
		{"空区间", "- * * * *"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCronExpr(tc.expr)
			require.Error(t, err)
		})
	}
}

// TestParseCronExpr_NextMonotonic 锁定 Next 严格单调（不含 from 本刻）：
// 恰好命中时刻调用 Next 得到下一节拍——cron 领取推进依赖该性质。
func TestParseCronExpr_NextMonotonic(t *testing.T) {
	c, err := ParseCronExpr("*/15 * * * *")
	require.NoError(t, err)
	t0 := atUTC(t, "2026-09-09T10:30:00Z")
	t1, err := c.Next(t0)
	require.NoError(t, err)
	require.Equal(t, "2026-09-09T10:45:00Z", t1.Format(time.RFC3339))
	t2, err := c.Next(t1)
	require.NoError(t, err)
	require.Equal(t, "2026-09-09T11:00:00Z", t2.Format(time.RFC3339))
}

// TestDefaultCronNext 表驱动锁定 misfire 语义（设计 §3，P1 验收锚点）：
// 准时（宽限内）→ 正常节拍推进 + 补跑；错过 → 一次推进跨过全部错过周期；
// catch_up_once 补跑 1 次 / skip 只推进不补跑——宕机 1000 周期也只补 1 次。
func TestDefaultCronNext(t *testing.T) {
	due := atUTC(t, "2026-09-09T10:00:00Z")

	t.Run("准时补跑", func(t *testing.T) {
		plan, err := DefaultCronNext(CronNextInput{Due: due, Now: due.Add(20 * time.Second), Expr: "0 10 * * *", Misfire: MisfireCatchUpOnce})
		require.NoError(t, err)
		require.True(t, plan.Run)
		require.Equal(t, "2026-09-10T10:00:00Z", plan.Next.Format(time.RFC3339))
	})

	t.Run("错过 catch_up_once 补跑一次且跳到 now 之后", func(t *testing.T) {
		// 宕机 3 天后恢复：一次推进跨过 9/10、9/11、9/12 三个周期。
		plan, err := DefaultCronNext(CronNextInput{Due: due, Now: atUTC(t, "2026-09-12T23:00:00Z"), Expr: "0 10 * * *", Misfire: MisfireCatchUpOnce})
		require.NoError(t, err)
		require.True(t, plan.Run)
		require.Equal(t, "2026-09-13T10:00:00Z", plan.Next.Format(time.RFC3339))
	})

	t.Run("错过 skip 只推进不补跑", func(t *testing.T) {
		plan, err := DefaultCronNext(CronNextInput{Due: due, Now: atUTC(t, "2026-09-12T23:00:00Z"), Expr: "0 10 * * *", Misfire: MisfireSkip})
		require.NoError(t, err)
		require.False(t, plan.Run)
		require.Equal(t, "2026-09-13T10:00:00Z", plan.Next.Format(time.RFC3339))
	})

	t.Run("misfire 空按默认 catch_up_once", func(t *testing.T) {
		plan, err := DefaultCronNext(CronNextInput{Due: due, Now: atUTC(t, "2026-09-12T23:00:00Z"), Expr: "0 10 * * *"})
		require.NoError(t, err)
		require.True(t, plan.Run)
	})

	t.Run("宽限边界：恰在 90s 处仍按准时", func(t *testing.T) {
		plan, err := DefaultCronNext(CronNextInput{Due: due, Now: due.Add(CronMisfireGrace), Expr: "0 10 * * *"})
		require.NoError(t, err)
		require.True(t, plan.Run)
		require.Equal(t, "2026-09-10T10:00:00Z", plan.Next.Format(time.RFC3339))
	})

	t.Run("准时但自然下一节拍已过期：补跑本次且推进到 now 之后", func(t *testing.T) {
		// 每分钟表达式 + 扫描滞后 80s：Next(due)=10:02:00 已 ≤ now=10:02:20。
		// 不变量：推进目标必须 > now（否则下一轮扫描立即重复领取——bunrepo
		// 并发测试观测到的双领取根因）。
		due := atUTC(t, "2026-09-09T10:01:00Z")
		now := atUTC(t, "2026-09-09T10:02:20Z")
		plan, err := DefaultCronNext(CronNextInput{Due: due, Now: now, Expr: "* * * * *", Misfire: MisfireCatchUpOnce})
		require.NoError(t, err)
		require.True(t, plan.Run)
		require.Equal(t, "2026-09-09T10:03:00Z", plan.Next.Format(time.RFC3339))
		require.True(t, plan.Next.After(now), "推进目标必须严格大于 now")
	})

	t.Run("坏表达式报错", func(t *testing.T) {
		_, err := DefaultCronNext(CronNextInput{Due: due, Now: due, Expr: "bad * * * *"})
		require.Error(t, err)
	})
}
