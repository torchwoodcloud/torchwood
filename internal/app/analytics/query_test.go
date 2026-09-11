package analytics

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// queryRepoSpy 是查询仓储桩：记录触库调用（护栏矩阵断言"超窗不触库"的
// 判定量）+ 可编程返回值。
type queryRepoSpy struct {
	calls          []string
	coveredDays    int
	dailySeries    []domainanalytics.DailyPoint
	userDaySeries  []domainanalytics.DailyPoint
	rawPoints      []domainanalytics.TimeseriesPoint
	rawKpiTotal    int64
	rawKpiUV       int64
	newUsers       int64
	activeUsers    int64
	retention      []domainanalytics.RetentionCohort
	breakdownTop   []domainanalytics.BreakdownBucket
	breakdownOther *domainanalytics.BreakdownBucket
}

func (s *queryRepoSpy) touched() bool { return len(s.calls) > 0 }

func (s *queryRepoSpy) mark(call string) { s.calls = append(s.calls, call) }

func (s *queryRepoSpy) DailyCoveredDays(_ context.Context, _ string, _, _ time.Time) (int, error) {
	s.mark("DailyCoveredDays")
	return s.coveredDays, nil
}

func (s *queryRepoSpy) DailySeries(_ context.Context, _, _ string, _, _ time.Time) ([]domainanalytics.DailyPoint, error) {
	s.mark("DailySeries")
	return s.dailySeries, nil
}

func (s *queryRepoSpy) UserDaySeries(_ context.Context, _ string, _, _ time.Time) ([]domainanalytics.DailyPoint, error) {
	s.mark("UserDaySeries")
	return s.userDaySeries, nil
}

func (s *queryRepoSpy) CountActiveUsers(_ context.Context, _ string, _, _ time.Time) (int64, error) {
	s.mark("CountActiveUsers")
	return s.activeUsers, nil
}

func (s *queryRepoSpy) CountNewUsers(_ context.Context, _ string, _, _ time.Time) (int64, error) {
	s.mark("CountNewUsers")
	return s.newUsers, nil
}

func (s *queryRepoSpy) TopEventsFromDaily(_ context.Context, _ string, _, _ time.Time, _ int) ([]domainanalytics.TopEvent, error) {
	s.mark("TopEventsFromDaily")
	return nil, nil
}

func (s *queryRepoSpy) ListEventDefinitions(_ context.Context, _ string, _, _ int) ([]domainanalytics.EventDefinitionInfo, int, error) {
	s.mark("ListEventDefinitions")
	return nil, 0, nil
}

func (s *queryRepoSpy) RawTimeseries(_ context.Context, _ string, _ []string, _, _ time.Time, _ domainanalytics.Granularity) ([]domainanalytics.TimeseriesPoint, error) {
	s.mark("RawTimeseries")
	return s.rawPoints, nil
}

func (s *queryRepoSpy) RawOverviewKPI(_ context.Context, _ string, _, _ time.Time) (int64, int64, error) {
	s.mark("RawOverviewKPI")
	return s.rawKpiTotal, s.rawKpiUV, nil
}

func (s *queryRepoSpy) RawTopEvents(_ context.Context, _ string, _, _ time.Time, _ int) ([]domainanalytics.TopEvent, error) {
	s.mark("RawTopEvents")
	return nil, nil
}

func (s *queryRepoSpy) RawTodayStats(_ context.Context, _ string, _ time.Time) (domainanalytics.TodayStats, error) {
	s.mark("RawTodayStats")
	return domainanalytics.TodayStats{}, nil
}

func (s *queryRepoSpy) RawBreakdown(_ context.Context, _, _, _ string, _, _ time.Time, _ int) ([]domainanalytics.BreakdownBucket, *domainanalytics.BreakdownBucket, error) {
	s.mark("RawBreakdown")
	return s.breakdownTop, s.breakdownOther, nil
}

