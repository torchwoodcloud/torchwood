// vendored from fleet-ui registry/values/metric-chart.tsx @0.1.1 的行折叠纯函数
// （2026-10-25 D 批拆分旁置——react-refresh 只导出组件约束；语义与上游一致）。
import type { MetricSeries } from "./metric-chart";

export interface BuildMetricRowsOptions {
  maxSeries?: number;
  activeWindowMs?: number;
  missingValue?: number | "gap";
}

export interface BuiltMetricRows {
  /** 对齐到统一时间轴的行（字段 s{原始下标}） */
  rows: ReadonlyArray<Record<string, number | string | null> & { t: number }>;
  /** 渲染面（排序+截断后）的行字段名，即 s{原始下标} */
  fields: ReadonlyArray<string>;
  /** 行字段 → 图例文案 */
  labels: Readonly<Record<string, string>>;
  /** 行字段 → 图例末值（最后一个非空样本） */
  lastValues: Readonly<Record<string, number | undefined>>;
  /** 渲染上限外被截下的序列数 */
  hidden: number;
}

/** 序列→行折叠（纯函数，导出可单测；块内经 useMemo 消费）。 */
export function buildMetricRows(
  series: ReadonlyArray<MetricSeries>,
  options: BuildMetricRowsOptions = {},
): BuiltMetricRows {
  const maxSeries = options.maxSeries ?? 10;
  const activeWindowMs = options.activeWindowMs ?? 5 * 60_000;
  const missingValue = options.missingValue ?? 0;

  const scored = series.map((s, index) => {
    let lastT = Number.NEGATIVE_INFINITY;
    let peak = Number.NEGATIVE_INFINITY;
    for (const point of s.points) {
      if (!Number.isFinite(point.t)) continue;
      if (point.t > lastT) lastT = point.t;
      const v = point.v ?? (missingValue === "gap" ? null : missingValue);
      if (v !== null && v > peak) peak = v;
    }
    return { index, lastT, peak };
  });
  const windowEnd = scored.reduce((acc, s) => Math.max(acc, s.lastT), Number.NEGATIVE_INFINITY);
  const ranked = scored
    .filter((s) => s.lastT > Number.NEGATIVE_INFINITY) // 全脏点序列不进渲染面
    .sort((a, b) => {
      const aActive = windowEnd - a.lastT <= activeWindowMs;
      const bActive = windowEnd - b.lastT <= activeWindowMs;
      if (aActive !== bActive) return aActive ? -1 : 1;
      if (a.peak !== b.peak) return b.peak - a.peak;
      return a.index - b.index;
    });
  const displayed = ranked.slice(0, maxSeries);
  const hidden = series.length - displayed.length;

  const fields: string[] = [];
  const labels: Record<string, string> = {};
  for (const s of displayed) {
    const field = `s${s.index}`;
    fields.push(field);
    labels[field] = series[s.index]!.label;
  }

  // 对齐到统一时间轴行：缺席序列在该时刻无字段（recharts 断线）；v 缺席按方言解释
  const timeMap = new Map<number, Record<string, number | string | null> & { t: number }>();
  for (const s of displayed) {
    const field = `s${s.index}`;
    for (const point of series[s.index]!.points) {
      if (!Number.isFinite(point.t)) continue;
      const row = timeMap.get(point.t) ?? { t: point.t };
      if (point.v === undefined || point.v === null) {
        row[field] = missingValue === "gap" ? null : missingValue;
      } else {
        row[field] = point.v;
      }
      timeMap.set(point.t, row);
    }
  }
  const rows = [...timeMap.values()].sort((a, b) => a.t - b.t);

  const lastValues: Record<string, number | undefined> = {};
  for (const field of fields) {
    for (let i = rows.length - 1; i >= 0; i -= 1) {
      const value = rows[i]![field];
      if (typeof value === "number") {
        lastValues[field] = value;
        break;
      }
    }
  }

  return { rows, fields, labels, lastValues, hidden };
}
