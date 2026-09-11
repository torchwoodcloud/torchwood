// 本文件是查询面用例（PR3，docs/design/analytics.md §4.1/§6）：六个固定形状
// 查询的窗口护栏（protovalidate 之上的跨字段规则）、rollup/raw 择路（覆盖
// 检测回退）、`source: rollup|raw` 口径标注（D8）与 keyset 游标编解码。
// 红线：查询 SQL 全参数化（prop_key/事件名白名单校验后绑定，仓储层执行）；
// 全部护栏在触库之前判定（超窗 InvalidArgument 不扫表）；注入面（事件名/
// prop_key）在用例层防御性复检（可被非 gRPC 链路复用）。
package analytics

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"time"

	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 概览 Top 事件行数（设计未定值；Console 概览卡片容量级）。
const overviewTopEventsLimit = 10

// ListUserEvents 缺省页大小（proto3 零值 = 未给出）。
const userEventsDefaultPageSize = 50

// Query 是查询面用例聚合。
type Query struct {
	repo domainanalytics.QueryRepository
	// retentionDays 是 raw 回退窗口护栏的保留期口径（PR5 起与 analytics
	// .retention_days 配置同源，裁剪与回退一致；缺省 DefaultRetentionDays）。
	retentionDays int
	now           func() time.Time
}

// NewQuery 构造用例（保留期取缺省值；组合根经 NewQueryFromConfig 注入配置
// 口径）。
func NewQuery(repo domainanalytics.QueryRepository) *Query {
	return &Query{repo: repo, retentionDays: domainanalytics.DefaultRetentionDays, now: time.Now}
}

// NewQueryFromConfig 组合根入口：raw 回退窗口护栏消费 analytics.retention_days
// （未配置/越界值经 NormalizeRetentionDays 归一，与保留期裁剪同口径）。
func NewQueryFromConfig(cfg *config.AppConfig, repo domainanalytics.QueryRepository) *Query {
	q := NewQuery(repo)
	if cfg != nil {
		q.retentionDays = domainanalytics.NormalizeRetentionDays(cfg.GetAnalytics().GetRetentionDays())
	}
	return q
}

// —— 命令/结果（传输层从 proto 装配，保持 gRPC 映射薄） ——

// OverviewCommand 是 GetOverview 请求。
type OverviewCommand struct {
	ProjectID   string
	PeriodStart time.Time
	PeriodEnd   time.Time
}

// OverviewResult 是 GetOverview 响应。
type OverviewResult struct {
	Kpi       domainanalytics.OverviewKpi
	TopEvents []domainanalytics.TopEvent
	Today     domainanalytics.TodayStats
	Source    string // rollup | raw（窗口 KPI/Top 口径；today 恒 raw）
}

// ListDefinitionsCommand 是 ListEventDefinitions 请求（分页参数经 pkg/crud
// 语义由传输层解析后仍以原始形态传入——offset token 校验属 crud，用例层
// 只透传，保持单一职责）。
type ListDefinitionsCommand struct {
	ProjectID string
	PageSize  int32
	PageToken string
}

// ListDefinitionsResult 是 ListEventDefinitions 响应。
type ListDefinitionsResult struct {
	Definitions   []domainanalytics.EventDefinitionInfo
	PageSize      int32
	TotalCount    int32
	NextPageToken string
	PrevPageToken string
}

// TimeseriesCommand 是 QueryTimeseries 请求。
type TimeseriesCommand struct {
	ProjectID   string
	Names       []string // ≤10（protovalidate）；空 = 全事件聚合
	PeriodStart time.Time
	PeriodEnd   time.Time
	Granularity domainanalytics.Granularity
}

// TimeseriesResult 是 QueryTimeseries 响应（Points 按桶时序升序、零桶补齐）。
type TimeseriesResult struct {
	Points []domainanalytics.TimeseriesPoint
	Source string
}

// BreakdownCommand 是 QueryBreakdown 请求。
type BreakdownCommand struct {
	ProjectID   string
	Name        string
	PropKey     string
	PeriodStart time.Time
	PeriodEnd   time.Time
	TopN        int32
}

// BreakdownResult 是 QueryBreakdown 响应。
type BreakdownResult struct {
	Buckets []domainanalytics.BreakdownBucket
	Source  string // 恒 raw（D9）
}

// RetentionCommand 是 QueryRetention 请求。
type RetentionCommand struct {
	ProjectID   string
	CohortStart time.Time
	CohortEnd   time.Time
}