func (s *queryRepoSpy) RetentionMatrix(_ context.Context, _ string, _, _ time.Time) ([]domainanalytics.RetentionCohort, error) {
	s.mark("RetentionMatrix")
	return s.retention, nil
}

func (s *queryRepoSpy) ListUserEvents(_ context.Context, _ string, _ domainanalytics.UserEventsQuery) ([]domainanalytics.UserEvent, bool, error) {
	s.mark("ListUserEvents")
	return nil, false, nil
}

func newQueryForTest(repo domainanalytics.QueryRepository) *Query {
	q := NewQuery(repo)
	q.now = func() time.Time { return time.Date(2026, 9, 11, 15, 0, 0, 0, time.UTC) }
	return q
}

var guardBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

// TestQueryGuards_RejectWithoutRepoAccess：护栏矩阵——超窗/乱序/未指定粒度/
// 恶意注入面全部 InvalidArgument 且不触库（repo 桩零调用）。
func TestQueryGuards_RejectWithoutRepoAccess(t *testing.T) {
	cases := []struct {
		name string
		run  func(q *Query) error
	}{
		{"timeseries hour window > 7d", func(q *Query) error {
			_, err := q.QueryTimeseries(context.Background(), TimeseriesCommand{
				ProjectID: "p1", PeriodStart: guardBase, PeriodEnd: guardBase.Add(7*24*time.Hour + time.Hour),
				Granularity: domainanalytics.GranularityHour})
			return err
		}},
		{"timeseries day window > 366d", func(q *Query) error {
			_, err := q.QueryTimeseries(context.Background(), TimeseriesCommand{
				ProjectID: "p1", PeriodStart: guardBase, PeriodEnd: guardBase.Add(367 * 24 * time.Hour),
				Granularity: domainanalytics.GranularityDay})
			return err
		}},
		{"timeseries unspecified granularity", func(q *Query) error {
			_, err := q.QueryTimeseries(context.Background(), TimeseriesCommand{
				ProjectID: "p1", PeriodStart: guardBase, PeriodEnd: guardBase.Add(time.Hour)})
			return err
		}},
		{"timeseries malicious name", func(q *Query) error {
			_, err := q.QueryTimeseries(context.Background(), TimeseriesCommand{
				ProjectID: "p1", Names: []string{`x"; DROP TABLE analytics_events;--`},
				PeriodStart: guardBase, PeriodEnd: guardBase.Add(time.Hour),
				Granularity: domainanalytics.GranularityDay})
			return err
		}},
		{"timeseries start >= end", func(q *Query) error {
			_, err := q.QueryTimeseries(context.Background(), TimeseriesCommand{
				ProjectID: "p1", PeriodStart: guardBase, PeriodEnd: guardBase,
				Granularity: domainanalytics.GranularityDay})
			return err
		}},
		{"overview window > 366d", func(q *Query) error {
			_, err := q.GetOverview(context.Background(), OverviewCommand{
				ProjectID: "p1", PeriodStart: guardBase, PeriodEnd: guardBase.Add(367 * 24 * time.Hour)})
			return err
		}},
		{"breakdown window > 30d", func(q *Query) error {
			_, err := q.QueryBreakdown(context.Background(), BreakdownCommand{
				ProjectID: "p1", Name: "evt", PropKey: "channel",
				PeriodStart: guardBase, PeriodEnd: guardBase.Add(31 * 24 * time.Hour), TopN: 10})
			return err
		}},
		{"breakdown top_n > 50", func(q *Query) error {
			_, err := q.QueryBreakdown(context.Background(), BreakdownCommand{
				ProjectID: "p1", Name: "evt", PropKey: "channel",
				PeriodStart: guardBase, PeriodEnd: guardBase.Add(time.Hour), TopN: 51})
			return err
		}},
		{"breakdown malicious prop_key", func(q *Query) error {
			_, err := q.QueryBreakdown(context.Background(), BreakdownCommand{
				ProjectID: "p1", Name: "evt", PropKey: `x') OR ('1'='1`,
				PeriodStart: guardBase, PeriodEnd: guardBase.Add(time.Hour), TopN: 10})
			return err
		}},
		{"breakdown malicious name", func(q *Query) error {
			_, err := q.QueryBreakdown(context.Background(), BreakdownCommand{
				ProjectID: "p1", Name: `evt'; DROP TABLE analytics_events;--`, PropKey: "channel",
				PeriodStart: guardBase, PeriodEnd: guardBase.Add(time.Hour), TopN: 10})
			return err
		}},
		{"retention cohort window > 92d", func(q *Query) error {
			_, err := q.QueryRetention(context.Background(), RetentionCommand{
				ProjectID: "p1", CohortStart: guardBase, CohortEnd: guardBase.Add(93 * 24 * time.Hour)})
			return err
		}},
		{"user events window > 92d", func(q *Query) error {
			start := guardBase
			end := guardBase.Add(93 * 24 * time.Hour)
			_, err := q.ListUserEvents(context.Background(), UserEventsCommand{
				ProjectID: "p1", UserID: "u1", PeriodStart: &start, PeriodEnd: &end})
			return err
		}},
		{"user events bad page token", func(q *Query) error {
			_, err := q.ListUserEvents(context.Background(), UserEventsCommand{
				ProjectID: "p1", UserID: "u1", PageToken: "!!!not-base64!!!"})
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spy := &queryRepoSpy{}
			err := tc.run(newQueryForTest(spy))
			require.Equal(t, codes.InvalidArgument, status.Code(err), "err=%v", err)
			require.False(t, spy.touched(), "护栏拒绝必须先于触库（零仓储调用）")
		})
	}

	// 项目上下文缺失：Unauthenticated（与摄入链同语义），同样不触库。
	spy := &queryRepoSpy{}
	_, err := newQueryForTest(spy).GetOverview(context.Background(), OverviewCommand{
		PeriodStart: guardBase, PeriodEnd: guardBase.Add(time.Hour)})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.False(t, spy.touched())
}

