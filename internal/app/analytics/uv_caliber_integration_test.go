// 外部测试包（analytics_test）：S12 UV 口径统一验收——同一批 raw 事件
//（含无归属 user_id='' 事件）在 raw 面与 rollup 面的 UV 数字严格一致
//（排除空归属口径）。口径声明与收敛边界见 docs/design/analytics.md §6。
package analytics_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
)

// TestRollupIntegration_UVCaliberConsistency（S12 项4）：修复前 raw 面
//（COUNT(DISTINCT user_id)）计入空归属、rollup 面（user_days 基座 /
// daily.unique_users）排除——同一数据两分支数字漂移；统一为排除后一致。
func TestRollupIntegration_UVCaliberConsistency(t *testing.T) {
	dayD := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	dayDm1 := dayD.AddDate(0, 0, -1)
	clock := dayD.Add(12 * time.Hour)
	e := setupRollupEnv(t)
	qr := bunrepo.NewAnalyticsQueryRepository(e.db)

	// 昨日：level_complete 5 条 = u1×2 + u2×1 + 无归属×2（计入 total、
	// 不计入 UV——旧口径下 raw UV 会是 3，daily 是 2，两分支漂移）。
	base := dayDm1.Add(10 * time.Hour)
	e.seedEvent(t, "level_complete", "u1", base)
	e.seedEvent(t, "level_complete", "u1", base.Add(time.Hour))
	e.seedEvent(t, "level_complete", "u2", base.Add(2*time.Hour))
	e.seedEvent(t, "level_complete", "", base.Add(3*time.Hour))
	e.seedEvent(t, "level_complete", "", base.Add(4*time.Hour))
	// 今日：ad_watch 2 条 = u3 + 无归属（覆盖 RawTodayStats 今日实时块）。
	todayBase := dayD.Add(9 * time.Hour)
	e.seedEvent(t, "ad_watch", "u3", todayBase)
	e.seedEvent(t, "ad_watch", "", todayBase.Add(time.Hour))

	e.runRollup(t, clock)

	// rollup 面：daily.unique_users 排除空归属；total 仍计全部事件。
	lcTotal, lcUV := e.dailyRow(t, dayDm1, "level_complete")
	require.Equal(t, int64(5), lcTotal, "无归属事件计入 total")
	require.Equal(t, int64(2), lcUV, "daily unique_users 排除空归属（旧口径为 3）")
	adTotal, adUV := e.dailyRow(t, dayD, "ad_watch")
	require.Equal(t, int64(2), adTotal)
	require.Equal(t, int64(1), adUV)
	// user_days 基座不变：只有归属用户进表。
	require.Equal(t, 3, e.userDaysCount(t))

	// raw 面：同一批数据的 KPI / 分桶 / 今日块 UV 同口径。
	kpiTotal, kpiUV, err := qr.RawOverviewKPI(e.ctx, e.projectID, dayDm1, dayD)
	require.NoError(t, err)
	require.Equal(t, int64(5), kpiTotal)
	require.Equal(t, int64(2), kpiUV, "RawOverviewKPI UV 排除空归属（FILTER 口径）")

	pts, err := qr.RawTimeseries(e.ctx, e.projectID, nil, dayDm1, dayD, domainanalytics.GranularityDay)
	require.NoError(t, err)
	require.Len(t, pts, 1)
	require.Equal(t, int64(5), pts[0].Total)
	require.Equal(t, int64(2), pts[0].UniqueUsers, "RawTimeseries UV 排除空归属")

	today, err := qr.RawTodayStats(e.ctx, e.projectID, dayD)
	require.NoError(t, err)
	require.Equal(t, int64(2), today.TotalEvents)
	require.Equal(t, int64(1), today.UniqueUsers, "今日块 UV 排除空归属")

	// 分支一致性：raw 窗口 UV == rollup daily 单名 unique_users（窗口
	// [D-1, D) 内只有 level_complete 一个名字，精确可比；跨名并集 UV 由
	// user_days 承担，本文件不重复 D6 断言）。
	require.Equal(t, lcUV, kpiUV, "同一数据 raw 与 rollup 两分支 UV 一致")

	// 收敛边界（口径切换不回写历史日）：再落 1 条无归属事件到 D-2（旧窗口，
	// 本轮 rollup 不重算），daily D-1/D 数字不变——只有 [昨日, 今日] 被覆盖
	// 重写，更早历史日保持旧口径产物直到全量重算入口统一。
	e.seedEvent(t, "level_complete", "", dayDm1.AddDate(0, 0, -1).Add(10*time.Hour))
	e.runRollup(t, clock)
	total2, unique2 := e.dailyRow(t, dayDm1, "level_complete")
	require.Equal(t, lcTotal, total2, "窗口外历史日不被本轮重算触碰")
	require.Equal(t, lcUV, unique2)
}
