// 外部测试包（analytics_test）：查询面端到端集成（PR3，docs/design/
// analytics-execution-plan.md「PR3 — 查询面」验收）。复用 ingest_integration_test
// 的生产同构装配（真实 Postgres 项目 schema + 真实拦截器链 + API key 凭证），
// 断言覆盖：
//   - 护栏矩阵经真 handler 全部 InvalidArgument；
//   - 覆盖检测回退：daily 空 → DAY 趋势回退 raw 且 source=raw；daily 全覆盖
//     → rollup 口径（daily/user_days/first_seen 手工种子，PR5 worker 未合入
//     的常态即空表回退路径）；
//   - Breakdown：Top-N + (unset) + __other__ 归并（含引号等特殊维度值）；
//   - Retention：first_seen ⋈ user_days 手算一致；基座表空返回空矩阵；
//   - ListUserEvents：keyset 分页（occurred_at+id 游标）无重无漏；
//   - 注入面：恶意事件名/prop_key 用例层拒绝且表完好；
//   - 审计读动词归类：六查询调用零 audit_logs 行（写动词对照 +1）。
package analytics_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/api/servergrpc"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/pkg/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// seedEvent 是 raw 直插种子行（绕过摄入链以精确控制 occurred_at 与归属；
// 摄入链自身的校验/钳制语义已由 PR2 集成测试覆盖）。
type seedEvent struct {
	name     string
	userID   string
	at       time.Time
	propsSQL string // JSON 字符串（'' = '{}'）
}

// seedRawEvents 单条多 VALUES 直插项目 schema.analytics_events。
func (e *ingestTestEnv) seedRawEvents(t *testing.T, rows []seedEvent) {
	t.Helper()
	if len(rows) == 0 {
		return
	}
	values := ""
	args := make([]any, 0, len(rows)*4)
	for _, r := range rows {
		if values != "" {
			values += ","
		}
		values += "(?, ?, 'client', '', ?, ?::jsonb)"
		props := r.propsSQL
		if props == "" {
			props = "{}"
		}
		args = append(args, r.name, r.userID, r.at, props)
	}
	_, err := e.db.ExecContext(e.ctx,
		fmt.Sprintf(`INSERT INTO %s.analytics_events (name, user_id, source, app_version, occurred_at, props) VALUES %s`, e.schema, values),
		args...)
	require.NoError(t, err)
}

func (e *ingestTestEnv) insertDailyRows(t *testing.T, rows []struct {
	day   time.Time
	name  string
	total int64
	uv    int64
}) {
	t.Helper()
	for _, r := range rows {
		_, err := e.db.ExecContext(e.ctx,
			fmt.Sprintf(`INSERT INTO %s.analytics_daily (day, name, total, unique_users) VALUES (?, ?, ?, ?)`, e.schema),
			r.day, r.name, r.total, r.uv)
		require.NoError(t, err)
	}
}

func (e *ingestTestEnv) insertUserDayRows(t *testing.T, rows []struct {
	userID string
	day    time.Time
	events int64
}) {
	t.Helper()
	for _, r := range rows {
		_, err := e.db.ExecContext(e.ctx,
			fmt.Sprintf(`INSERT INTO %s.analytics_user_days (user_id, day, events) VALUES (?, ?, ?)`, e.schema),
			r.userID, r.day, r.events)
		require.NoError(t, err)
	}
}

func (e *ingestTestEnv) insertFirstSeenRows(t *testing.T, rows []struct {
	userID   string
	firstDay time.Time
	lastDay  time.Time
}) {
	t.Helper()
	for _, r := range rows {
		_, err := e.db.ExecContext(e.ctx,
			fmt.Sprintf(`INSERT INTO %s.analytics_user_first_seen (user_id, first_day, last_day) VALUES (?, ?, ?)`, e.schema),
			r.userID, r.firstDay, r.lastDay)
		require.NoError(t, err)
	}
}

func (e *ingestTestEnv) insertDefinitionRows(t *testing.T, names []string, lastSeen time.Time) {
	t.Helper()
	for i, name := range names {
		_, err := e.db.ExecContext(e.ctx,
			fmt.Sprintf(`INSERT INTO %s.analytics_event_definitions (name, first_seen, last_seen) VALUES (?, ?, ?)`, e.schema),
			name, lastSeen.Add(-time.Duration(len(names)-i)*time.Hour), lastSeen.Add(-time.Duration(len(names)-i)*time.Hour))
		require.NoError(t, err)
	}
}

// apiKeyMD 创建带给定 scopes 的 API key 凭证（查询面 = analytics:read）。
func (e *ingestTestEnv) apiKeyMD(t *testing.T, scopes ...string) metadata.MD {
	t.Helper()
	secret, dropKey := testutil.CreateTestAPIKey(e.ctx, e.db, e.projectID, scopes)
	t.Cleanup(dropKey)
	return metadata.Pairs("x-api-key", secret)
}