// TestQueryTimeseries_FallbackRouting：覆盖检测择路——daily 无覆盖（含
// rollup 表空）回退 raw；多事件名恒 raw；单名且覆盖走 rollup。
func TestQueryTimeseries_FallbackRouting(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	end := start.Add(3 * 24 * time.Hour)

	// 覆盖缺失（表空）：回退 raw，source=raw。
	spy := &queryRepoSpy{coveredDays: 0}
	res, err := newQueryForTest(spy).QueryTimeseries(ctx, TimeseriesCommand{
		ProjectID: "p1", PeriodStart: start, PeriodEnd: end, Granularity: domainanalytics.GranularityDay})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.Source)
	require.Equal(t, []string{"DailyCoveredDays", "RawTimeseries"}, spy.calls)
	require.Len(t, res.Points, 3, "DAY 零桶补齐为 3 个日桶")

	// 覆盖完整：rollup 单名路径（DailySeries name=evt）。
	spy = &queryRepoSpy{coveredDays: 3, dailySeries: []domainanalytics.DailyPoint{
		{Day: start, Total: 5, UniqueUsers: 2},
		{Day: start.Add(24 * time.Hour), Total: 7, UniqueUsers: 3},
		{Day: start.Add(48 * time.Hour), Total: 0, UniqueUsers: 0},
	}}
	res, err = newQueryForTest(spy).QueryTimeseries(ctx, TimeseriesCommand{
		ProjectID: "p1", Names: []string{"evt"}, PeriodStart: start, PeriodEnd: end,
		Granularity: domainanalytics.GranularityDay})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.Source)
	require.Equal(t, []string{"DailyCoveredDays", "DailySeries"}, spy.calls)
	require.Equal(t, int64(5), res.Points[0].Total)

	// 覆盖完整 + 全事件：daily 总量 + user_days UV 合并。
	spy = &queryRepoSpy{coveredDays: 3,
		dailySeries:   []domainanalytics.DailyPoint{{Day: start, Total: 5}},
		userDaySeries: []domainanalytics.DailyPoint{{Day: start, UniqueUsers: 4}}}
	res, err = newQueryForTest(spy).QueryTimeseries(ctx, TimeseriesCommand{
		ProjectID: "p1", PeriodStart: start, PeriodEnd: end, Granularity: domainanalytics.GranularityDay})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.Source)
	require.Equal(t, []string{"DailyCoveredDays", "DailySeries", "UserDaySeries"}, spy.calls)
	require.Equal(t, int64(4), res.Points[0].UniqueUsers)

	// 多事件名：并集 UV 不可从 rollup 导出 → 恒 raw（跳过覆盖检测）。
	spy = &queryRepoSpy{coveredDays: 3}
	res, err = newQueryForTest(spy).QueryTimeseries(ctx, TimeseriesCommand{
		ProjectID: "p1", Names: []string{"a", "b"}, PeriodStart: start, PeriodEnd: end,
		Granularity: domainanalytics.GranularityDay})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.Source)
	require.Equal(t, []string{"RawTimeseries"}, spy.calls)

	// 覆盖缺失且窗口 > 保留期缺省（90d）：拒绝（不静默回退、不扫 raw）。
	spy = &queryRepoSpy{}
	_, err = newQueryForTest(spy).QueryTimeseries(ctx, TimeseriesCommand{
		ProjectID: "p1", PeriodStart: start, PeriodEnd: start.Add(91 * 24 * time.Hour),
		Granularity: domainanalytics.GranularityDay})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, []string{"DailyCoveredDays"}, spy.calls, "仅覆盖检测触库，raw 不扫")

	// HOUR：恒 raw、零桶补齐按小时。
	spy = &queryRepoSpy{}
	res, err = newQueryForTest(spy).QueryTimeseries(ctx, TimeseriesCommand{
		ProjectID: "p1", PeriodStart: start, PeriodEnd: start.Add(3 * time.Hour),
		Granularity: domainanalytics.GranularityHour})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.Source)
	require.Len(t, res.Points, 3)
}

