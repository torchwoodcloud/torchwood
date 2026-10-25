import { useMemo } from "react";
import type { AnalyticsTimeseriesPoint } from "@/api/analytics";
import { MetricChart as FleetMetricChart } from "@/components/fleet/values/metric-chart";
import { formatBucket, toTrendSeries } from "./shared";

// 图表披露文案（§12.10-⑤ labels 注入面；原 vendored 内置中文改页面注入）
const METRIC_ZH = {
  empty: "窗口内无样本",
  more: (hidden: number) => `+${hidden} 条序列未显示——排序保持活跃序列优先`,
  serverCap: (shown: number, total: number) =>
    `已达服务端上限——显示 ${shown} / ${total} 条序列`,
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
    <FleetMetricChart
      series={series}
      height={height}
      formatValue={(value) => value.toLocaleString()}
      formatTick={(t) => formatBucket(new Date(t).toISOString(), granularity)}
      labels={METRIC_ZH}
    />
  );
}
