import { useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import {
  ResponsiveContainer,
  LineChart,
  Line,
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
  timeseriesRows,
  type DetailRange,
} from "./shared";

const GRANULARITIES: { key: "HOUR" | "DAY"; label: string }[] = [
  { key: "DAY", label: "按天" },
  { key: "HOUR", label: "按小时" },
];

const RANGES: { key: DetailRange; label: string }[] = [
  { key: "24h", label: "近 24 小时" },
  { key: "7d", label: "近 7 天" },
  { key: "30d", label: "近 30 天" },
];

// AnalyticsEventDetailPage 事件详情：趋势（HOUR/DAY 切换）+ 维度拆解
// （prop_key + Top-N 条形，QueryBreakdown 恒 raw，D9）。
export function AnalyticsEventDetailPage() {
  const { name = "" } = useParams<{ name: string }>();
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

  const rows = useMemo(
    () => timeseriesRows(trendQ.data?.points ?? [], granularity),
    [trendQ.data, granularity]
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
        description="事件详情：趋势与维度拆解"
      />
      <div className="space-y-4">
        <Card>
          <CardHeader>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <CardTitle className="text-base">趋势</CardTitle>
                <CardDescription>
                  事件数与活跃用户（{granularity === "HOUR" ? "按小时" : "按天"}，UTC 桶）
                </CardDescription>
              </div>
              <SourceBadge source={trendQ.data?.source} />
            </div>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex flex-wrap gap-4">
              <div className="flex items-center gap-2">
                <span className="text-sm text-muted-foreground">粒度</span>
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
                <span className="text-sm text-muted-foreground">范围</span>
                {RANGES.map((r) => (
                  <Button
                    key={r.key}
                    size="sm"
                    variant={effectiveRange === r.key ? "default" : "outline"}
                    onClick={() => setRange(r.key)}
                    title={
                      granularity === "HOUR" && r.key === "30d"
                        ? "按小时粒度窗口上限 7 天，将按 7 天查询"
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
              <p className="text-sm text-destructive">加载失败：{errText(trendQ.error)}</p>
            ) : rows.every((r) => r.total === 0 && r.uv === 0) ? (
              <EmptyState
                title="窗口内暂无该事件"
                description="该事件在所选窗口内没有上报记录；可切换粒度或扩大时间范围。"
              />
            ) : (
              <div className="h-64 w-full">
                <ResponsiveContainer width="100%" height="100%">
                  <LineChart data={rows} margin={{ top: 8, right: 16, bottom: 0, left: 0 }}>
                    <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" />
                    <XAxis dataKey="label" tick={{ fontSize: 12 }} />
                    <YAxis tick={{ fontSize: 12 }} allowDecimals={false} width={48} />
                    <Tooltip />
                    <Line
                      type="monotone"
                      dataKey="total"
                      name="事件数"
                      stroke="#2563eb"
                      strokeWidth={2}
                      dot={false}
                    />
                    <Line
                      type="monotone"
                      dataKey="uv"
                      name="活跃用户"
                      stroke="#16a34a"
                      strokeWidth={2}
                      dot={false}
                    />
                  </LineChart>
                </ResponsiveContainer>
              </div>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">维度拆解</CardTitle>
            <CardDescription>
              按事件属性（props 键）拆解 Top-20，其余归并为 __other__（raw 实时扫描，窗口 ≤30 天）
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
                placeholder="props 键名，例如 level、scene"
                className="max-w-xs"
              />
              <Button type="submit" size="sm" disabled={!propKeyValid}>
                拆解
              </Button>
              <span className="text-xs text-muted-foreground">
                键名规则：字母或下划线开头，仅含字母/数字/下划线/点，≤64 字符
              </span>
            </form>

            {!appliedPropKey ? (
              <p className="text-sm text-muted-foreground">
                输入一个 props 键名后点击"拆解"，查看该维度取值的分布。
              </p>
            ) : breakdownQ.isLoading ? (
              <Skeleton className="h-56 w-full" />
            ) : breakdownQ.error ? (
              <p className="text-sm text-destructive">加载失败：{errText(breakdownQ.error)}</p>
            ) : !hasBreakdownData ? (
              <EmptyState
                title="该维度在窗口内无数据"
                description="所选窗口内该事件的 props 中没有出现此键；确认键名或调整时间范围后重试。"
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
                      <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" horizontal={false} />
                      <XAxis type="number" tick={{ fontSize: 12 }} allowDecimals={false} />
                      <YAxis
                        type="category"
                        dataKey="value"
                        tick={{ fontSize: 12 }}
                        width={160}
                      />
                      <Tooltip />
                      <Bar dataKey="total" name="事件数" fill="#2563eb" radius={[0, 4, 4, 0]} />
                    </BarChart>
                  </ResponsiveContainer>
                </div>
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>取值</TableHead>
                      <TableHead className="text-right">事件数</TableHead>
                      <TableHead className="text-right">活跃用户</TableHead>
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
                  (unset) = 该键未出现在事件 props 中；__other__ = Top-20 之外取值的归并桶。
                </p>
              </>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  );
}