// runServerQuery 把查询 RPC 挂进生产同构拦截器链（clientInfo → auth →
// rate limit → audit）调用真 servergrpc handler。
func (e *ingestTestEnv) runServerQuery(t *testing.T, method string, md metadata.MD, call func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error)) (any, error) {
	t.Helper()
	handler := servergrpc.NewAnalyticsService(e.ingest, e.query)
	var resp any
	err := e.env.InvokeUnaryHandler(e.ctx, method, md, func(ctx context.Context, _ any) (any, error) {
		var inErr error
		resp, inErr = call(ctx, handler)
		return resp, inErr
	})
	return resp, err
}

func dayUTC(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

// TestQueryIntegration_TimeseriesDayFallbackRaw（PR3 验收：覆盖回退）：
// daily 表空（PR5 worker 未合入的常态）→ DAY 趋势回退 raw 扫描，
// source=raw，零桶补齐、每桶 total/UV 与手工种子一致。
func TestQueryIntegration_TimeseriesDayFallbackRaw(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	day0 := dayUTC(time.Now().Add(-48 * time.Hour))
	day1 := day0.Add(24 * time.Hour)
	day2 := day0.Add(48 * time.Hour)
	e.seedRawEvents(t, []seedEvent{
		{name: "level_complete", userID: "u1", at: day0.Add(10 * time.Hour)},
		{name: "level_complete", userID: "u1", at: day0.Add(11 * time.Hour)},
		{name: "level_complete", userID: "u2", at: day0.Add(12 * time.Hour)},
		{name: "ad_watch", userID: "u3", at: day1.Add(9 * time.Hour)},
		{name: "level_complete", userID: "u1", at: day2.Add(8 * time.Hour)},
		{name: "level_complete", userID: "u4", at: day2.Add(9 * time.Hour)},
	})

	start := day0
	end := day2.Add(24 * time.Hour)
	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				PeriodStart: timestamppb.New(start),
				PeriodEnd:   timestamppb.New(end),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY,
			})
		})
	require.NoError(t, err)
	res := respAny.(*serverv1.QueryTimeseriesResponse)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.GetSource(), "daily 表空必须回退 raw 口径")
	require.Len(t, res.GetPoints(), 3, "DAY 零桶补齐为 3 个日桶")
	type pt struct {
		total, uv int64
	}
	got := map[string]pt{}
	for _, p := range res.GetPoints() {
		got[p.GetBucket().AsTime().Format("2006-01-02")] = pt{p.GetTotal(), p.GetUniqueUsers()}
	}
	require.Equal(t, pt{3, 2}, got[day0.Format("2006-01-02")], "day0: 3 事件 / {u1,u2}=2 UV")
	require.Equal(t, pt{1, 1}, got[day1.Format("2006-01-02")], "day1: 1 事件 / 1 UV")
	require.Equal(t, pt{2, 2}, got[day2.Format("2006-01-02")], "day2: 2 事件 / 2 UV")

	// 单事件名同样回退 raw 且数值一致（只数 level_complete）。
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				Names:       []string{"level_complete"},
				PeriodStart: timestamppb.New(start),
				PeriodEnd:   timestamppb.New(end),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY,
			})
		})
	require.NoError(t, err)
	res = respAny.(*serverv1.QueryTimeseriesResponse)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.GetSource())
	require.Equal(t, int64(3), res.GetPoints()[0].GetTotal())
	require.Equal(t, int64(2), res.GetPoints()[0].GetUniqueUsers())
	require.Equal(t, int64(2), res.GetPoints()[2].GetTotal())
}

// TestQueryIntegration_TimeseriesHourRaw（PR3 验收：HOUR 恒 raw）。
func TestQueryIntegration_TimeseriesHourRaw(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	base := dayUTC(time.Now()).Add(10 * time.Hour)
	e.seedRawEvents(t, []seedEvent{
		{name: "evt", userID: "u1", at: base.Add(30 * time.Minute)},
		{name: "evt", userID: "u1", at: base.Add(45 * time.Minute)},
		{name: "evt", userID: "u2", at: base.Add(2 * time.Hour)},
	})

	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				PeriodStart: timestamppb.New(base),
				PeriodEnd:   timestamppb.New(base.Add(3 * time.Hour)),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_HOUR,
			})
		})
	require.NoError(t, err)
	res := respAny.(*serverv1.QueryTimeseriesResponse)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.GetSource(), "HOUR 恒 raw（无小时聚合表，D6）")
	require.Len(t, res.GetPoints(), 3, "小时零桶补齐")
	require.Equal(t, int64(2), res.GetPoints()[0].GetTotal())
	require.Equal(t, int64(1), res.GetPoints()[0].GetUniqueUsers())
	require.Equal(t, int64(0), res.GetPoints()[1].GetTotal(), "空桶补零")
	require.Equal(t, int64(1), res.GetPoints()[2].GetTotal())
}

