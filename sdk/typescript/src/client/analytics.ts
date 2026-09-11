import type { HttpTransport } from "../http.js";

/**
 * 端侧事件摄入（docs/design/analytics.md §4.2）：只有写入、没有查询
 * （行为数据是项目方资产）。归因（user_id/session_id）取自 principal
 * （含匿名会话），请求体不可伪造；平台级上限：批 100 / 名 64 / 键 25 /
 * 单事件 16KiB。
 *
 * 端配方（会话边界与可靠性由各端 SDK 定义，平台不感知——PR6 缓冲器与
 * 端配方文档随后）：Web visibilitychange + sendBeacon；小游戏持久队列 +
 * onHide 尽力 flush + onShow 补发；原生前后台切换。
 */

/** 单个端侧事件。occurred_at 缺省 = 服务端 now（钳制 [now-24h, now+5min]）。 */
export interface ClientAnalyticsEvent {
  name: string;
  occurred_at?: string;
  props?: Record<string, unknown>;
  session_id?: string;
}

export interface IngestEventsResult {
  accepted: number;
  skipped: number;
}

export class ClientAnalyticsService {
  constructor(private readonly http: HttpTransport) {}

  /**
   * 批量摄入端侧事件（POST /v1/analytics/events）。部分接收语义：坏事件
   * 计入 skipped、好事件照收；at-least-once（重试可能重复计数）。
   */
  async ingest(events: ClientAnalyticsEvent[]): Promise<IngestEventsResult> {
    return this.http.request<IngestEventsResult>("POST", "/v1/analytics/events", {
      body: { events },
    });
  }
}
