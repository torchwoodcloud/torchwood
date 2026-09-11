import type { HttpTransport } from "../http.js";
import { TorchwoodError } from "../errors.js";

/**
 * 端侧事件摄入（docs/design/analytics.md §4.2）：只有写入、没有查询
 * （行为数据是项目方资产）。归因（user_id/session_id）取自 principal
 * （含匿名会话），请求体不可伪造；平台级上限：批 100 / 名 64 / 键 25 /
 * 单事件 16KiB。
 *
 * 端配方（会话边界与可靠性由各端 SDK 定义，平台不感知——详见
 * docs/developer/18-analytics.md 端配方章节）：Web visibilitychange +
 * sendBeacon；小游戏持久队列 + onHide 尽力 flush + onShow 补发；原生
 * 前后台切换。`AnalyticsEventBuffer` 是 Web/可映射平台的通用批量缓冲器。
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

/**
 * 定时器注入点：生产环境默认 globalThis；测试用假时钟替换，确定性驱动
 * flush 定时与退避重试（本包不引入 @types/node 依赖）。
 */
export interface AnalyticsTimers {
  setTimeout(fn: () => void, ms: number): unknown;
  clearTimeout(handle: unknown): void;
}

export interface AnalyticsBufferOptions {
  /**
   * size 阈值：缓冲事件数达到该值立即 flush（设计 §9：size/time 双阈值，
   * 默认 20 条）。上限为服务端单批 100 事件的平台级强制（超设按 100 分批）。
   */
  maxBatchSize?: number;
  /** time 阈值：事件进入缓冲后最多等待的毫秒数（默认 10s），到点 flush。 */
  flushIntervalMs?: number;
  /**
   * 单批失败后的最大重试次数（默认 2，即最多 1 + 2 次尝试；指数退避，
   * 有界——耗尽后该批静默丢弃，语义见 AnalyticsEventBuffer 类注释）。
   */
  maxRetries?: number;
  /** 退避基延迟毫秒（默认 500）：第 n 次重试前等待 retryBackoffMs * 2^(n-1)。 */
  retryBackoffMs?: number;
  /**
   * 页面隐藏/关闭时尽力 flush（默认 true）：浏览器环境自动注册
   * document visibilitychange（hidden 时）与 window beforeunload。非浏览器
   * 环境（Node / 小游戏 / 原生）无 document，注册自动跳过——小游戏端用
   * onHide/onShow 自行驱动（docs/developer/18-analytics.md 端配方章节）。
   */
  flushOnHide?: boolean;
  /** 定时器替换点（测试注入）；缺省 = globalThis。 */
  timers?: AnalyticsTimers;
}

/**
 * AnalyticsEventBuffer 是端侧摄入的通用批量缓冲器（设计 §9；执行计划 PR6）：
 *
 * - **size/time 双阈值 flush**：缓冲满 `maxBatchSize`（默认 20）或事件在
 *   缓冲中等待满 `flushIntervalMs`（默认 10s）即发送；
 * - **页面隐藏尽力 flush**：浏览器环境 `visibilitychange`(hidden) 与
 *   `beforeunload` 触发即时 flush（尽力语义——unload 竞速不保证送达，需要
 *   更强送达时 Web 端按 18-analytics.md 配方换 sendBeacon）；
 * - **失败静默**：`track`/`flush` 永不抛错、永不 reject。单批失败按指数
 *   退避重试 `maxRetries` 次（有界；408/429/5xx 与网络错误可重试，其余
 *   4xx 重试必败直接放弃），耗尽后该批**静默丢弃**并停止本轮（剩余事件
 *   等下一次触发）——分析事件是可丢的最佳努力遥测，绝不阻断宿主应用；
 * - **at-least-once（D14）**：重试与页面隐藏补发可导致同一事件重复上报
 *   （响应 accepted/skipped 不抛错，部分接收语义）；平台不做去重，
 *   DISTINCT 类指标天然免疫、计数类接受 ±1% 口径偏差（设计 §3/§11）；
 * - **会话边界不归本类管**：session_id 由各端定义后随事件携带，平台只收
 *   不解释（设计 §2）。
 *
 * 用法（Web）：
 * ```ts
 * const tw = Torchwood.withAccessToken(endpoint, projectId, accessToken);
 * const events = new AnalyticsEventBuffer((batch) => tw.analytics.ingest(batch));
 * events.track({ name: "level_up", props: { level: 3 } });
 * // 页面卸载前（可选）：await events.flush();
 * ```
 */
export class AnalyticsEventBuffer {
  /** 服务端单请求事件数上限（对齐 internal/domain/analytics MaxBatchEvents）。 */
  private static readonly SERVER_MAX_BATCH = 100;

  private readonly send: (events: ClientAnalyticsEvent[]) => Promise<IngestEventsResult>;
  private readonly maxBatchSize: number;
  private readonly flushIntervalMs: number;
  private readonly maxRetries: number;
  private readonly retryBackoffMs: number;
  private readonly timers: AnalyticsTimers;
  private readonly buffer: ClientAnalyticsEvent[] = [];
  private timer?: unknown;
  private inFlight?: Promise<void>;
  private detachHooks?: () => void;
  private disposed = false;

  constructor(
    send: (events: ClientAnalyticsEvent[]) => Promise<IngestEventsResult>,
    options: AnalyticsBufferOptions = {}
  ) {
    this.send = send;
    this.maxBatchSize = clampInt(options.maxBatchSize ?? 20, 1, AnalyticsEventBuffer.SERVER_MAX_BATCH);
    this.flushIntervalMs = Math.max(0, options.flushIntervalMs ?? 10_000);
    this.maxRetries = Math.max(0, options.maxRetries ?? 2);
    this.retryBackoffMs = Math.max(0, options.retryBackoffMs ?? 500);
    this.timers = options.timers ?? globalThisTimers();

    if (options.flushOnHide !== false) {
      this.detachHooks = attachHideHooks(() => {
        // 尽力 flush：unload 竞速下不等待、不重试、不抛错。
        void this.flush();
      });
    }
  }