// TestQueryIntegration_TimeseriesRollupCovered（rollup 覆盖路径对真 PG）：
// daily 全覆盖 + 单名 → daily 直读（total/UV 精确）；全事件 → daily 总量 +
// user_days 并集 UV；多事件名 → 并集 UV 不可导出恒 raw。
func TestQueryIntegration_TimeseriesRollupCovered(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	day0 := dayUTC(time.Now().Add(-24 * time.Hour))
	day1 := dayUTC(time.Now())
	e.insertDailyRows(t, []struct {
		day   time.Time
		name  string
		total int64
		uv    int64
	}{
		{day0, "level_complete", 3, 2},
		{day0, "ad_watch", 2, 2},
		{day1, "level_complete", 4, 2},
	})
	e.insertUserDayRows(t, []struct {
		userID string
		day    time.Time
		events int64
	}{
		{"u1", day0, 2}, {"u2", day0, 1}, {"u3", day0, 1}, // day0 UV=3
		{"u1", day1, 2}, {"u4", day1, 1}, // day1 UV=2
	})

	start, end := day0, day1.Add(24*time.Hour)

	// 单名 rollup：daily 直读。
	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				Names:       []string{"level_complete"},
				PeriodStart: timestamppb.New(start),
				PeriodEnd:   timestamppb.New(end),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY,
			})
		})
	require.NoError(t, err)
	res := respAny.(*serverv1.QueryTimeseriesResponse)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.GetSource())
	require.Len(t, res.GetPoints(), 2)
	require.Equal(t, int64(3), res.GetPoints()[0].GetTotal())
	require.Equal(t, int64(2), res.GetPoints()[0].GetUniqueUsers())
	require.Equal(t, int64(4), res.GetPoints()[1].GetTotal())

	// 全事件 rollup：daily 总量 + user_days 并集 UV。
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				PeriodStart: timestamppb.New(start),
				PeriodEnd:   timestamppb.New(end),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY,
			})
		})
	require.NoError(t, err)
	res = respAny.(*serverv1.QueryTimeseriesResponse)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.GetSource())
	require.Equal(t, int64(5), res.GetPoints()[0].GetTotal())
	require.Equal(t, int64(3), res.GetPoints()[0].GetUniqueUsers(), "并集 UV 来自 user_days")
	require.Equal(t, int64(2), res.GetPoints()[1].GetUniqueUsers())

	// 多事件名：恒 raw（并集 UV 不可从 rollup 导出）。
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				Names:       []string{"level_complete", "ad_watch"},
				PeriodStart: timestamppb.New(start),
				PeriodEnd:   timestamppb.New(end),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY,
			})
		})
	require.NoError(t, err)
	res = respAny.(*serverv1.QueryTimeseriesResponse)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.GetSource(), "多事件名必须标注 raw 口径")
}

// TestQueryIntegration_GuardMatrixViaHandler（PR3 验收：护栏矩阵）：
// 超窗/坏游标经真 handler 全部 InvalidArgument（护栏先于触库的零仓储调用
// 断言由用例层 spy 测试 TestQueryGuards_RejectWithoutRepoAccess 承担）。
func TestQueryIntegration_GuardMatrixViaHandler(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	base := dayUTC(time.Now())
	cases := []struct {
		name   string
		method string
		call   func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error)
	}{
		{"overview window 367d", testutil.MethodAnalyticsGetOverview,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.GetOverview(ctx, &serverv1.GetAnalyticsOverviewRequest{
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(367 * 24 * time.Hour))})
			}},
		{"timeseries hour window 8d", testutil.MethodAnalyticsQueryTimeseries,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(8 * 24 * time.Hour)),
					Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_HOUR})
			}},
		{"timeseries day window 367d", testutil.MethodAnalyticsQueryTimeseries,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(367 * 24 * time.Hour)),
					Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY})
			}},
		{"timeseries unspecified granularity", testutil.MethodAnalyticsQueryTimeseries,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(time.Hour))})
			}},
		{"breakdown window 31d", testutil.MethodAnalyticsQueryBreakdown,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.QueryBreakdown(ctx, &serverv1.QueryBreakdownRequest{
					Name: "evt", PropKey: "channel",
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(31 * 24 * time.Hour))})
			}},
		{"breakdown top_n 51", testutil.MethodAnalyticsQueryBreakdown,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.QueryBreakdown(ctx, &serverv1.QueryBreakdownRequest{
					Name: "evt", PropKey: "channel",
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(time.Hour)), TopN: 51})
			}},
		{"retention window 93d", testutil.MethodAnalyticsQueryRetention,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.QueryRetention(ctx, &serverv1.QueryRetentionRequest{
					CohortStart: timestamppb.New(base),
					CohortEnd:   timestamppb.New(base.Add(93 * 24 * time.Hour))})
			}},
		{"user events window 93d", testutil.MethodAnalyticsListUserEvents,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.ListUserEvents(ctx, &serverv1.ListUserEventsRequest{
					UserId:      "u1",
					PeriodStart: timestamppb.New(base),
					PeriodEnd:   timestamppb.New(base.Add(93 * 24 * time.Hour))})
			}},
		{"user events bad page token", testutil.MethodAnalyticsListUserEvents,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.ListUserEvents(ctx, &serverv1.ListUserEventsRequest{
					UserId: "u1", PageToken: "!!!not-base64!!!"})
			}},
		{"definitions bad page token", testutil.MethodAnalyticsListDefinitions,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.ListEventDefinitions(ctx, &serverv1.ListEventDefinitionsRequest{
					PageToken: "%%%invalid%%%"})
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.runServerQuery(t, tc.method, md, tc.call)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "err=%v", err)
		})
	}

	// scope 门：无 analytics scope 的 API key 被策略拒绝（PermissionDenied）。
	otherKey := e.apiKeyMD(t, "users")
	_, err := e.runServerQuery(t, testutil.MethodAnalyticsGetOverview, otherKey,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.GetOverview(ctx, &serverv1.GetAnalyticsOverviewRequest{
				PeriodStart: timestamppb.New(base),
				PeriodEnd:   timestamppb.New(base.Add(time.Hour))})
		})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "无 analytics:read scope 必须被拒")
}

