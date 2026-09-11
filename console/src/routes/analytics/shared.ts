// Analytics 区页面共享的纯工具（时间窗换算、UTC 桶格式化、参数校验）。
// 口径对齐服务端（docs/design/analytics.md）：v1 UTC 切日；窗口护栏
// HOUR ≤7 天 / DAY ≤366 天 / Breakdown ≤30 天 / Retention cohort ≤92 天。

import { num } from "@/api/analytics";

// int64 数值化统一复用 api 模块的 num（re-export 供页面与测试统一入口）。
export { num };

const DAY_MS = 24 * 60 * 60 * 1000;
const HOUR_MS = 60 * 60 * 1000;

// proto 侧事件名/prop_key 白名单（buf.validate 同款正则），客户端先行校验
// 拆解输入，避免无效请求；服务端仍为准（参数化 + 白名单）。
export const EVENT_NAME_PATTERN = /^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$/;
export const PROP_KEY_PATTERN = /^[a-zA-Z_][a-zA-Z0-9_.]{0,63}$/;

export type OverviewRange = "today" | "7d" | "30d";

// overviewWindow 把范围选择换算为 [start, end) RFC3339：
// today = 今日 UTC 零点起；7d/30d = now 往回等宽窗口。
export function overviewWindow(
  range: OverviewRange,
  now: Date = new Date()
): { start: string; end: string } {
  const end = now;
  let start: Date;
  if (range === "today") {
    start = new Date(
      Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate())
    );
    // now 恰为 UTC 零点时 start == end，服务端要求 start < end。
    if (start.getTime() >= end.getTime()) {
      start = new Date(end.getTime() - HOUR_MS);
    }
  } else {
    start = new Date(end.getTime() - (range === "7d" ? 7 : 30) * DAY_MS);
  }
  return { start: start.toISOString(), end: end.toISOString() };
}

export type DetailRange = "24h" | "7d" | "30d";

// detailWindow 事件详情页的时间窗。
export function detailWindow(
  range: DetailRange,
  now: Date = new Date()
): { start: string; end: string } {
  const end = now;
  const span = range === "24h" ? DAY_MS : range === "7d" ? 7 * DAY_MS : 30 * DAY_MS;
  return { start: new Date(end.getTime() - span).toISOString(), end: end.toISOString() };
}

// granularityRange 修正：HOUR 粒度窗 ≤7 天（服务端护栏），30d 选择在
// HOUR 下收敛为 7d。
export function granularityRange(
  granularity: "HOUR" | "DAY",
  range: DetailRange
): DetailRange {
  return granularity === "HOUR" && range === "30d" ? "7d" : range;
}

// 趋势图桶标签：服务端桶起点为 UTC（DAY = 当日零点，HOUR = 整点），
// 统一按 UTC 格式化避免客户端时区把桶错位到别处。
export function formatBucket(iso: string, granularity: "HOUR" | "DAY"): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const mm = String(d.getUTCMonth() + 1).padStart(2, "0");
  const dd = String(d.getUTCDate()).padStart(2, "0");
  if (granularity === "DAY") return `${mm}-${dd}`;
  const hh = String(d.getUTCHours()).padStart(2, "0");
  return `${mm}-${dd} ${hh}:00`;
}

// 趋势数据转图表行（total/uv 数值化）。
export function timeseriesRows(
  points: { bucket: string; total: number | string; unique_users?: number | string }[],
  granularity: "HOUR" | "DAY"
): { label: string; total: number; uv: number }[] {
  return points.map((p) => ({
    label: formatBucket(p.bucket, granularity),
    total: num(p.total),
    uv: num(p.unique_users),
  }));
}

// retentionPercent 留存率（百分比，四舍五入；size 为 0 时无意义返回 null）。
export function retentionPercent(retained: number, size: number): number | null {
  if (size <= 0) return null;
  return Math.round((retained / size) * 100);
}

// 留存单元格是否为"未来日"（cohort + k 天尚未来到，无数据可谈）。
export function retentionFutureDay(cohortIso: string, k: number, now: Date = new Date()): boolean {
  const cohort = new Date(cohortIso);
  if (Number.isNaN(cohort.getTime())) return false;
  const day = Date.UTC(
    cohort.getUTCFullYear(),
    cohort.getUTCMonth(),
    cohort.getUTCDate()
  );
  const today = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate());
  return day + k * DAY_MS > today;
}

// 留存 cohort 窗（服务端护栏 ≤92 天；取整到 UTC 零点对齐日粒度基座）。
export function retentionWindow(
  days: number,
  now: Date = new Date()
): { start: string; end: string } {
  const end = new Date(
    Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()) + DAY_MS
  );
  const start = new Date(end.getTime() - days * DAY_MS);
  return { start: start.toISOString(), end: end.toISOString() };
}

// errText 提取 axios/gRPC 网关错误信息（ErrorResponse.error.message 优先）。
export function errText(error: unknown): string {
  if (!error) return "请求失败";
  const e = error as {
    response?: { data?: { error?: { message?: string } } };
    message?: string;
  };
  return e.response?.data?.error?.message || e.message || "请求失败";
}
