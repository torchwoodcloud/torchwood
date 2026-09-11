import { api } from "./client";

// Analytics server 面查询 RPC 的 Console client（proto/server/v1/analytics.proto，
// PR4）。JSON 形状对齐 grpc-gateway 序列化约定：字段名 snake_case
// （UseProtoNames）、int64 以字符串承载、Timestamp 为 RFC3339 字符串、
// EmitUnpopulated=false（零值字段可能缺省）。项目上下文由 client.ts 的请求
// 拦截器统一注入 X-Torchwood-Project（Console admin 会话）。

// int64 字段网关可能给 string 或 number，统一解析为 number。
export function num(value: number | string | undefined | null): number {
  if (value === undefined || value === null || value === "") return 0;
  const n = typeof value === "number" ? value : Number(value);
  return Number.isFinite(n) ? n : 0;
}

export interface ListResponseMeta {
  page_size?: number;
  next_page_token?: string;
  prev_page_token?: string;
  // total_count ≤0 = unknown（keyset 分页下以 next_page_token 判定是否还有更多）。
  total_count?: number;
}

export interface AnalyticsOverviewKpi {
  total_events: number | string;
  unique_users: number | string;
  new_users: number | string;
  events_per_user: number;
}

export interface AnalyticsTopEvent {
  name: string;
  total: number | string;
}

export interface AnalyticsTodayStats {
  total_events: number | string;
  unique_users: number | string;
}

// source: "rollup" | "raw"（D8 口径标注；rollup worker 未上线时 DAY 查询回退 raw）。
export interface AnalyticsOverview {
  kpi?: AnalyticsOverviewKpi;
  top_events?: AnalyticsTopEvent[];
  today?: AnalyticsTodayStats;
  source?: string;
}

export interface AnalyticsEventDefinition {
  name: string;
  first_seen?: string;
  last_seen?: string;
  // rollup 维护的近 30 天总量（rollup 未跑时为 0）。
  total_30d: number | string;
}

export interface ListEventDefinitionsResponse {
  definitions?: AnalyticsEventDefinition[];
  meta?: ListResponseMeta;
}

export interface AnalyticsTimeseriesPoint {
  bucket: string;
  total: number | string;
  unique_users: number | string;
}

export interface QueryTimeseriesResponse {
  points?: AnalyticsTimeseriesPoint[];
  source?: string;
}

export interface AnalyticsBreakdownBucket {
  value: string;
  total: number | string;
  unique_users: number | string;
}

export interface QueryBreakdownResponse {
  buckets?: AnalyticsBreakdownBucket[];
  source?: string;
}

export interface AnalyticsRetentionCohort {
  cohort: string;
  size: number | string;
  // retained[k] = cohort+k 日仍活跃用户数，D0–D14（retained[0] = size）。
  retained?: (number | string)[];
}

export interface QueryRetentionResponse {
  cohorts?: AnalyticsRetentionCohort[];
  source?: string;
}

export interface AnalyticsUserEvent {
  id: number | string;
  name: string;
  occurred_at?: string;
  ingested_at?: string;
  // 摄入通道：client | server（区别于响应级 rollup|raw 口径）。
  source?: string;
  platform?: string;
  app_version?: string;
  session_id?: string;
  props?: Record<string, unknown>;
}

export interface ListUserEventsResponse {
  events?: AnalyticsUserEvent[];
  meta?: ListResponseMeta;
  source?: string;
}

// buildSearch 手工序列化查询参数：grpc-gateway 期望 repeated 字段以重复的
// query key 承载（names=a&names=b），axios 默认的数组序列化（names[]=a）
// 不兼容，因此全模块统一走 URLSearchParams。
function buildSearch(
  params: Record<string, string | number | string[] | undefined>
): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value === undefined) continue;
    if (Array.isArray(value)) {
      for (const item of value) search.append(key, item);
    } else {
      search.append(key, String(value));
    }
  }
  return search.toString();
}

// GetOverview 窗口 KPI + Top 事件 + 今日实时数。
export async function getOverview(
  periodStart: string,
  periodEnd: string
): Promise<AnalyticsOverview> {
  const search = buildSearch({ period_start: periodStart, period_end: periodEnd });
  const res = await api.get<AnalyticsOverview>(`/server/analytics/overview?${search}`);
  return res.data;
}

export async function listEventDefinitions(
  pageSize?: number,
  pageToken?: string
): Promise<ListEventDefinitionsResponse> {
  const search = buildSearch({ page_size: pageSize, page_token: pageToken });
  const res = await api.get<ListEventDefinitionsResponse>(
    `/server/analytics/event-definitions?${search}`
  );
  return res.data;
}

// 趋势查询粒度（proto 枚举 AnalyticsGranularity 的 JSON 名称）。
export const GRANULARITY = {
  HOUR: "ANALYTICS_GRANULARITY_HOUR",
  DAY: "ANALYTICS_GRANULARITY_DAY",
} as const;

export async function queryTimeseries(input: {
  names?: string[];
  periodStart: string;
  periodEnd: string;
  granularity: "HOUR" | "DAY";
}): Promise<QueryTimeseriesResponse> {
  const search = buildSearch({
    names: input.names,
    period_start: input.periodStart,
    period_end: input.periodEnd,
    granularity: GRANULARITY[input.granularity],
  });
  const res = await api.get<QueryTimeseriesResponse>(`/server/analytics/timeseries?${search}`);
  return res.data;
}

export async function queryBreakdown(input: {
  name: string;
  propKey: string;
  periodStart: string;
  periodEnd: string;
  topN?: number;
}): Promise<QueryBreakdownResponse> {
  const search = buildSearch({
    name: input.name,
    prop_key: input.propKey,
    period_start: input.periodStart,
    period_end: input.periodEnd,
    top_n: input.topN,
  });
  const res = await api.get<QueryBreakdownResponse>(`/server/analytics/breakdown?${search}`);
  return res.data;
}

export async function queryRetention(
  cohortStart: string,
  cohortEnd: string
): Promise<QueryRetentionResponse> {
  const search = buildSearch({ cohort_start: cohortStart, cohort_end: cohortEnd });
  const res = await api.get<QueryRetentionResponse>(`/server/analytics/retention?${search}`);
  return res.data;
}

// ListUserEvents 用户行为轨迹（raw keyset 分页，page_token 不透明游标）。
export async function listUserEvents(input: {
  userId: string;
  pageSize?: number;
  pageToken?: string;
  periodStart?: string;
  periodEnd?: string;
}): Promise<ListUserEventsResponse> {
  const search = buildSearch({
    page_size: input.pageSize,
    page_token: input.pageToken,
    period_start: input.periodStart,
    period_end: input.periodEnd,
  });
  const res = await api.get<ListUserEventsResponse>(
    `/server/analytics/users/${encodeURIComponent(input.userId)}/events?${search}`
  );
  return res.data;
}