// TestQueryIntegration_BreakdownTopNOtherUnset（PR3 验收）：
// Top-N 排序 + (unset) 桶 + __other__ 精确归并（含引号等特殊维度值）。
func TestQueryIntegration_BreakdownTopNOtherUnset(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	at := dayUTC(time.Now()).Add(8 * time.Hour)
	rows := []seedEvent{
		// wx=3（u1×2+u2）、tt=2、无 channel 键=2 → (unset)。
		{name: "level_complete", userID: "u1", at: at, propsSQL: `{"channel":"wx"}`},
		{name: "level_complete", userID: "u1", at: at, propsSQL: `{"channel":"wx"}`},
		{name: "level_complete", userID: "u2", at: at, propsSQL: `{"channel":"wx"}`},
		{name: "level_complete", userID: "u3", at: at, propsSQL: `{"channel":"tt"}`},
		{name: "level_complete", userID: "u4", at: at, propsSQL: `{"channel":"tt"}`},
		{name: "level_complete", userID: "u5", at: at, propsSQL: `{"other_key":1}`},
		{name: "level_complete", userID: "u6", at: at, propsSQL: `{}`},
	}
	// 12 个长尾值（各 1 事件 1 用户）+ 1 个含引号值 → __other__ = 13。
	for i := 0; i < 12; i++ {
		rows = append(rows, seedEvent{name: "level_complete", userID: fmt.Sprintf("u7_%02d", i),
			at: at, propsSQL: fmt.Sprintf(`{"channel":"v%02d"}`, i)})
	}
	rows = append(rows, seedEvent{name: "level_complete", userID: "u8_quote", at: at,
		propsSQL: `{"channel":"he said \"hi\""}`})
	e.seedRawEvents(t, rows)

	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsQueryBreakdown, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryBreakdown(ctx, &serverv1.QueryBreakdownRequest{
				Name:        "level_complete",
				PropKey:     "channel",
				PeriodStart: timestamppb.New(dayUTC(time.Now())),
				PeriodEnd:   timestamppb.New(dayUTC(time.Now()).Add(24 * time.Hour)),
				TopN:        3,
			})
		})
	require.NoError(t, err)
	res := respAny.(*serverv1.QueryBreakdownResponse)
	require.Equal(t, domainanalytics.QuerySourceRaw, res.GetSource(), "breakdown v1 恒 raw（D9）")
	// Top-3 = wx(3) > (unset)(2) = tt(2)（val ASC 决胜：'(' < 't'）。
	require.Len(t, res.GetBuckets(), 4, "3 个 Top 桶 + __other__")
	require.Equal(t, "wx", res.GetBuckets()[0].GetValue())
	require.Equal(t, int64(3), res.GetBuckets()[0].GetTotal())
	require.Equal(t, int64(2), res.GetBuckets()[0].GetUniqueUsers())
	require.Equal(t, domainanalytics.BreakdownUnsetBucket, res.GetBuckets()[1].GetValue(), "缺失维度值归 (unset) 桶")
	require.Equal(t, int64(2), res.GetBuckets()[1].GetTotal())
	require.Equal(t, "tt", res.GetBuckets()[2].GetValue())
	require.Equal(t, domainanalytics.BreakdownOtherBucket, res.GetBuckets()[3].GetValue())
	require.Equal(t, int64(13), res.GetBuckets()[3].GetTotal(), "长尾 12 + 引号值 1 归并")
	require.Equal(t, int64(13), res.GetBuckets()[3].GetUniqueUsers(), "__other__ UV 必须整体去重（非差值）")

	// 注入面（真库上验证）：恶意 prop_key / 事件名 → InvalidArgument，
	// 且表完好可查（无注入生效）。
	_, err = e.runServerQuery(t, testutil.MethodAnalyticsQueryBreakdown, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryBreakdown(ctx, &serverv1.QueryBreakdownRequest{
				Name: "level_complete", PropKey: `x') OR ('1'='1`,
				PeriodStart: timestamppb.New(dayUTC(time.Now())),
				PeriodEnd:   timestamppb.New(dayUTC(time.Now()).Add(time.Hour))})
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "恶意 prop_key 必须被白名单拒绝")
	_, err = e.runServerQuery(t, testutil.MethodAnalyticsQueryBreakdown, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryBreakdown(ctx, &serverv1.QueryBreakdownRequest{
				Name: `evt'; DROP TABLE analytics_events;--`, PropKey: "channel",
				PeriodStart: timestamppb.New(dayUTC(time.Now())),
				PeriodEnd:   timestamppb.New(dayUTC(time.Now()).Add(time.Hour))})
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "恶意事件名必须被白名单拒绝")
	require.Equal(t, len(rows), e.countEvents(t, ""), "注入尝试后表完好（行数不变）")
}

