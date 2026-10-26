// vendored from @lynx-go/ui registry/values/metric-chart.tsx @0.2.1（2026-10-25 §12.10-⑤ 上游回流 re-vendor（labels 面））
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground、border-line→border-border、text-fg→text-foreground；cn 统一 @/lib/utils。
// 手工步骤（判断题，工具不自动改）：
// - 依赖 shadcn chart 基元（components/ui/chart.tsx）——消费方缺则随批拷入（torchwood D 批先例）
// 其余与上游一致。
/**
 * metric-rows（values 块的纯函数旁置）—— 序列→行折叠，MetricChart 的数据面。
 * 独立成文件：纯函数可单测、消费方 react-refresh 只导出组件约束免拆分
 * （§12.10-⑤ 预拆——torchwood D 批先例回流，块 API 不变）。
 */

export interface MetricPoint {
  /** ms epoch（数值时间轴） */
  t: number;
  /** 缺席/空 = 无值，按 missingValue 解释（缺省 0 = protojson 零值缺省方言） */
  v?: number | null;
}

export interface MetricSeries {
  /** 全图唯一身份（跨刷新稳定——配色与 React key 都随它） */
  key: string;
  /** 图例文案（实体词汇由页面给） */
  label: string;
  points: ReadonlyArray<MetricPoint>;
}

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

/** 序列→行折叠（纯函数；块内经 useMemo 消费）。序列治理四件套见 MetricChart 注释。 */
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