// RetentionResult 是 QueryRetention 响应。
type RetentionResult struct {
	Cohorts []domainanalytics.RetentionCohort
	Source  string // 恒 rollup（user_days/first_seen 基座，D11）
}

// UserEventsCommand 是 ListUserEvents 请求。
type UserEventsCommand struct {
	ProjectID   string
	UserID      string
	PageSize    int32
	PageToken   string
	PeriodStart *time.Time
	PeriodEnd   *time.Time
}

// UserEventsResult 是 ListUserEvents 响应。
type UserEventsResult struct {
	Events        []domainanalytics.UserEvent
	PageSize      int32
	NextPageToken string
}

// —— 用例入口 ——

// GetOverview 窗口 KPI + Top 事件（覆盖检测择路）+ 今日实时数（恒 raw）。
func (u *Query) GetOverview(ctx context.Context, cmd OverviewCommand) (*OverviewResult, error) {
	if err := u.precheck(cmd.ProjectID); err != nil {
		return nil, err
	}
	if err := validatePeriod(cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.MaxOverviewWindow, "overview"); err != nil {
		return nil, err
	}
	dayStart, dayEnd := dayWindowBounds(cmd.PeriodStart, cmd.PeriodEnd)

	var (
		kpi      domainanalytics.OverviewKpi
		top      []domainanalytics.TopEvent
		source   string
		covered  bool
		coveredN int
		err      error
	)
	if coveredN, err = u.repo.DailyCoveredDays(ctx, cmd.ProjectID, dayStart, dayEnd); err != nil {
		return nil, err
	}
	covered = coveredN >= expectedDays(dayStart, dayEnd)

	if covered {
		// rollup 口径：total 从 daily 求和（精确）、UV 从 user_days 窗口去重
		//（精确——跨日不重复计数，不可按日求和）、Top 从 daily。
		daily, err := u.repo.DailySeries(ctx, cmd.ProjectID, "", dayStart, dayEnd)
		if err != nil {
			return nil, err
		}
		var total int64
		for _, d := range daily {
			total += d.Total
		}
		uv, err := u.repo.CountActiveUsers(ctx, cmd.ProjectID, dayStart, dayEnd)
		if err != nil {
			return nil, err
		}
		if top, err = u.repo.TopEventsFromDaily(ctx, cmd.ProjectID, dayStart, dayEnd, overviewTopEventsLimit); err != nil {
			return nil, err
		}
		kpi.TotalEvents = total
		kpi.UniqueUsers = uv
		source = domainanalytics.QuerySourceRollup
	} else {
		// 覆盖缺失（worker 未部署/停摆/零事件日）：窗口 ≤ raw 保留期才回退
		// raw 扫描；超窗明确报错（不静默返回残缺窗口）。
		if err := u.ensureRawFallbackWindow(dayStart, dayEnd); err != nil {
			return nil, err
		}
		total, uv, err := u.repo.RawOverviewKPI(ctx, cmd.ProjectID, cmd.PeriodStart, cmd.PeriodEnd)
		if err != nil {
			return nil, err
		}
		if top, err = u.repo.RawTopEvents(ctx, cmd.ProjectID, cmd.PeriodStart, cmd.PeriodEnd, overviewTopEventsLimit); err != nil {
			return nil, err
		}
		kpi.TotalEvents = total
		kpi.UniqueUsers = uv
		source = domainanalytics.QuerySourceRaw
	}

	// 新增用户恒 first_seen 表口径（cohort 锚点，D11）；表空（rollup 未跑）
	// = 0——与 source 标注共同构成诚实口径。
	newUsers, err := u.repo.CountNewUsers(ctx, cmd.ProjectID, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	kpi.NewUsers = newUsers
	kpi.EventsPerUser = eventsPerUser(kpi.TotalEvents, kpi.UniqueUsers)

	today, err := u.repo.RawTodayStats(ctx, cmd.ProjectID, u.now().UTC().Truncate(24*time.Hour))
	if err != nil {
		return nil, err
	}
	return &OverviewResult{Kpi: kpi, TopEvents: top, Today: today, Source: source}, nil
}

// ListEventDefinitions 字典分页（按 last_seen DESC, name ASC——total_30d
// 排序键由 rollup 维护，PR5 起生效；当前 last_seen 为最接近的活性序）。
func (u *Query) ListEventDefinitions(ctx context.Context, cmd ListDefinitionsCommand) (*ListDefinitionsResult, error) {
	if err := u.precheck(cmd.ProjectID); err != nil {
		return nil, err
	}
	limit := int(cmd.PageSize)
	if limit <= 0 {
		limit = userEventsDefaultPageSize
	}
	// page_token 是 pkg/crud 偏移 token；校验（签名/上限）属 crud 语义，
	// 此处按仓库既有模式在传输层完成——用例层收到已解码偏移。此处接受
	// 原始 token 并复用 crud 解码以保持传输层薄（见 servergrpc 装配）。
	offset, err := decodeDefinitionsOffset(cmd.PageToken)
	if err != nil {
		return nil, err
	}
	defs, total, err := u.repo.ListEventDefinitions(ctx, cmd.ProjectID, offset, limit)
	if err != nil {
		return nil, err
	}
	res := &ListDefinitionsResult{
		Definitions: defs,
		PageSize:    int32(limit),
		TotalCount:  int32(total),
	}
	if offset+limit < int(total) {
		if res.NextPageToken, err = encodeDefinitionsOffset(offset + limit); err != nil {
			return nil, err
		}
	}
	if offset > 0 {
		prev := offset - limit
		if prev < 0 {
			prev = 0
		}
		if res.PrevPageToken, err = encodeDefinitionsOffset(prev); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// QueryTimeseries 趋势查询：HOUR 恒 raw（≤7d）；DAY 优先 rollup，覆盖缺失
// 回退 raw（≤保留期）；多事件名并集 UV 不可从 rollup 导出 → 恒 raw。
func (u *Query) QueryTimeseries(ctx context.Context, cmd TimeseriesCommand) (*TimeseriesResult, error) {
	if err := u.precheck(cmd.ProjectID); err != nil {
		return nil, err
	}
	if cmd.Granularity == domainanalytics.GranularityUnspecified {
		return nil, status.Error(codes.InvalidArgument, "granularity is required")
	}
	// 注入面防御性复检：事件名白名单（与 Breakdown 同防线——传输层已有
	// protovalidate，非 gRPC 链路复用仍须用例层拦截；仓储侧值恒绑定参数，
	// 正则是形状纪律的第二道闸）。
	for _, n := range cmd.Names {
		if !domainanalytics.EventNamePattern.MatchString(n) {
			return nil, status.Error(codes.InvalidArgument, "invalid event name")
		}
	}
	if cmd.Granularity == domainanalytics.GranularityHour {
		if err := validatePeriod(cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.MaxTimeseriesHourWindow, "timeseries"); err != nil {
			return nil, err
		}
		points, err := u.repo.RawTimeseries(ctx, cmd.ProjectID, cmd.Names, cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.GranularityHour)
		if err != nil {
			return nil, err
		}
		return &TimeseriesResult{
			Points: fillHourBuckets(points, cmd.PeriodStart, cmd.PeriodEnd),
			Source: domainanalytics.QuerySourceRaw,
		}, nil
	}

	// DAY。
	if err := validatePeriod(cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.MaxTimeseriesDayWindow, "timeseries"); err != nil {
		return nil, err
	}
	dayStart, dayEnd := dayWindowBounds(cmd.PeriodStart, cmd.PeriodEnd)

	// 多事件名：并集 UV 不能由 daily/user_days 导出（跨名去重），恒 raw。
	if len(cmd.Names) > 1 {
		if err := u.ensureRawFallbackWindow(dayStart, dayEnd); err != nil {
			return nil, err
		}
		points, err := u.repo.RawTimeseries(ctx, cmd.ProjectID, cmd.Names, cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.GranularityDay)
		if err != nil {
			return nil, err
		}
		return &TimeseriesResult{
			Points: fillDayBuckets(points, dayStart, dayEnd),
			Source: domainanalytics.QuerySourceRaw,
		}, nil
	}

	coveredN, err := u.repo.DailyCoveredDays(ctx, cmd.ProjectID, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	if coveredN < expectedDays(dayStart, dayEnd) {
		// 覆盖缺失（worker 未部署/停摆/零事件日）→ 回退 raw（≤保留期）。
		if err := u.ensureRawFallbackWindow(dayStart, dayEnd); err != nil {
			return nil, err
		}
		points, err := u.repo.RawTimeseries(ctx, cmd.ProjectID, cmd.Names, cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.GranularityDay)
		if err != nil {
			return nil, err
		}
		return &TimeseriesResult{
			Points: fillDayBuckets(points, dayStart, dayEnd),
			Source: domainanalytics.QuerySourceRaw,
		}, nil
	}

	// rollup 口径。
	var (
		points []domainanalytics.TimeseriesPoint
		name   string
	)
	if len(cmd.Names) == 1 {
		name = cmd.Names[0]
	}
	daily, err := u.repo.DailySeries(ctx, cmd.ProjectID, name, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	if name != "" {
		// 单事件名：daily 行 total/unique_users 均精确。
		points = make([]domainanalytics.TimeseriesPoint, 0, len(daily))
		for _, d := range daily {
			points = append(points, domainanalytics.TimeseriesPoint{Bucket: d.Day, Total: d.Total, UniqueUsers: d.UniqueUsers})
		}
	} else {
		// 全事件：total 从 daily；并集 UV 从 user_days（按日去重精确）。
		uvs, err := u.repo.UserDaySeries(ctx, cmd.ProjectID, dayStart, dayEnd)
		if err != nil {
			return nil, err
		}
		uvByDay := make(map[time.Time]int64, len(uvs))
		for _, d := range uvs {
			uvByDay[d.Day] = d.UniqueUsers
		}
		points = make([]domainanalytics.TimeseriesPoint, 0, len(daily))
		for _, d := range daily {
			points = append(points, domainanalytics.TimeseriesPoint{Bucket: d.Day, Total: d.Total, UniqueUsers: uvByDay[d.Day]})
		}
	}
	return &TimeseriesResult{
		Points: fillDayBuckets(points, dayStart, dayEnd),
		Source: domainanalytics.QuerySourceRollup,
	}, nil
}

// QueryBreakdown 维度拆解（raw 直查 + Top-N + 应用层 __other__ 归并）。
func (u *Query) QueryBreakdown(ctx context.Context, cmd BreakdownCommand) (*BreakdownResult, error) {
	if err := u.precheck(cmd.ProjectID); err != nil {
		return nil, err
	}
	// 注入面防御性复检：白名单正则命中才允许进 SQL（经参数绑定；正则即
	// 沿 aggregateDocuments 的列名纪律——不拼值，只放行形状）。
	if !domainanalytics.EventNamePattern.MatchString(cmd.Name) {
		return nil, status.Error(codes.InvalidArgument, "invalid event name")
	}
	if !domainanalytics.PropKeyPattern.MatchString(cmd.PropKey) {
		return nil, status.Error(codes.InvalidArgument, "invalid prop_key")
	}
	if err := validatePeriod(cmd.PeriodStart, cmd.PeriodEnd, domainanalytics.MaxBreakdownWindow, "breakdown"); err != nil {
		return nil, err
	}
	topN := int(cmd.TopN)
	if topN <= 0 {
		topN = domainanalytics.BreakdownDefaultTopN
	}
	if topN > domainanalytics.BreakdownMaxTopN {
		return nil, status.Errorf(codes.InvalidArgument, "top_n must be <= %d", domainanalytics.BreakdownMaxTopN)
	}

	top, other, err := u.repo.RawBreakdown(ctx, cmd.ProjectID, cmd.Name, cmd.PropKey, cmd.PeriodStart, cmd.PeriodEnd, topN)
	if err != nil {
		return nil, err
	}
	buckets := top
	if other != nil && (other.Total > 0 || other.UniqueUsers > 0) {
		buckets = append(buckets, domainanalytics.BreakdownBucket{
			Value:       domainanalytics.BreakdownOtherBucket,
			Total:       other.Total,
			UniqueUsers: other.UniqueUsers,
		})
	}
	return &BreakdownResult{Buckets: buckets, Source: domainanalytics.QuerySourceRaw}, nil
}

// QueryRetention cohort × D0–D14 矩阵（first_seen ⋈ user_days 自连接，D11）。
// 基座表空（PR5 rollup 未合入）时返回空矩阵而非报错。
func (u *Query) QueryRetention(ctx context.Context, cmd RetentionCommand) (*RetentionResult, error) {
	if err := u.precheck(cmd.ProjectID); err != nil {
		return nil, err
	}
	if err := validatePeriod(cmd.CohortStart, cmd.CohortEnd, domainanalytics.MaxRetentionCohortWindow, "retention"); err != nil {
		return nil, err
	}
	dayStart, dayEnd := dayWindowBounds(cmd.CohortStart, cmd.CohortEnd)
	cohorts, err := u.repo.RetentionMatrix(ctx, cmd.ProjectID, dayStart, dayEnd)
	if err != nil {
		return nil, err
	}
	// 防御性归一：retained 恒 D0–D14（仓储契约之外的形状兜底）。
	for i := range cohorts {
		r := cohorts[i].Retained
		if len(r) == domainanalytics.RetentionSlots {
			continue
		}
		norm := make([]int64, domainanalytics.RetentionSlots)
		copy(norm, r)
		cohorts[i].Retained = norm
	}
	return &RetentionResult{Cohorts: cohorts, Source: domainanalytics.QuerySourceRollup}, nil
}

// ListUserEvents 用户行为轨迹（raw keyset 分页，(occurred_at, id) 游标）。
func (u *Query) ListUserEvents(ctx context.Context, cmd UserEventsCommand) (*UserEventsResult, error) {
	if err := u.precheck(cmd.ProjectID); err != nil {
		return nil, err
	}
	if cmd.UserID == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	if cmd.PeriodStart != nil && cmd.PeriodEnd != nil {
		if err := validatePeriod(*cmd.PeriodStart, *cmd.PeriodEnd, domainanalytics.MaxUserEventsWindow, "user_events"); err != nil {
			return nil, err
		}
	}
	limit := int(cmd.PageSize)
	if limit <= 0 {
		limit = userEventsDefaultPageSize
	}
	var cursor *domainanalytics.UserEventCursor
	if cmd.PageToken != "" {
		c, err := decodeUserEventCursor(cmd.PageToken)
		if err != nil {
			return nil, err
		}
		cursor = c
	}
	events, hasMore, err := u.repo.ListUserEvents(ctx, cmd.ProjectID, domainanalytics.UserEventsQuery{
		UserID:      cmd.UserID,
		PeriodStart: cmd.PeriodStart,
		PeriodEnd:   cmd.PeriodEnd,
		Cursor:      cursor,
		Limit:       limit,
	})
	if err != nil {
		return nil, err
	}
	res := &UserEventsResult{Events: events, PageSize: int32(limit)}
	if hasMore && len(events) > 0 {
		last := events[len(events)-1]
		next, err := encodeUserEventCursor(domainanalytics.UserEventCursor{OccurredAt: last.OccurredAt, ID: last.ID})
		if err != nil {
			return nil, err
		}
		res.NextPageToken = next
	}
	return res, nil
}

// —— 内部工具 ——

// precheck 是查询面公共前置：仓储装配 + 项目上下文（来自凭证，红线：
// 请求体不携带 project_id）。
func (u *Query) precheck(projectID string) error {
	if u.repo == nil {
		return status.Error(codes.Internal, "analytics query repository is not configured")
	}
	if projectID == "" {
		return status.Error(codes.Unauthenticated, "missing project context")
	}
	return nil
}

// validatePeriod 跨字段窗口校验：start < end 且窗宽 ≤ max（超窗 InvalidArgument，
// 先于任何触库调用执行）。
func validatePeriod(start, end time.Time, max time.Duration, subject string) error {
	if !start.Before(end) {
		return status.Errorf(codes.InvalidArgument, "%s: period_start must be before period_end", subject)
	}
	if end.Sub(start) > max {
		return status.Errorf(codes.InvalidArgument, "%s: window exceeds %s limit", subject, max)
	}
	return nil
}

// dayWindowBounds 把请求窗口对齐到 UTC 日桶：[start 当日零点, end 当日零点
// （非零点则进位）)——DAY 桶覆盖范围与覆盖检测的期望天数基数。
func dayWindowBounds(start, end time.Time) (time.Time, time.Time) {
	dayStart := start.UTC().Truncate(24 * time.Hour)
	dayEnd := end.UTC().Truncate(24 * time.Hour)
	if !dayEnd.Equal(end.UTC()) {
		dayEnd = dayEnd.Add(24 * time.Hour)
	}
	return dayStart, dayEnd
}

// expectedDays 返回 [dayStart, dayEnd) 的天数。
func expectedDays(dayStart, dayEnd time.Time) int {
	return int(dayEnd.Sub(dayStart) / (24 * time.Hour))
}

// ensureRawFallbackWindow 回退 raw 的窗口护栏：day 桶跨度 ≤ analytics
// .retention_days（PR5 起配置化；未配置取缺省 90 天——与保留期裁剪 worker
// 同源归一）。
func (u *Query) ensureRawFallbackWindow(dayStart, dayEnd time.Time) error {
	days := u.retentionDays
	if days <= 0 {
		days = domainanalytics.DefaultRetentionDays
	}
	if max := time.Duration(days) * 24 * time.Hour; dayEnd.Sub(dayStart) > max {
		return status.Errorf(codes.InvalidArgument,
			"no rollup coverage for window and window exceeds raw retention (%d days); deploy the analytics rollup worker or narrow the window", days)
	}
	return nil
}

// eventsPerUser 人均事件（UV 为 0 时 0，proto 注释口径）。
func eventsPerUser(total, unique int64) float64 {
	if unique <= 0 {
		return 0
	}
	return float64(total) / float64(unique)
}

// fillDayBuckets 零桶补齐：[dayStart, dayEnd) 全序列升序（Console 趋势图
// 连续性；无数据日为 0）。
func fillDayBuckets(points []domainanalytics.TimeseriesPoint, dayStart, dayEnd time.Time) []domainanalytics.TimeseriesPoint {
	byBucket := make(map[time.Time]domainanalytics.TimeseriesPoint, len(points))
	for _, p := range points {
		byBucket[p.Bucket.UTC()] = p
	}
	out := make([]domainanalytics.TimeseriesPoint, 0, expectedDays(dayStart, dayEnd))
	for d := dayStart; d.Before(dayEnd); d = d.Add(24 * time.Hour) {
		if p, ok := byBucket[d]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, domainanalytics.TimeseriesPoint{Bucket: d})
	}
	return out
}

// fillHourBuckets 零桶补齐（小时粒度；对齐到整点桶序列）。
func fillHourBuckets(points []domainanalytics.TimeseriesPoint, start, end time.Time) []domainanalytics.TimeseriesPoint {
	hourStart := start.UTC().Truncate(time.Hour)
	hourEnd := end.UTC().Truncate(time.Hour)
	if !hourEnd.Equal(end.UTC()) {
		hourEnd = hourEnd.Add(time.Hour)
	}
	byBucket := make(map[time.Time]domainanalytics.TimeseriesPoint, len(points))
	for _, p := range points {
		byBucket[p.Bucket.UTC()] = p
	}
	out := make([]domainanalytics.TimeseriesPoint, 0, int(hourEnd.Sub(hourStart)/time.Hour))
	for h := hourStart; h.Before(hourEnd); h = h.Add(time.Hour) {
		if p, ok := byBucket[h]; ok {
			out = append(out, p)
			continue
		}
		out = append(out, domainanalytics.TimeseriesPoint{Bucket: h})
	}
	return out
}

// —— ListEventDefinitions 偏移游标（pkg/crud token 编解码，签名/上限校验
// 由 crud 承担；本用例不重复实现）——

func decodeDefinitionsOffset(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	offset, err := crud.DecodePageToken(token)
	if err != nil {
		return 0, status.Error(codes.InvalidArgument, err.Error())
	}
	return offset, nil
}

func encodeDefinitionsOffset(offset int) (string, error) {
	token, err := crud.EncodePageToken(offset)
	if err != nil {
		return "", status.Error(codes.Internal, err.Error())
	}
	return token, nil
}

// —— ListUserEvents keyset 游标：base64url(JSON {t: unix_nano, i: id})，
// 不透明；解码失败/越界值 → InvalidArgument ——

type userEventCursorPayload struct {
	T int64 `json:"t"`
	I int64 `json:"i"`
}

func encodeUserEventCursor(c domainanalytics.UserEventCursor) (string, error) {
	b, err := json.Marshal(userEventCursorPayload{T: c.OccurredAt.UnixNano(), I: c.ID})
	if err != nil {
		return "", status.Error(codes.Internal, "encode cursor")
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeUserEventCursor(s string) (*domainanalytics.UserEventCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}
	var p userEventCursorPayload
	if err := json.Unmarshal(raw, &p); err != nil || p.T <= 0 || p.I <= 0 {
		return nil, status.Error(codes.InvalidArgument, "invalid page_token")
	}
	return &domainanalytics.UserEventCursor{OccurredAt: time.Unix(0, p.T).UTC(), ID: p.I}, nil
}