// TestQueryIntegration_RetentionHandComputed（PR3 验收）：
// first_seen ⋈ user_days 矩阵与手算一致；基座表空返回空矩阵不报错。
func TestQueryIntegration_RetentionHandComputed(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	// 基座表空（PR5 未合入常态）：空矩阵 + 恒 rollup 标注。
	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsQueryRetention, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryRetention(ctx, &serverv1.QueryRetentionRequest{
				CohortStart: timestamppb.New(dayUTC(time.Now().Add(-48 * time.Hour))),
				CohortEnd:   timestamppb.New(dayUTC(time.Now()))})
		})
	require.NoError(t, err)
	res := respAny.(*serverv1.QueryRetentionResponse)
	require.Empty(t, res.GetCohorts(), "基座表空必须返回空矩阵而非报错")
	require.Equal(t, domainanalytics.QuerySourceRollup, res.GetSource())

	// 手工种子（D11 自连接口径）：
	//   u1 首见 D0，活跃 D0/D1/D3；u2 首见 D0，活跃 D0；u3 首见 D1，活跃 D1/D2。
	day0 := dayUTC(time.Now().Add(-72 * time.Hour))
	day1 := day0.Add(24 * time.Hour)
	day2 := day0.Add(48 * time.Hour)
	day3 := day0.Add(72 * time.Hour)
	e.insertFirstSeenRows(t, []struct {
		userID   string
		firstDay time.Time
		lastDay  time.Time
	}{
		{"u1", day0, day3}, {"u2", day0, day0}, {"u3", day1, day2},
	})
	e.insertUserDayRows(t, []struct {
		userID string
		day    time.Time
		events int64
	}{
		{"u1", day0, 2}, {"u1", day1, 1}, {"u1", day3, 1},
		{"u2", day0, 1},
		{"u3", day1, 1}, {"u3", day2, 1},
	})

	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsQueryRetention, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryRetention(ctx, &serverv1.QueryRetentionRequest{
				CohortStart: timestamppb.New(day0),
				CohortEnd:   timestamppb.New(day2)}) // 窗 [D0, D2)：cohort = D0, D1
		})
	require.NoError(t, err)
	res = respAny.(*serverv1.QueryRetentionResponse)
	require.Len(t, res.GetCohorts(), 2)
	require.Equal(t, domainanalytics.QuerySourceRollup, res.GetSource())

	byCohort := map[string]*serverv1.AnalyticsRetentionCohort{}
	for _, c := range res.GetCohorts() {
		byCohort[c.GetCohort().AsTime().Format("2006-01-02")] = c
	}
	c0 := byCohort[day0.Format("2006-01-02")]
	require.NotNil(t, c0, "D0 cohort 缺失")
	require.Equal(t, int64(2), c0.GetSize(), "D0 cohort = {u1,u2}")
	require.Len(t, c0.GetRetained(), domainanalytics.RetentionSlots, "retained 恒 D0–D14")
	require.Equal(t, []int64{2, 1, 0, 1,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, c0.GetRetained(),
		"D0=2, D1=u1, D2=0, D3=u1")
	c1 := byCohort[day1.Format("2006-01-02")]
	require.NotNil(t, c1, "D1 cohort 缺失")
	require.Equal(t, int64(1), c1.GetSize(), "D1 cohort = {u3}")
	require.Equal(t, []int64{1, 1,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, c1.GetRetained(),
		"D0=1, D1=u3 次日活跃")
}

// TestQueryIntegration_ListUserEventsKeyset（PR3 验收）：
// keyset 分页（occurred_at+id 游标）跨页无重无漏；含同刻事件决胜序；
// 窗口过滤与 props 回传。
func TestQueryIntegration_ListUserEventsKeyset(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	base := dayUTC(time.Now()).Add(-24 * time.Hour)
	// 7 事件：e4/e5 同刻（决胜序 = id DESC）；e1 带多类型 props。
	tie := base.Add(2 * time.Hour)
	rows := []seedEvent{
		{name: "level_complete", userID: "u-page", at: base.Add(5 * time.Hour), propsSQL: `{"idx":1,"tag":"wx"}`},
		{name: "ad_watch", userID: "u-page", at: base.Add(4 * time.Hour)},
		{name: "level_complete", userID: "u-page", at: base.Add(3 * time.Hour)},
		{name: "level_complete", userID: "u-page", at: tie},
		{name: "level_complete", userID: "u-page", at: tie},
		{name: "ad_watch", userID: "u-page", at: base.Add(1 * time.Hour)},
		{name: "level_complete", userID: "u-page", at: base},
	}
	e.seedRawEvents(t, rows)

	// PG 端真值序（occurred_at DESC, id DESC）作为分页完备性的对照。
	var wantOrder []int64
	truthRows, err := e.db.QueryContext(e.ctx, fmt.Sprintf(
		`SELECT id FROM %s.analytics_events WHERE user_id = 'u-page' ORDER BY occurred_at DESC, id DESC`, e.schema))
	require.NoError(t, err)
	for truthRows.Next() {
		var id int64
		require.NoError(t, truthRows.Scan(&id))
		wantOrder = append(wantOrder, id)
	}
	require.NoError(t, truthRows.Err())
	require.NoError(t, truthRows.Close())
	require.Len(t, wantOrder, len(rows))

	// 页大小 3 走完全部页，收集 id 序列。
	var (
		gotOrder []int64
		token    string
		pages    int
	)
	for {
		req := &serverv1.ListUserEventsRequest{UserId: "u-page", PageSize: 3, PageToken: token}
		respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsListUserEvents, md,
			func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
				return h.ListUserEvents(ctx, req)
			})
		require.NoError(t, err)
		res := respAny.(*serverv1.ListUserEventsResponse)
		require.Equal(t, domainanalytics.QuerySourceRaw, res.GetSource())
		require.LessOrEqual(t, len(res.GetEvents()), 3)
		for _, ev := range res.GetEvents() {
			gotOrder = append(gotOrder, ev.GetId())
		}
		pages++
		token = res.GetMeta().GetNextPageToken()
		if token == "" {
			break
		}
		require.LessOrEqual(t, pages, 10, "分页发散保护")
	}
	require.Equal(t, 3, pages, "7 行 / 页 3 → 3 页")
	require.Equal(t, wantOrder, gotOrder, "keyset 分页必须与 SQL 全序一致（无重无漏，含同刻决胜序）")
	require.ElementsMatch(t, wantOrder, gotOrder)

	// 窗口过滤：[base+1h30m, base+4h30m) → e2/e3/tie×2 共 4 行。
	var ids []int64
	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsListUserEvents, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListUserEvents(ctx, &serverv1.ListUserEventsRequest{
				UserId:      "u-page",
				PeriodStart: timestamppb.New(base.Add(90 * time.Minute)),
				PeriodEnd:   timestamppb.New(base.Add(270 * time.Minute)),
				PageSize:    10,
			})
		})
	require.NoError(t, err)
	res := respAny.(*serverv1.ListUserEventsResponse)
	require.Len(t, res.GetEvents(), 4)
	for _, ev := range res.GetEvents() {
		ids = append(ids, ev.GetId())
	}
	require.ElementsMatch(t, wantOrder[1:5], ids, "窗口内 = 全序第 2–5 行")

	// props 回传（数字/字符串类型保真）：无窗口全量取一页找 idx 事件。
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsListUserEvents, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListUserEvents(ctx, &serverv1.ListUserEventsRequest{UserId: "u-page", PageSize: 10})
		})
	require.NoError(t, err)
	res = respAny.(*serverv1.ListUserEventsResponse)
	require.Len(t, res.GetEvents(), len(rows))
	var found bool
	for _, ev := range res.GetEvents() {
		if ev.GetProps().GetFields()["idx"] != nil {
			require.Equal(t, 1.0, ev.GetProps().GetFields()["idx"].GetNumberValue())
			require.Equal(t, "wx", ev.GetProps().GetFields()["tag"].GetStringValue())
			found = true
		}
	}
	require.True(t, found, "带 props 的事件应完整回传结构化字段")

	// 其他用户零泄漏。
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsListUserEvents, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListUserEvents(ctx, &serverv1.ListUserEventsRequest{UserId: "u-other", PageSize: 10})
		})
	require.NoError(t, err)
	res = respAny.(*serverv1.ListUserEventsResponse)
	require.Empty(t, res.GetEvents())
}

