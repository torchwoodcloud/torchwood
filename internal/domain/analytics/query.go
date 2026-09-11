// 本文件是查询面端口与领域类型（PR3，docs/design/analytics.md §4.1/§6；
// D8 固定形状查询 + source 口径标注；D11 留存自连接）。端口方法按数据需求
// 切分（择路判定在 app 用例层：覆盖检测 → rollup/raw），全部实现必须在读
// 事务内 SET LOCAL statement_timeout（D15）且 SQL 全参数化（prop_key/事件名
// 服务端白名单校验后绑定，沿 aggregateDocuments 纪律）。
package analytics

import (
	"context"
	"time"
)

// 查询窗口护栏（设计 §4.1；proto 注解之上的跨字段规则，app 用例层执行）。
const (
	// MaxTimeseriesHourWindow HOUR 粒度窗上限（走 raw 分区裁剪）。
	MaxTimeseriesHourWindow = 7 * 24 * time.Hour
	// MaxTimeseriesDayWindow DAY 粒度窗上限（优先 rollup）。
	MaxTimeseriesDayWindow = 366 * 24 * time.Hour
	// MaxBreakdownWindow 维度拆解窗上限（raw 直查，D9）。
	MaxBreakdownWindow = 30 * 24 * time.Hour
	// MaxRetentionCohortWindow 留存 cohort 锚定窗上限（D11）。
	MaxRetentionCohortWindow = 92 * 24 * time.Hour
	// MaxUserEventsWindow 用户下钻时间窗上限。
	MaxUserEventsWindow = 92 * 24 * time.Hour
	// MaxOverviewWindow 概览窗上限（与 DAY 趋势同口径；设计护栏表未列，
	// 对齐 Timeseries DAY）。
	MaxOverviewWindow = MaxTimeseriesDayWindow
)

// raw 回退的保留期判定量。PR5 前无 per-project 配置（analytics.retention_days
// 属 PR5），以缺省值参与「窗口 ≤ raw 保留期才允许回退」判定；超窗且无
// rollup 覆盖 → InvalidArgument（不静默返回残缺窗口，PR5 验收同语义）。
const (
	// DefaultRetentionDays raw 保留期缺省（设计 §5；可配域 7–365 由 PR5 落地）。
	DefaultRetentionDays = 90
	// RetentionDaysMin / RetentionDaysMax 保留期可配域（PR5 config）。
	RetentionDaysMin = 7
	RetentionDaysMax = 365
)

// RetentionSlots 留存矩阵列数（D0–D14，D11）。
const RetentionSlots = 15

// 查询口径标注（D8：响应 source 字段值域，Console 明示新鲜度差异）。
const (
	// QuerySourceRollup rollup 三表口径（小时级新鲜度）。
	QuerySourceRollup = "rollup"
	// QuerySourceRaw raw 直读口径（即时）。
	QuerySourceRaw = "raw"
)

// 桶语义常量（Breakdown，D9）。
const (
	// BreakdownUnsetBucket 维度键缺失桶标签。
	BreakdownUnsetBucket = "(unset)"
	// BreakdownOtherBucket Top-N 之外归并桶标签。
	BreakdownOtherBucket = "__other__"
	// BreakdownDefaultTopN top_n 未显式给出（proto3 零值）时的缺省桶数。
	BreakdownDefaultTopN = 20
	// BreakdownMaxTopN top_n 上限（与 protovalidate 注解对齐）。
	BreakdownMaxTopN = 50
)

// Granularity 是趋势查询粒度（proto AnalyticsGranularity 的领域投影）。
type Granularity int

const (
	// GranularityUnspecified 未指定（app 层拒绝）。
	GranularityUnspecified Granularity = iota
	// GranularityHour 小时粒度（恒 raw）。
	GranularityHour
	// GranularityDay 日粒度（优先 rollup，覆盖缺失回退 raw）。
	GranularityDay
)

// TimeseriesPoint 是趋势桶（bucket = UTC 桶起点；DAY 为当日零点，HOUR 为整点）。
type TimeseriesPoint struct {
	Bucket      time.Time
	Total       int64
	UniqueUsers int64
}

// DailyPoint 是 rollup 面按日聚合点（day = UTC 零点）。
type DailyPoint struct {
	Day         time.Time
	Total       int64
	UniqueUsers int64
}

// OverviewKpi 是窗口 KPI（GetOverview）。
type OverviewKpi struct {
	TotalEvents   int64
	UniqueUsers   int64
	NewUsers      int64 // first_seen 表口径（rollup 未跑时为 0）
	EventsPerUser float64
}

// TopEvent 是窗口 Top 事件行。
type TopEvent struct {
	Name  string
	Total int64
}

// TodayStats 是今日实时块（raw 直读，恒 raw 口径）。
type TodayStats struct {
	TotalEvents int64
	UniqueUsers int64
}

// BreakdownBucket 是维度拆解桶（Value 为维度值/(unset)/__other__）。
type BreakdownBucket struct {
	Value       string
	Total       int64
	UniqueUsers int64
}

// RetentionCohort 是单 cohort 行（Retained 长度恒 RetentionSlots，D0–D14）。
type RetentionCohort struct {
	Cohort   time.Time // UTC 零点
	Size     int64
	Retained []int64
}