// TestQueryOverview_Fallback：概览覆盖择路（raw 回退 + retention 超窗拒绝）。
func TestQueryOverview_Fallback(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(2 * 24 * time.Hour)

	spy := &queryRepoSpy{coveredDays: 0, rawKpiTotal: 9, rawKpiUV: 3, newUsers: 2}
	res, err := newQueryForTest(spy).GetOverview(ctx, OverviewCommand{ProjectID: "p1", PeriodStart: start, PeriodEnd: end})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.Source)
	require.Equal(t, int64(9), res.Kpi.TotalEvents)
	require.Equal(t, int64(3), res.Kpi.UniqueUsers)
	require.Equal(t, int64(2), res.Kpi.NewUsers)
	require.InDelta(t, 3.0, res.Kpi.EventsPerUser, 1e-9)
	require.Equal(t, []string{"DailyCoveredDays", "RawOverviewKPI", "RawTopEvents", "CountNewUsers", "RawTodayStats"}, spy.calls)

	// rollup 覆盖路径。
	spy = &queryRepoSpy{coveredDays: 2,
		dailySeries: []domainanalytics.DailyPoint{{Day: start, Total: 5}, {Day: start.Add(24 * time.Hour), Total: 4}},
		activeUsers: 6}
	res, err = newQueryForTest(spy).GetOverview(ctx, OverviewCommand{ProjectID: "p1", PeriodStart: start, PeriodEnd: end})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.Source)
	require.Equal(t, int64(9), res.Kpi.TotalEvents)
	require.Equal(t, int64(6), res.Kpi.UniqueUsers)
	require.Equal(t, []string{"DailyCoveredDays", "DailySeries", "CountActiveUsers", "TopEventsFromDaily", "CountNewUsers", "RawTodayStats"}, spy.calls)

	// 覆盖缺失且超保留期：拒绝。
	spy = &queryRepoSpy{}
	_, err = newQueryForTest(spy).GetOverview(ctx, OverviewCommand{
		ProjectID: "p1", PeriodStart: start, PeriodEnd: start.Add(120 * 24 * time.Hour)})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// TestQueryBreakdown_OtherMerge：__other__ 归并与 (unset) 透传、top_n 缺省。