// TestQueryIntegration_GetOverview（PR3 验收）：
// daily 空 → KPI/Top 回退 raw（source=raw）；daily 全覆盖 → rollup 口径
// （UV 取 user_days 跨日去重、新增取 first_seen）；今日块恒 raw 直读。
func TestQueryIntegration_GetOverview(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	day0 := dayUTC(time.Now().Add(-24 * time.Hour))
	day1 := dayUTC(time.Now())

	// —— raw 回退路径（daily 空） ——
	e.seedRawEvents(t, []seedEvent{
		{name: "level_complete", userID: "u1", at: day0.Add(8 * time.Hour)},
		{name: "level_complete", userID: "u1", at: day0.Add(9 * time.Hour)},
		{name: "ad_watch", userID: "u2", at: day0.Add(10 * time.Hour)},
		{name: "level_complete", userID: "u3", at: day1.Add(1 * time.Hour)},
		{name: "level_complete", userID: "u4", at: day1.Add(2 * time.Hour)},
	})
	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsGetOverview, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.GetOverview(ctx, &serverv1.GetAnalyticsOverviewRequest{
				PeriodStart: timestamppb.New(day0),
				PeriodEnd:   timestamppb.New(day1.Add(24 * time.Hour))})
		})
	require.NoError(t, err)
	raw := respAny.(*serverv1.AnalyticsOverview)
	require.Equal(t, domainanalytics.QuerySourceRaw, raw.GetSource(), "daily 空 → 回退 raw")
	require.Equal(t, int64(5), raw.GetKpi().GetTotalEvents())
	require.Equal(t, int64(4), raw.GetKpi().GetUniqueUsers(), "raw 口径 COUNT(DISTINCT user_id)")
	require.InDelta(t, 1.25, raw.GetKpi().GetEventsPerUser(), 1e-9)
	require.NotEmpty(t, raw.GetTopEvents())
	require.Equal(t, "level_complete", raw.GetTopEvents()[0].GetName())
	require.Equal(t, int64(4), raw.GetTopEvents()[0].GetTotal())
	require.Equal(t, int64(2), raw.GetToday().GetTotalEvents(), "今日块 = 今日 2 事件（raw 直读）")
	require.Equal(t, int64(2), raw.GetToday().GetUniqueUsers())
	require.Equal(t, int64(0), raw.GetKpi().GetNewUsers(), "first_seen 空 → 新增 0（诚实口径）")

	// —— rollup 覆盖路径 ——
	e.insertDailyRows(t, []struct {
		day   time.Time
		name  string
		total int64
		uv    int64
	}{
		{day0, "level_complete", 3, 2},
		{day0, "ad_watch", 2, 1},
		{day1, "level_complete", 4, 2},
	})
	e.insertUserDayRows(t, []struct {
		userID string
		day    time.Time
		events int64
	}{
		{"u1", day0, 2}, {"u2", day0, 1}, {"u3", day0, 1},
		{"u3", day1, 1}, {"u4", day1, 1}, {"u5", day1, 1},
	})
	e.insertFirstSeenRows(t, []struct {
		userID   string
		firstDay time.Time
		lastDay  time.Time
	}{
		{"u2", day0, day0}, {"u3", day0, day1}, {"u5", day1, day1},
	})
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsGetOverview, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.GetOverview(ctx, &serverv1.GetAnalyticsOverviewRequest{
				PeriodStart: timestamppb.New(day0),
				PeriodEnd:   timestamppb.New(day1.Add(24 * time.Hour))})
		})
	require.NoError(t, err)
	rollup := respAny.(*serverv1.AnalyticsOverview)
	require.Equal(t, domainanalytics.QuerySourceRollup, rollup.GetSource())
	require.Equal(t, int64(9), rollup.GetKpi().GetTotalEvents(), "3+2+4")
	require.Equal(t, int64(5), rollup.GetKpi().GetUniqueUsers(), "user_days 跨日去重 {u1..u5}")
	require.InDelta(t, 1.8, rollup.GetKpi().GetEventsPerUser(), 1e-9)
	require.Equal(t, int64(3), rollup.GetKpi().GetNewUsers(), "first_day ∈ 窗口 = 3")
	require.Equal(t, "level_complete", rollup.GetTopEvents()[0].GetName())
	require.Equal(t, int64(7), rollup.GetTopEvents()[0].GetTotal(), "3+4")
	require.Equal(t, int64(2), rollup.GetToday().GetTotalEvents(), "今日块恒 raw（不随窗口口径变化）")
}

