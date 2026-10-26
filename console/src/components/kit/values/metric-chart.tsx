// vendored from @lynx-go/ui registry/values/metric-chart.tsx @0.2.1（2026-10-25 §12.10-⑤ 上游回流 re-vendor（labels 面））
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground、border-line→border-border、text-fg→text-foreground；cn 统一 @/lib/utils。
// 手工步骤（判断题，工具不自动改）：
// - 依赖 shadcn chart 基元（components/ui/chart.tsx）——消费方缺则随批拷入（torchwood D 批先例）
// 其余与上游一致。
import { useMemo } from "react";
import { CartesianGrid, Line, LineChart, XAxis, YAxis } from "recharts";
import {
  type ChartConfig,
  ChartContainer,
  ChartTooltip,
  ChartTooltipContent,
} from "@/components/ui/chart";
import { cn } from "@/lib/utils";
import { buildMetricRows, type MetricSeries } from "./metric-rows";

// 纯函数面经旁置文件拆出（metric-rows.ts）。本仓 eslint 只许组件文件导出
// 组件（react-refresh）——上游的单入口 re-export 在此去除，消费方直连
// ./metric-rows（行为等价，§12.10-⑤ 方言）。

/**
 * MetricChart（values 块）——多序列时序折线，序列治理四件套内置（design.md §6.1 裁决）：
 *
 * 1. 活跃窗排序：末样本距窗尾 ≤ activeWindowMs（缺省 5min）的序列优先，峰值次之，
 *    声明序兜底——停更的旧序列不淹没在写序列，也不静默丢数据；
 * 2. 渲染上限 + N more：maxSeries（缺省 10）之外的序列计数披露；
 * 3. 服务端截断披露：serverCap { shown, total }（命中超过服务端上限只回前 N 条时）；
 * 4. 点对齐统一时间轴行 + protojson 零值缺省：v 缺席按 missingValue 解释（缺省 0
 *    ——proto3 标量零值省略序列化，全零是合法序列不是缺数据；"gap" 显式断线）。
 *
 * 序列身份 = key（跨刷新稳定；配色按输入序循环 var(--chart-N)）。数值时间轴单形态
 * （t = ms epoch）；动画/圆点全局关闭（轮询刷新不闪烁；recharts 3.10 Bar 入场动画
 * 在嵌入式 WebView 卡死的前车之鉴——§12.8）。行字段内部用 s{原始下标}
 * ——key 里出现点/空格等字符不会落进 CSS 变量名。
 */

export interface MetricChartLabels {
  empty: string;
  more: (hidden: number) => string;
  serverCap: (shown: number, total: number) => string;
}

const DEFAULT_LABELS: MetricChartLabels = {
  empty: "No samples in window",
  more: (hidden) => `+${hidden} more series not shown — ranking keeps active series first`,
  serverCap: (shown, total) => `server cap reached — ${shown} of ${total} series shown`,
};

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
  /** 披露文案注入（消费方语言） */
  labels?: Partial<MetricChartLabels>;
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
  labels,
  className,
}: MetricChartProps) {
  const t = { ...DEFAULT_LABELS, ...labels };
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
      ? t.serverCap(serverCap.shown, serverCap.total)
      : null;

  return (
    <div className={cn("flex flex-col", className)} data-testid="metric-chart">
      {built.rows.length === 0 ? (
        <div
          className="flex items-center justify-center rounded-md border border-border text-xs text-muted-foreground"
          style={{ height }}
          data-testid="metric-chart-empty"
        >
          {t.empty}
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
          {t.more(built.hidden)}
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