// UserEvent 是用户行为轨迹行（raw 直读）。
type UserEvent struct {
	ID         int64
	Name       string
	OccurredAt time.Time
	IngestedAt time.Time
	Source     string // SourceClient | SourceServer
	Platform   string
	AppVersion string
	SessionID  string
	Props      []byte // JSON 对象（空 = '{}'）
}

// EventDefinitionInfo 是事件字典行（ListEventDefinitions；含 rollup 维护的
// total_30d——PR5 前恒 0）。
type EventDefinitionInfo struct {
	Name      string
	FirstSeen time.Time
	LastSeen  time.Time
	Total30d  int64
}

// UserEventCursor 是 ListUserEvents 的 keyset 游标（上一页末条去重键：
// occurred_at + id 副键——分区表 id 非全局唯一声明，但 BIGSERIAL 单调，
// (occurred_at, id) 二元组在单项目内构成稳定全序）。
type UserEventCursor struct {
	OccurredAt time.Time
	ID         int64
}

// UserEventsQuery 是用户下钻查询（窗口可选；游标可选 = 首页）。
type UserEventsQuery struct {
	UserID      string
	PeriodStart *time.Time
	PeriodEnd   *time.Time
	Cursor      *UserEventCursor // nil = 首页
	Limit       int              // 页大小（不含探测行）
}

// QueryRepository 是查询面存储端口（bunrepo 提供 Scoped 读事务实现：
// SET LOCAL statement_timeout='15s' + TimeZone='UTC'，全参数化 SQL）。
type QueryRepository interface {
	// DailyCoveredDays 返回 [startDay, endDayExclusive) 内 analytics_daily
	// 有行的去重天数（覆盖检测：与期望天数相等 = rollup 可服务；零事件日
	// 无行 → 视为未覆盖回退 raw，保守但恒正确）。
	DailyCoveredDays(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) (int, error)
	// DailySeries 单事件名（或 name=="" 全事件）的按日聚合：
	//   - name != ""：total/unique_users 直读（(day,name) 主键行，精确 UV）；
	//   - name == ""：total = SUM(total)（全事件精确），UniqueUsers 不填
	//     （跨名并集 UV 不可从 daily 导出，由调用方走 UserDaySeries）。
	DailySeries(ctx context.Context, projectID, name string, startDay, endDayExclusive time.Time) ([]DailyPoint, error)
	// UserDaySeries user_days 按日活跃用户数（全事件并集 UV，精确；行
	// (user_id, day) 主键 → COUNT(*) 即去重用户数）。
	UserDaySeries(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) ([]DailyPoint, error)
	// CountActiveUsers user_days 窗口去重用户数（Overview KPI UV，精确；
	// 跨日不重复计数——区别于 UserDaySeries 的按日计数）。
	CountActiveUsers(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) (int64, error)
	// CountNewUsers first_seen 表 first_day ∈ 窗口的行数（cohort 锚点口径；
	// 表空（PR5 未跑）= 0，不报错）。
	CountNewUsers(ctx context.Context, projectID string, startDay, endDayExclusive time.Time) (int64, error)
	// TopEventsFromDaily 窗口内事件×总量 Top-N（analytics_daily 精确口径）。
	TopEventsFromDaily(ctx context.Context, projectID string, startDay, endDayExclusive time.Time, limit int) ([]TopEvent, error)
	// ListEventDefinitions 字典分页（按 last_seen DESC, name ASC——total_30d
	// 由 rollup 维护（PR5 刷新），当前以 last_seen 排序）；返回当页行 + 总数。
	ListEventDefinitions(ctx context.Context, projectID string, offset, limit int) ([]EventDefinitionInfo, int, error)

	// RawTimeseries raw 扫描分桶聚合（names nil/空 = 全事件；DAY 用
	// date_trunc('day')，HOUR 用 date_trunc('hour')；分区键谓词触发裁剪）。
	RawTimeseries(ctx context.Context, projectID string, names []string, start, end time.Time, granularity Granularity) ([]TimeseriesPoint, error)
	// RawOverviewKPI raw 窗口总量 + UV（GetOverview 回退路径）。
	RawOverviewKPI(ctx context.Context, projectID string, start, end time.Time) (total, uniqueUsers int64, err error)
	// RawTopEvents raw 窗口 Top-N 事件（GetOverview 回退路径）。
	RawTopEvents(ctx context.Context, projectID string, start, end time.Time, limit int) ([]TopEvent, error)
	// RawTodayStats 今日实时块（dayStart = 今日 UTC 零点；恒 raw）。
	RawTodayStats(ctx context.Context, projectID string, dayStart time.Time) (TodayStats, error)
	// RawBreakdown raw 维度拆解：Top-N 桶 + 其余事件聚合的 __other__ 桶
	//（other 为 nil 表示无剩余；UV 精确——其余集整体去重，非 Top-N UV 差值）。
	RawBreakdown(ctx context.Context, projectID, name, propKey string, start, end time.Time, topN int) (top []BreakdownBucket, other *BreakdownBucket, err error)
	// RetentionMatrix first_seen ⋈ user_days 自连接 cohort × D0–D14 矩阵
	//（D11；表空返回空切片，不报错）。
	RetentionMatrix(ctx context.Context, projectID string, cohortStart, cohortEndExclusive time.Time) ([]RetentionCohort, error)
	// ListUserEvents raw keyset 分页（(occurred_at, id) 倒序；Limit+1 探测
	// hasMore，返回恰好 Limit 行）。
	ListUserEvents(ctx context.Context, projectID string, q UserEventsQuery) ([]UserEvent, bool, error)
}