// TestQueryIntegration_ListDefinitionsPaging（PR3 验收）：字典分页
// （last_seen DESC 活性序——total_30d 由 PR5 rollup 刷新，当前恒 0）。
func TestQueryIntegration_ListDefinitionsPaging(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read")

	seen := time.Now().UTC().Add(-time.Hour)
	e.insertDefinitionRows(t, []string{"evt_old", "evt_mid", "evt_new"}, seen)

	// 首页（page_size=2）。
	respAny, err := e.runServerQuery(t, testutil.MethodAnalyticsListDefinitions, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListEventDefinitions(ctx, &serverv1.ListEventDefinitionsRequest{PageSize: 2})
		})
	require.NoError(t, err)
	page1 := respAny.(*serverv1.ListEventDefinitionsResponse)
	require.Equal(t, int32(2), page1.GetMeta().GetPageSize())
	require.Equal(t, int32(3), page1.GetMeta().GetTotalCount())
	require.Len(t, page1.GetDefinitions(), 2)
	require.Equal(t, "evt_new", page1.GetDefinitions()[0].GetName(), "按 last_seen DESC 活性序")
	require.Equal(t, "evt_mid", page1.GetDefinitions()[1].GetName())
	require.NotEmpty(t, page1.GetMeta().GetNextPageToken())
	require.Empty(t, page1.GetMeta().GetPrevPageToken())
	require.Equal(t, int64(0), page1.GetDefinitions()[0].GetTotal_30D(), "total_30d 恒 0 直至 PR5 rollup")

	// 次页。
	respAny, err = e.runServerQuery(t, testutil.MethodAnalyticsListDefinitions, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListEventDefinitions(ctx, &serverv1.ListEventDefinitionsRequest{
				PageSize: 2, PageToken: page1.GetMeta().GetNextPageToken()})
		})
	require.NoError(t, err)
	page2 := respAny.(*serverv1.ListEventDefinitionsResponse)
	require.Len(t, page2.GetDefinitions(), 1)
	require.Equal(t, "evt_old", page2.GetDefinitions()[0].GetName())
	require.Empty(t, page2.GetMeta().GetNextPageToken(), "末页无续页 token")
	require.NotEmpty(t, page2.GetMeta().GetPrevPageToken())
}

