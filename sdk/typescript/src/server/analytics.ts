import type { HttpTransport } from "../http.js";

/**
 * 事件分析面（docs/design/analytics.md §4.1）：服务端权威事件摄入 +
 * 固定形状查询。项目上下文来自凭证（API key 绑定项目；admin 会话带
 * X-Torchwood-Project）。时间参数一律 RFC3339 字符串；日粒度 UTC 切日。
 */

/** 单个服务端事件（可信代报 user_id；上限：批 100 / 名 64 / 键 25 / 16KiB）。 */
export interface ServerAnalyticsEvent {
  name: string;
  /** RFC3339；缺省 = 服务端 now（服务端钳制 [now-24h, now+5min]）。 */
  occurred_at?: string;
  props?: Record<string, unknown>;
  session_id?: string;
  /** 可信代报的事件归属用户；缺省 = 无归属（仅进总量口径）。 */
  user_id?: string;
}

export interface IngestEventsResult {
  accepted: number;
  skipped: number;
}

export interface AnalyticsOverviewKpi {
  total_events: string;
  unique_users: string;
  new_users: string;
  events_per_user: number;
}

export interface AnalyticsTopEvent {
  name: string;
  total: string;
}

export interface AnalyticsTodayStats {
  total_events: string;
  unique_users: string;
}

export interface AnalyticsOverview {
  kpi?: AnalyticsOverviewKpi;
  top_events?: AnalyticsTopEvent[];
  today?: AnalyticsTodayStats;
  /** 窗口 KPI 口径：rollup | raw（worker 未覆盖时回退 raw）。 */
  source?: string;
}

export interface AnalyticsEventDefinition {
  name: string;
  first_seen?: string;
  last_seen?: string;
  total_30d?: string;
}

export interface ListEventDefinitionsResult {
  definitions?: AnalyticsEventDefinition[];
  meta?: {
    total_count?: string;
    page_size?: number;
    next_page_token?: string;
    prev_page_token?: string;
  };
}

export interface AnalyticsTimeseriesPoint {
  bucket?: string;
  total?: string;
  unique_users?: string;
}

export interface QueryTimeseriesResult {
  points?: AnalyticsTimeseriesPoint[];
  /** rollup | raw（HOUR 恒 raw，DAY 优先 rollup）。 */
  source?: string;
}

export interface AnalyticsBreakdownBucket {
  value?: string;
  total?: string;
  unique_users?: string;
}

export interface QueryBreakdownResult {
  buckets?: AnalyticsBreakdownBucket[];
  /** breakdown v1 恒 raw。 */
  source?: string;
}

export interface AnalyticsRetentionCohort {
  cohort?: string;
  size?: string;
  /** retained[k] = cohort+k 日仍活跃用户数（D0–D14）。 */
  retained?: string[];
}

export interface QueryRetentionResult {
  cohorts?: AnalyticsRetentionCohort[];
  /** retention 恒 rollup（user_days / first_seen 基座）。 */
  source?: string;
}

export interface AnalyticsUserEvent {
  id?: string;
  name?: string;
  occurred_at?: string;
  ingested_at?: string;
  /** 摄入通道 client | server。 */
  source?: string;
  platform?: string;
  app_version?: string;
  session_id?: string;
  props?: Record<string, unknown>;
}

export interface ListUserEventsResult {
  events?: AnalyticsUserEvent[];
  meta?: {
    total_count?: string;
    page_size?: number;
    next_page_token?: string;
    prev_page_token?: string;
  };
  /** 下钻恒 raw。 */
  source?: string;
}

export class AnalyticsService {
  constructor(private readonly http: HttpTransport) {}

  /**
   * 服务端权威事件批量摄入（POST /v1/server/analytics/events）。
   * 部分接收语义：坏事件计入 skipped；at-least-once 不做去重。
   */
  async ingest(events: ServerAnalyticsEvent[]): Promise<IngestEventsResult> {
    return this.http.request<IngestEventsResult>("POST", "/v1/server/analytics/events", {
      auth: "apiKey",
      body: { events },
    });
  }