  /** 当前缓冲中的事件数（监控/测试用）。 */
  get pending(): number {
    return this.buffer.length;
  }

  /**
   * 追加一个事件。缓冲达到 size 阈值时触发异步 flush；任何失败都静默
   * （不抛错）。flush 在途期间新 track 的事件由在途循环顺带清空。
   */
  track(event: ClientAnalyticsEvent): void {
    this.buffer.push(event);
    if (this.buffer.length >= this.maxBatchSize) {
      void this.flush();
      return;
    }
    this.scheduleTimer();
  }

  /**
   * 立即 flush 全部缓冲事件（按 size 阈值分批、逐批发送）。永不 reject；
   * 某批重试耗尽后丢弃该批并停止本轮（剩余事件等下一次触发）。
   * 已有 flush 在途时返回该在途 Promise。
   */
  flush(): Promise<void> {
    if (this.inFlight) return this.inFlight;
    const run = this.runFlush();
    const wrapped = run.finally(() => {
      if (this.inFlight === wrapped) this.inFlight = undefined;
    });
    this.inFlight = wrapped;
    return wrapped;
  }

  /** 停止定时 flush 与页面钩子（缓冲保留，仍可手动 track/flush）。 */
  dispose(): void {
    this.disposed = true;
    this.clearTimer();
    this.detachHooks?.();
    this.detachHooks = undefined;
  }

  private scheduleTimer(): void {
    if (this.disposed || this.timer !== undefined || this.buffer.length === 0) return;
    this.timer = this.timers.setTimeout(() => {
      this.timer = undefined;
      void this.flush();
    }, this.flushIntervalMs);
  }

  private clearTimer(): void {
    if (this.timer !== undefined) {
      this.timers.clearTimeout(this.timer);
      this.timer = undefined;
    }
  }

  private async runFlush(): Promise<void> {
    this.clearTimer();
    try {
      while (this.buffer.length > 0) {
        const batch = this.buffer.splice(0, this.maxBatchSize);
        const sent = await this.sendWithRetry(batch);
        if (!sent) break; // 该批已静默丢弃；本轮停止，剩余事件等下次触发
      }
    } finally {
      // 失败残留 / 在途期间新 track 的事件按 time 阈值再排下一轮。
      this.scheduleTimer();
    }
  }

  /**
   * 单批发送 + 有界退避重试。返回 true = 已发送（含部分接收 accepted/
   * skipped 响应）；false = 重试耗尽或不可重试失败，该批静默丢弃。
   * 重试期间批保留在途（不丢批）——at-least-once 语义的来源之一。
   */
  private async sendWithRetry(batch: ClientAnalyticsEvent[]): Promise<boolean> {
    for (let attempt = 0; ; attempt++) {
      try {
        await this.send(batch);
        return true;
      } catch (err) {
        if (!isRetryableError(err) || attempt >= this.maxRetries) {
          return false;
        }
        await this.delay(this.retryBackoffMs * 2 ** attempt);
      }
    }
  }

  private delay(ms: number): Promise<void> {
    if (ms <= 0) return Promise.resolve();
    return new Promise((resolve) => {
      this.timers.setTimeout(resolve, ms);
    });
  }
}

/** isRetryableError 判定失败是否值得重试（规则见 AnalyticsEventBuffer 类注释）。 */
function isRetryableError(err: unknown): boolean {
  if (err instanceof TorchwoodError) {
    if (err.status === 408 || err.status === 429) return true;
    if (err.status >= 500 && err.status < 600) return true;
    return false; // 4xx 语义性失败（校验/鉴权）与本地构造错误：重试必败
  }
  return true; // 网络层异常（fetch TypeError 等）：保守重试
}

function clampInt(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, Math.floor(value)));
}

function globalThisTimers(): AnalyticsTimers {
  const g = globalThis as unknown as {
    setTimeout: (fn: () => void, ms: number) => unknown;
    clearTimeout: (handle: unknown) => void;
  };
  return { setTimeout: (fn, ms) => g.setTimeout(fn, ms), clearTimeout: (h) => g.clearTimeout(h) };
}

/** attachHideHooks 可用的事件监听目标（浏览器 DOM 的最小结构类型）。 */
interface HideEventTarget {
  addEventListener?: (type: string, fn: () => void) => void;
  removeEventListener?: (type: string, fn: () => void) => void;
}

/**
 * attachHideHooks 在浏览器环境注册页面隐藏钩子；非浏览器环境（无 document）
 * 返回 undefined。返回注销函数（dispose 用）。
 */
function attachHideHooks(onHide: () => void): (() => void) | undefined {
  const g = globalThis as {
    document?: {
      visibilityState?: string;
      defaultView?: HideEventTarget;
    } & HideEventTarget;
    window?: HideEventTarget;
  };
  const doc = g.document;
  if (!doc?.addEventListener) return undefined;
  const win = g.window ?? doc.defaultView;

  const onVisibility = () => {
    if (!doc.visibilityState || doc.visibilityState === "hidden") onHide();
  };
  doc.addEventListener("visibilitychange", onVisibility);

  const onUnload = () => onHide();
  if (win?.addEventListener) {
    win.addEventListener("beforeunload", onUnload);
    return () => {
      doc.removeEventListener?.("visibilitychange", onVisibility);
      win.removeEventListener?.("beforeunload", onUnload);
    };
  }
  return () => {
    doc.removeEventListener?.("visibilitychange", onVisibility);
  };
}
