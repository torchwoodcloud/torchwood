// vendored from fleet-ui registry/values/metric-chart.tsx @0.1.1（2026-10-25 D 批图表切片）
// 拷贝适配（design.md §12.6-5）：token 方言映射 border-line→border-border、
// text-muted→text-muted-foreground、text-fg→text-foreground；文案中文化；
// buildMetricRows 拆至 ./metric-rows（torchwood react-refresh 只导出组件的
// 约束——纯函数旁置，语义与上游一致）；其余与上游一致（序列治理四件套：
// 活跃窗排序/渲染上限+N more/服务端截断披露/统一时间轴+零值缺省）。
import { useMemo } from "react";
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts";
import {
  type ChartConfig,
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { cn } from "@/lib/utils";
import { buildMetricRows } from "./metric-rows";

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


export interface MetricChartProps {
  series: ReadonlyArray<MetricSeries>;
  /** y 轴/tooltip/图例末值的量纲格式化（bytes/percent 等单位词表由页面注入） */
  formatValue?: (value: number) => string;
  /** x 轴刻度格式化（缺省 HH:MM） */
  formatTick?: (t: number) => string;
  maxSeries?: number;
  activeWindowMs?: number;
  missingValue?: number | "gap";
  /** 服务端截断披露（命中超过服务端上限只回前 N 条） */
  serverCap?: { shown: number; total: number };
  /** 图高 px（缺省 224） */
  height?: number;
  className?: string;
}

export function MetricChart({
  series,
  formatValue,
  formatTick,
  maxSeries,
  activeWindowMs,
  missingValue,
  serverCap,
  height = 224,
  className,
}: MetricChartProps) {
  const built = useMemo(
    () => buildMetricRows(series, { maxSeries, activeWindowMs, missingValue }),
    [series, maxSeries, activeWindowMs, missingValue],
  );
  const fmt = formatValue ?? ((value: number) => String(value));
  const fmtTick = formatTick ?? ((t: number) => new Date(t).toTimeString().slice(0, 5));

  const config = useMemo(() => {
    const out: ChartConfig = {};
    // 配色按原始下标循环——序列在输入数组中的次序稳定则跨刷新配色稳定
    for (const field of built.fields) {
      const index = Number(field.slice(1));
      out[field] = {
        label: built.labels[field],
        color: `var(--chart-${(index % 5) + 1})`,
      };
    }
    return out;
  }, [built]);

  const capNote =
    serverCap && serverCap.total > serverCap.shown
      ? `已达服务端截断上限——仅显示 ${serverCap.shown} / ${serverCap.total} 条序列`
      : null;

  return (
    <div className={cn("flex flex-col", className)} data-testid="metric-chart">
      {built.rows.length === 0 ? (
        <div
          className="flex items-center justify-center rounded-md border border-border text-xs text-muted-foreground"
          style={{ height }}
          data-testid="metric-chart-empty"
        >
          窗口内无样本
        </div>
      ) : (
        <ChartContainer config={config} className="w-full" style={{ height }}>
          <LineChart data={built.rows} margin={{ left: 4, right: 12, top: 8, bottom: 0 }}>
            <CartesianGrid vertical={false} strokeDasharray="3 3" />
            <XAxis
              dataKey="t"
              type="number"
              scale="time"
              domain={["dataMin", "dataMax"]}
              tickFormatter={fmtTick}
              tickLine={false}
              axisLine={false}
              tickMargin={8}
              minTickGap={48}
            />
            <YAxis tickFormatter={fmt} tickLine={false} axisLine={false} width={64} />
            <ChartTooltip
              content={
                <ChartTooltipContent
                  indicator="line"
                  labelFormatter={(_, payload) => {
                    const t = (payload?.[0]?.payload as { t?: number } | undefined)?.t;
                    return t != null ? new Date(t).toLocaleTimeString() : "";
                  }}
                />
              }
            />
            {built.fields.map((field) => (
              <Line
                key={field}
                dataKey={field}
                name={field}
                stroke={`var(--color-${field})`}
                strokeWidth={1.5}
                dot={false}
                isAnimationActive={false}
              />
            ))}
          </LineChart>
        </ChartContainer>
      )}
      <div className="flex flex-wrap gap-x-4 gap-y-1 px-1 py-1.5 text-xs text-muted-foreground">
        {built.fields.map((field) => (
          <span key={field} className="inline-flex items-center gap-1.5">
            <span
              className="inline-block h-0.5 w-3 rounded-sm"
              style={{ background: `var(--color-${field})` }}
            />
            {built.labels[field]}
            {built.lastValues[field] !== undefined ? (
              <b className="text-foreground">{fmt(built.lastValues[field]!)}</b>
            ) : null}
          </span>
        ))}
      </div>
      {built.hidden > 0 ? (
        <p className="px-1 pb-1 text-[11px] text-muted-foreground" data-testid="metric-chart-more">
          +{built.hidden} 条序列未显示——排序保活跃序列优先
        </p>
      ) : null}
      {capNote ? (
        <p className="px-1 pb-1 text-[11px] text-muted-foreground" data-testid="metric-chart-cap">
          {capNote}
        </p>
      ) : null}
    </div>
  );
}