func TestQueryBreakdown_OtherMerge(t *testing.T) {
	ctx := context.Background()
	start := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)

	spy := &queryRepoSpy{
		breakdownTop: []domainanalytics.BreakdownBucket{
			{Value: "wechat", Total: 10, UniqueUsers: 4},
			{Value: domainanalytics.BreakdownUnsetBucket, Total: 3, UniqueUsers: 2},
		},
		breakdownOther: &domainanalytics.BreakdownBucket{Total: 7, UniqueUsers: 5},
	}
	res, err := newQueryForTest(spy).QueryBreakdown(ctx, BreakdownCommand{
		ProjectID: "p1", Name: "level_complete", PropKey: "channel",
		PeriodStart: start, PeriodEnd: end})
	require.NoError(t, err)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.Source)
	require.Len(t, res.Buckets, 3)
	require.Equal(t, "wechat", res.Buckets[0].Value)
	require.Equal(t, domainanalytics.BreakdownOtherBucket, res.Buckets[2].Value)
	require.Equal(t, int64(7), res.Buckets[2].Total)

	// 无剩余桶：不追加 __other__。
	spy = &queryRepoSpy{breakdownTop: []domainanalytics.BreakdownBucket{{Value: "a", Total: 1}}}
	res, err = newQueryForTest(spy).QueryBreakdown(ctx, BreakdownCommand{
		ProjectID: "p1", Name: "e", PropKey: "k", PeriodStart: start, PeriodEnd: end})
	require.NoError(t, err)
	require.Len(t, res.Buckets, 1)
}

// TestQueryRetention_EmptyMatrix：基座表空（PR5 未合入）返回空矩阵不报错。
func TestQueryRetention_EmptyMatrix(t *testing.T) {
	spy := &queryRepoSpy{}
	res, err := newQueryForTest(spy).QueryRetention(context.Background(), RetentionCommand{
		ProjectID: "p1", CohortStart: guardBase, CohortEnd: guardBase.Add(24 * time.Hour)})
	require.NoError(t, err)
	require.Empty(t, res.Cohorts)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.Source)
}

// TestUserEventCursorCodec：keyset 游标编解码（非法输入拒绝）。
func TestUserEventCursorCodec(t *testing.T) {
	at := time.Date(2026, 9, 11, 1, 2, 3, 456789012, time.UTC)
	token, err := encodeUserEventCursor(domainanalytics.UserEventCursor{OccurredAt: at, ID: 99})
	require.NoError(t, err)
	got, err := decodeUserEventCursor(token)
	require.NoError(t, err)
	require.Equal(t, at, got.OccurredAt)
	require.Equal(t, int64(99), got.ID)

	for _, bad := range []string{"", "###", "e30=", "eyJ0IjowLCJpIjotMX0="} {
		_, err := decodeUserEventCursor(bad)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "token=%q", bad)
	}
}

// TestDayWindowBounds：UTC 日桶对齐（含非零点进位）。
func TestDayWindowBounds(t *testing.T) {
	start := time.Date(2026, 9, 10, 5, 30, 0, 0, time.UTC)
	end := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	dayStart, dayEnd := dayWindowBounds(start, end)
	require.Equal(t, time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC), dayStart)
	require.Equal(t, time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), dayEnd)
	require.Equal(t, 2, expectedDays(dayStart, dayEnd))

	end = time.Date(2026, 9, 12, 0, 0, 1, 0, time.UTC)
	_, dayEnd = dayWindowBounds(start, end)
	require.Equal(t, time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC), dayEnd, "非零点 end 进位到次日")
	require.Equal(t, 3, expectedDays(dayStart, dayEnd))
}
