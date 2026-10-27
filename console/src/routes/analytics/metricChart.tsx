import { useMemo } from "react";
import type { AnalyticsTimeseriesPoint } from "@/api/analytics";
import { MetricChart as KitMetricChart } from "@/components/kit/values/metric-chart";
import { formatBucket, toTrendSeries } from "./shared";

// 图表披露文案（§12.10-⑤ labels 注入面；原 vendored 内置中文改页面注入）
const METRIC_LABELS = {
  empty: "No samples in window",
  more: (hidden: number) => `+${hidden} more series not shown — ranking keeps active series first`,
  serverCap: (shown: number, total: number) =>
    `Server cap reached — showing ${shown} of ${total} series`,
} as const;

// MetricChart 投影薄壳（D 批，design.md §12.6-3）：渲染面换 fleet-ui 块，
// 投影纯函数 toTrendSeries 在 ./shared（与 formatBucket 同居，react-refresh
// 约束）。UTC 桶刻度方言由页面吸收（HOUR `mm-dd hh:00` / DAY `mm-dd`）。

export function TrendChart({
  points,
  granularity,
  height = 256,
}: {
  points: ReadonlyArray<AnalyticsTimeseriesPoint>;
  granularity: "HOUR" | "DAY";
  height?: number;
}) {
  const series = useMemo(() => toTrendSeries(points), [points]);
  return (
    <KitMetricChart
      series={series}
      height={height}
      formatValue={(value) => value.toLocaleString()}
      formatTick={(t) => formatBucket(new Date(t).toISOString(), granularity)}
      labels={METRIC_LABELS}
    />
  );
}