  /** 窗口 KPI + Top 事件 + 今日实时数（GET /v1/server/analytics/overview）。 */
  async getOverview(periodStart: string, periodEnd: string): Promise<AnalyticsOverview> {
    return this.http.request<AnalyticsOverview>("GET", "/v1/server/analytics/overview", {
      auth: "apiKey",
      query: { period_start: periodStart, period_end: periodEnd },
    });
  }

  /** 事件字典分页（GET /v1/server/analytics/event-definitions）。 */
  async listEventDefinitions(params?: { page_size?: number; page_token?: string }): Promise<AnalyticsEventDefinition[]> {
    const res = await this.listEventDefinitionWithMeta(params);
    return res.definitions ?? [];
  }

  /** 同 listEventDefinitions，但携带分页 meta。 */
  async listEventDefinitionWithMeta(params?: {
    page_size?: number;
    page_token?: string;
  }): Promise<ListEventDefinitionsResult> {
    return this.http.request<ListEventDefinitionsResult>("GET", "/v1/server/analytics/event-definitions", {
      auth: "apiKey",
      query: { page_size: params?.page_size, page_token: params?.page_token },
    });
  }

  /** 事件趋势（GET /v1/server/analytics/timeseries；HOUR 走 raw、DAY 走 rollup）。 */
  async queryTimeseries(params: {
    period_start: string;
    period_end: string;
    granularity: "HOUR" | "DAY";
    /** 事件名集合（≤10）；缺省 = 全部事件聚合。 */
    names?: string[];
  }): Promise<QueryTimeseriesResult> {
    return this.http.request<QueryTimeseriesResult>("GET", "/v1/server/analytics/timeseries", {
      auth: "apiKey",
      query: {
        names: params.names,
        period_start: params.period_start,
        period_end: params.period_end,
        granularity: params.granularity,
      },
    });
  }

  /**
   * 维度拆解（GET /v1/server/analytics/breakdown；Top-N + __other__ 归并，
   * 窗口护栏 ≤30 天）。prop_key 服务端白名单校验后参数化。
   */
  async queryBreakdown(params: {
    name: string;
    prop_key: string;
    period_start: string;
    period_end: string;
    top_n?: number;
  }): Promise<QueryBreakdownResult> {
    return this.http.request<QueryBreakdownResult>("GET", "/v1/server/analytics/breakdown", {
      auth: "apiKey",
      query: {
        name: params.name,
        prop_key: params.prop_key,
        period_start: params.period_start,
        period_end: params.period_end,
        top_n: params.top_n,
      },
    });
  }

  /** 留存矩阵（GET /v1/server/analytics/retention；cohort 窗 ≤92 天 × D0–D14）。 */
  async queryRetention(params: { cohort_start: string; cohort_end: string }): Promise<QueryRetentionResult> {
    return this.http.request<QueryRetentionResult>("GET", "/v1/server/analytics/retention", {
      auth: "apiKey",
      query: { cohort_start: params.cohort_start, cohort_end: params.cohort_end },
    });
  }

  /**
   * 用户行为轨迹下钻（GET /v1/server/analytics/users/{user_id}/events；
   * keyset 分页，可选时间窗 ≤92 天）。
   */
  async listUserEvents(
    userId: string,
    params?: { page_size?: number; page_token?: string; period_start?: string; period_end?: string }
  ): Promise<AnalyticsUserEvent[]> {
    const res = await this.listUserEventsWithMeta(userId, params);
    return res.events ?? [];
  }

  /** 同 listUserEvents，但携带分页 meta 与 source 口径。 */
  async listUserEventsWithMeta(
    userId: string,
    params?: { page_size?: number; page_token?: string; period_start?: string; period_end?: string }
  ): Promise<ListUserEventsResult> {
    return this.http.request<ListUserEventsResult>(
      "GET",
      `/v1/server/analytics/users/${encodeURIComponent(userId)}/events`,
      {
        auth: "apiKey",
        query: {
          page_size: params?.page_size,
          page_token: params?.page_token,
          period_start: params?.period_start,
          period_end: params?.period_end,
        },
      }
    );
  }
}