// TestQueryIntegration_QueriesDoNotAudit（PR3 验收：审计读动词归类，
// S2 遗留接线项的端到端护栏）：六查询 RPC 经全拦截器链调用不产生
// audit_logs 行；写动词（拒收请求也记）对照 +1 证明审计通路健在。
func TestQueryIntegration_QueriesDoNotAudit(t *testing.T) {
	e := setupIngestEnv(t)
	md := e.apiKeyMD(t, "analytics.read", "assets.write")

	auditBefore, err := e.env.AuditLogCount(e.ctx)
	require.NoError(t, err)

	base := dayUTC(time.Now())
	// 六查询各调一次（护栏拒绝/成功皆可——读动词不落行）。
	_, _ = e.runServerQuery(t, testutil.MethodAnalyticsGetOverview, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.GetOverview(ctx, &serverv1.GetAnalyticsOverviewRequest{
				PeriodStart: timestamppb.New(base), PeriodEnd: timestamppb.New(base.Add(time.Hour))})
		})
	_, _ = e.runServerQuery(t, testutil.MethodAnalyticsListDefinitions, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListEventDefinitions(ctx, &serverv1.ListEventDefinitionsRequest{})
		})
	_, _ = e.runServerQuery(t, testutil.MethodAnalyticsQueryTimeseries, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryTimeseries(ctx, &serverv1.QueryTimeseriesRequest{
				PeriodStart: timestamppb.New(base), PeriodEnd: timestamppb.New(base.Add(time.Hour)),
				Granularity: serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY})
		})
	_, _ = e.runServerQuery(t, testutil.MethodAnalyticsQueryBreakdown, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryBreakdown(ctx, &serverv1.QueryBreakdownRequest{
				Name: "evt", PropKey: "channel",
				PeriodStart: timestamppb.New(base), PeriodEnd: timestamppb.New(base.Add(time.Hour))})
		})
	_, _ = e.runServerQuery(t, testutil.MethodAnalyticsQueryRetention, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.QueryRetention(ctx, &serverv1.QueryRetentionRequest{
				CohortStart: timestamppb.New(base), CohortEnd: timestamppb.New(base.Add(24 * time.Hour))})
		})
	_, _ = e.runServerQuery(t, testutil.MethodAnalyticsListUserEvents, md,
		func(ctx context.Context, h *servergrpc.AnalyticsService) (any, error) {
			return h.ListUserEvents(ctx, &serverv1.ListUserEventsRequest{UserId: "u1"})
		})

	auditAfterQueries, err := e.env.AuditLogCount(e.ctx)
	require.NoError(t, err)
	require.Equal(t, auditBefore, auditAfterQueries, "六个查询 RPC 是读动词，不得落 audit_logs 行")

	// 对照组：写动词（AssetsGrant，拒收请求也记审计）→ 恰好 +1。
	err = e.env.InvokeUnaryHandler(e.ctx, testutil.MethodAssetsGrant, md,
		func(context.Context, any) (any, error) {
			return nil, status.Error(codes.InvalidArgument, "guard probe")
		})
	require.Error(t, err)
	auditAfterWrite, err := e.env.AuditLogCount(e.ctx)
	require.NoError(t, err)
	require.Equal(t, auditBefore+1, auditAfterWrite, "审计通路必须健在（写动词 +1 行）")
}
