import { useState } from "react";
import { useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import {
  ResponsiveContainer,
  BarChart,
  Bar,
  XAxis,
  YAxis,
  CartesianGrid,
  Tooltip,
} from "recharts";
import { useAuth } from "@/hooks/useAuth";
import { PageHeader } from "@/components/PageHeader";
import { EmptyState } from "@/components/EmptyState";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { queryBreakdown, queryTimeseries, num } from "@/api/analytics";
import { SourceBadge, SourceNote } from "./components";
import {
  PROP_KEY_PATTERN,
  detailWindow,
  errText,
  granularityRange,
  type DetailRange,
} from "./shared";
import { TrendChart } from "./metricChart";

const GRANULARITIES: { key: "HOUR" | "DAY"; label: string }[] = [
  { key: "DAY", label: "Daily" },
  { key: "HOUR", label: "Hourly" },
];

const RANGES: { key: DetailRange; label: string }[] = [
  { key: "24h", label: "Last 24 hours" },
  { key: "7d", label: "Last 7 days" },
  { key: "30d", label: "Last 30 days" },
];

// AnalyticsEventDetailPage 事件详情：趋势（HOUR/DAY 切换）+ 维度拆解
// （prop_key + Top-N 条形，QueryBreakdown 恒 raw，D9）。
export function AnalyticsEventDetailPage() {
  const { name = "" } = useParams({ strict: false });
  const { projectId } = useAuth();
  const [granularity, setGranularity] = useState<"HOUR" | "DAY">("DAY");
  const [range, setRange] = useState<DetailRange>("7d");
  // HOUR 粒度窗 ≤7 天（服务端护栏）：30d 选择收敛为 7d。
  const effectiveRange = granularityRange(granularity, range);
  const win = detailWindow(effectiveRange);

  const trendQ = useQuery({
    queryKey: ["analytics", "event-trend", projectId, name, granularity, effectiveRange],
    queryFn: () =>
      queryTimeseries({
        names: [name],
        periodStart: win.start,
        periodEnd: win.end,
        granularity,
      }),
    enabled: !!projectId && !!name,
  });

  const trendPoints = trendQ.data?.points ?? [];
  // 全零窗口 = 该事件无上报（业务口径）——空态判断留在页面（§12.6-4），
  // 块内"全零是合法序列"照画的哲学与此两层各司其职。
  const trendAllZero = trendPoints.every(
    (p) => num(p.total) === 0 && num(p.unique_users) === 0
  );

  // 拆解维度键：输入后显式提交（避免逐键触发查询）；服务端白名单校验为准，
  // 客户端先行同款正则拦截明显非法输入。
  const [propKeyInput, setPropKeyInput] = useState("");
  const [appliedPropKey, setAppliedPropKey] = useState("");
  const propKeyValid = PROP_KEY_PATTERN.test(propKeyInput.trim());

  const breakdownQ = useQuery({
    queryKey: ["analytics", "breakdown", projectId, name, appliedPropKey, effectiveRange],
    queryFn: () =>
      queryBreakdown({
        name,
        propKey: appliedPropKey,
        periodStart: win.start,
        periodEnd: win.end,
        topN: 20,
      }),
    enabled: !!projectId && !!name && !!appliedPropKey,
  });

  const buckets = breakdownQ.data?.buckets ?? [];
  const barRows = buckets.map((b) => ({
    value: b.value,
    total: num(b.total),
    uv: num(b.unique_users),
  }));
  const hasBreakdownData = barRows.some((r) => r.total > 0);

  return (
    <>
      <PageHeader
        title={<span className="font-mono">{name}</span>}
        description="Event detail: trend and property breakdown"
      />
      <div className="space-y-4">
        <Card>
          <CardHeader>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <CardTitle className="text-base">Trend</CardTitle>
                <CardDescription>
                  Events and active users ({granularity === "HOUR" ? "hourly" : "daily"}, UTC buckets)
                </CardDescription>
              </div>
              <SourceBadge source={trendQ.data?.source} />
            </div>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap gap-4">
              <div className="flex items-center gap-2">
                <span className="text-sm text-muted-foreground">Granularity</span>
                {GRANULARITIES.map((g) => (
                  <Button
                    key={g.key}
                    size="sm"
                    variant={granularity === g.key ? "default" : "outline"}
                    onClick={() => setGranularity(g.key)}
                  >
                    {g.label}
                  </Button>
                ))}
              </div>
              <div className="flex items-center gap-2">
                <span className="text-sm text-muted-foreground">Range</span>
                {RANGES.map((r) => (
                  <Button
                    key={r.key}
                    size="sm"
                    variant={effectiveRange === r.key ? "default" : "outline"}
                    onClick={() => setRange(r.key)}
                    title={
                      granularity === "HOUR" && r.key === "30d"
                        ? "Hourly granularity is limited to a 7-day window; querying the last 7 days instead"
                        : undefined
                    }
                  >
                    {r.label}
                  </Button>
                ))}
              </div>
            </div>
            <SourceNote source={trendQ.data?.source} />
            {trendQ.isLoading ? (
              <Skeleton className="h-64 w-full" />
            ) : trendQ.error ? (
              <p className="text-sm text-destructive">Failed to load: {errText(trendQ.error)}</p>
            ) : trendAllZero ? (
              <EmptyState
                title="No data for this event in the window"
                description="This event has no reports in the selected window; switch the granularity or widen the time range."
              />
            ) : (
              <TrendChart points={trendPoints} granularity={granularity} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Property breakdown</CardTitle>
            <CardDescription>
              Top-20 breakdown by event property (props key), with the rest merged into __other__ (raw realtime scan, window ≤ 30 days)
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            <form
              className="flex flex-wrap items-center gap-2"
              onSubmit={(e) => {
                e.preventDefault();
                if (propKeyValid) setAppliedPropKey(propKeyInput.trim());
              }}
            >
              <Input
                value={propKeyInput}
                onChange={(e) => setPropKeyInput(e.target.value)}
                placeholder="Props key, e.g. level, scene"
                className="max-w-xs"
              />
              <Button type="submit" size="sm" disabled={!propKeyValid}>
                Break down
              </Button>
              <span className="text-xs text-muted-foreground">
                Key rules: must start with a letter or underscore; only letters, digits, underscores, and dots; up to 64 characters
              </span>
            </form>

            {!appliedPropKey ? (
              <p className="text-sm text-muted-foreground">
                Enter a props key and click "Break down" to see the distribution of its values.
              </p>
            ) : breakdownQ.isLoading ? (
              <Skeleton className="h-56 w-full" />
            ) : breakdownQ.error ? (
              <p className="text-sm text-destructive">Failed to load: {errText(breakdownQ.error)}</p>
            ) : !hasBreakdownData ? (
              <EmptyState
                title="No data for this dimension in the window"
                description="This key does not appear in the event's props within the selected window; verify the key name or adjust the time range and retry."
              />
            ) : (
              <>
                <div style={{ height: Math.max(200, barRows.length * 36) }}>
                  <ResponsiveContainer width="100%" height="100%">
                    <BarChart
                      data={barRows}
                      layout="vertical"
                      margin={{ top: 4, right: 24, bottom: 4, left: 8 }}
                    >
                      <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" horizontal={false} />
                      <XAxis type="number" tick={{ fontSize: 12 }} allowDecimals={false} />
                      <YAxis
                        type="category"
                        dataKey="value"
                        tick={{ fontSize: 12 }}
                        width={160}
                      />
                      <Tooltip />
                      <Bar dataKey="total" name="Events" fill="var(--chart-1)" radius={[0, 4, 4, 0]} isAnimationActive={false} />
                    </BarChart>
                  </ResponsiveContainer>
                </div>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Value</TableHead>
                      <TableHead className="text-right">Events</TableHead>
                      <TableHead className="text-right">Active users</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {buckets.map((b) => (
                      <TableRow key={b.value}>
                        <TableCell className="font-mono text-sm">{b.value}</TableCell>
                        <TableCell className="text-right tabular-nums">
                          {num(b.total).toLocaleString()}
                        </TableCell>
                        <TableCell className="text-right tabular-nums">
                          {num(b.unique_users).toLocaleString()}
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
                <p className="text-xs text-muted-foreground">
                  (unset) = the key does not appear in the event props; __other__ = merged bucket for values outside the Top-20.
                </p>
              </>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  );
}
