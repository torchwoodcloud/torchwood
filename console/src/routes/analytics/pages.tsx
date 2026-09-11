import { useMemo, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import {
  ResponsiveContainer,
  LineChart,
  Line,
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
import {
  getOverview,
  listEventDefinitions,
  queryRetention,
  queryTimeseries,
  num,
} from "@/api/analytics";
import {
  AnalyticsTabs,
  EventLink,
  SourceBadge,
  SourceNote,
} from "./components";
import {
  errText,
  formatBucket,
  overviewWindow,
  retentionFutureDay,
  retentionPercent,
  retentionWindow,
  timeseriesRows,
  type OverviewRange,
} from "./shared";

const OVERVIEW_RANGES: { key: OverviewRange; label: string }[] = [
  { key: "today", label: "今日" },
  { key: "7d", label: "近 7 天" },
  { key: "30d", label: "近 30 天" },
];

function RangeSwitcher<T extends string | number>({
  value,
  options,
  onChange,
}: {
  value: T;
  options: { key: T; label: string }[];
  onChange: (v: T) => void;
}) {
  return (
    <div className="flex gap-1">
      {options.map((opt) => (
        <Button
          key={opt.key}
          size="sm"
          variant={value === opt.key ? "default" : "outline"}
          onClick={() => onChange(opt.key)}
        >
          {opt.label}
        </Button>
      ))}
    </div>
  );
}

function LoadError({ error }: { error: unknown }) {
  return <p className="text-sm text-destructive">加载失败：{errText(error)}</p>;
}

function KpiCard({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardDescription>{label}</CardDescription>
        <CardTitle className="text-2xl tabular-nums">{value}</CardTitle>
      </CardHeader>
      {hint && (
        <CardContent className="text-xs text-muted-foreground">{hint}</CardContent>
      )}
    </Card>
  );
}

// AnalyticsOverviewPage 概览：日期范围切换 → KPI 卡 + 趋势图 + Top 事件表。
// 数据源 GetOverview + QueryTimeseries（今日视图用 HOUR 粒度，其余 DAY）。
export function AnalyticsOverviewPage() {
  const { projectId } = useAuth();
  const [range, setRange] = useState<OverviewRange>("7d");
  const win = overviewWindow(range);
  // 今日视图走 HOUR（raw，即时）；其余走 DAY（rollup 优先，未覆盖回退 raw）。
  const granularity: "HOUR" | "DAY" = range === "today" ? "HOUR" : "DAY";

  const overviewQ = useQuery({
    queryKey: ["analytics", "overview", projectId, range],
    queryFn: () => getOverview(win.start, win.end),
    enabled: !!projectId,
  });
  const trendQ = useQuery({
    queryKey: ["analytics", "trend", projectId, range, granularity],
    queryFn: () =>
      queryTimeseries({ periodStart: win.start, periodEnd: win.end, granularity }),
    enabled: !!projectId,
  });

  const kpi = overviewQ.data?.kpi;
  const today = overviewQ.data?.today;
  const topEvents = overviewQ.data?.top_events ?? [];
  const rows = useMemo(
    () => timeseriesRows(trendQ.data?.points ?? [], granularity),
    [trendQ.data, granularity]
  );
  const hasData = num(kpi?.total_events) > 0;
  const kpiLoading = overviewQ.isLoading;
  const trendLoading = trendQ.isLoading;

  return (
    <>
      <PageHeader
        title="Analytics"
        description="事件分析概览（UTC 切日口径；时间范围以服务端窗口护栏为准）"
      />
      <AnalyticsTabs />
      <div className="space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <RangeSwitcher value={range} options={OVERVIEW_RANGES} onChange={setRange} />
          <div className="flex items-center gap-2">
            <span className="text-xs text-muted-foreground">KPI 口径</span>
            <SourceBadge source={overviewQ.data?.source} />
          </div>
        </div>

        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-5">
          {overviewQ.error ? (
            <div className="sm:col-span-2 lg:col-span-5">
              <LoadError error={overviewQ.error} />
            </div>
          ) : kpiLoading || !overviewQ.data ? (
            Array.from({ length: 5 }).map((_, i) => (
              <Card key={i}>
                <CardHeader className="pb-2">
                  <Skeleton className="h-4 w-20" />
                  <Skeleton className="h-8 w-16" />
                </CardHeader>
              </Card>
            ))
          ) : (
            <>
              <KpiCard label="事件总量" value={num(kpi?.total_events).toLocaleString()} />
              <KpiCard
                label="活跃用户（UV）"
                value={num(kpi?.unique_users).toLocaleString()}
              />
              <KpiCard
                label="新增用户"
                value={num(kpi?.new_users).toLocaleString()}
                hint="首次上报口径（first_seen 表）"
              />
              <KpiCard
                label="人均事件"
                value={num(kpi?.events_per_user).toFixed(2)}
              />
              <KpiCard
                label="今日实时"
                value={`${num(today?.total_events).toLocaleString()} / ${num(today?.unique_users).toLocaleString()}`}
                hint="事件 / UV（raw 直读，恒为实时口径）"
              />
            </>
          )}
        </div>

        <Card>
          <CardHeader>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <CardTitle className="text-base">趋势</CardTitle>
                <CardDescription>
                  事件数与活跃用户（{granularity === "HOUR" ? "按小时" : "按天"}，UTC 桶）
                </CardDescription>
              </div>
              <div className="flex items-center gap-2">
                <SourceBadge source={trendQ.data?.source} />
              </div>
            </div>
          </CardHeader>
          <CardContent>
            <SourceNote source={trendQ.data?.source} />
            {trendLoading ? (
              <Skeleton className="h-64 w-full" />
            ) : trendQ.error ? (
              <LoadError error={trendQ.error} />
            ) : !hasData ? (
              <EmptyState
                title="暂无事件数据"
                description="接入端 SDK 或调用摄入 API 上报事件后，这里会出现趋势曲线。"
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
            <CardTitle className="text-base">Top 事件</CardTitle>
            <CardDescription>所选窗口内上报量前 10 的事件</CardDescription>
          </CardHeader>
          <CardContent>
            {kpiLoading ? (
              <Skeleton className="h-40 w-full" />
            ) : overviewQ.error ? (
              <LoadError error={overviewQ.error} />
            ) : topEvents.length === 0 ? (
              <EmptyState
                title="暂无 Top 事件"
                description="窗口内还没有任何事件上报。"
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>事件名</TableHead>
                    <TableHead className="text-right">上报量</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {topEvents.map((e) => (
                    <TableRow key={e.name}>
                      <TableCell>
                        <EventLink name={e.name} />
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {num(e.total).toLocaleString()}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  );
}

// AnalyticsEventsPage 事件字典（自由上报 + 事后发现入口）。
export function AnalyticsEventsPage() {
  const { projectId } = useAuth();
  const [filter, setFilter] = useState("");

  const defsQ = useInfiniteQuery({
    queryKey: ["analytics", "definitions", projectId],
    queryFn: ({ pageParam }) => listEventDefinitions(100, pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (last) => last.meta?.next_page_token || undefined,
    enabled: !!projectId,
  });

  const definitions = useMemo(
    () => defsQ.data?.pages.flatMap((p) => p.definitions ?? []) ?? [],
    [defsQ.data]
  );
  const filtered = filter
    ? definitions.filter((d) => d.name.toLowerCase().includes(filter.toLowerCase()))
    : definitions;

  return (
    <>
      <PageHeader
        title="Analytics"
        description="事件字典（按最近上报排序；点击事件名查看趋势与维度拆解）"
      />
      <AnalyticsTabs />
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <CardTitle className="text-base">事件字典</CardTitle>
              <CardDescription>
                近 30 天总量由 rollup worker 每小时刷新（新事件名的计数在下个整点后出现）
              </CardDescription>
            </div>
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="按事件名过滤（已加载页内）"
              className="max-w-xs"
            />
          </div>
        </CardHeader>
        <CardContent>
          {defsQ.isLoading ? (
            <Skeleton className="h-40 w-full" />
          ) : defsQ.error ? (
            <LoadError error={defsQ.error} />
          ) : filtered.length === 0 ? (
            <EmptyState
              title="暂无事件"
              description={
                filter
                  ? "没有匹配已加载页内事件名的条目。"
                  : "还没有任何事件上报。接入端 SDK 或调用摄入 API 后，事件字典会自动生成（自由上报、事后发现）。"
              }
            />
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>事件名</TableHead>
                    <TableHead>首次上报</TableHead>
                    <TableHead>最近上报</TableHead>
                    <TableHead className="text-right">近 30 天</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {filtered.map((d) => (
                    <TableRow key={d.name}>
                      <TableCell>
                        <EventLink name={d.name} />
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {d.first_seen ? new Date(d.first_seen).toLocaleString() : "-"}
                      </TableCell>
                      <TableCell className="text-sm text-muted-foreground">
                        {d.last_seen ? new Date(d.last_seen).toLocaleString() : "-"}
                      </TableCell>
                      <TableCell className="text-right tabular-nums">
                        {num(d.total_30d).toLocaleString()}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
              {defsQ.hasNextPage && (
                <div className="mt-4 flex justify-center">
                  <Button
                    variant="outline"
                    size="sm"
                    onClick={() => defsQ.fetchNextPage()}
                    disabled={defsQ.isFetchingNextPage}
                  >
                    {defsQ.isFetchingNextPage ? "加载中…" : "加载更多"}
                  </Button>
                </div>
              )}
            </>
          )}
        </CardContent>
      </Card>
    </>
  );
}

const RETENTION_RANGES = [
  { key: 7, label: "近 7 天" },
  { key: 30, label: "近 30 天" },
  { key: 90, label: "近 90 天" },
] as const;

const RETENTION_SLOTS = 15; // D0–D14

// AnalyticsRetentionPage 留存：cohort × D0–D14 矩阵。基座（user_days /
// first_seen）由 rollup worker 每小时幂等重算；项目尚无活跃数据（或 worker
// 尚未跑过首个整点）时为空态，不展示假数据。
export function AnalyticsRetentionPage() {
  const { projectId } = useAuth();
  const [days, setDays] = useState<number>(30);
  const win = retentionWindow(days);

  const q = useQuery({
    queryKey: ["analytics", "retention", projectId, days],
    queryFn: () => queryRetention(win.start, win.end),
    enabled: !!projectId,
  });

  const cohorts = (q.data?.cohorts ?? []).filter((c) => num(c.size) > 0);

  return (
    <>
      <PageHeader
        title="Analytics"
        description="留存矩阵（cohort × D0–D14，日粒度 UTC 切日）"
      />
      <AnalyticsTabs />
      <div className="space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <RangeSwitcher
            value={days}
            options={RETENTION_RANGES.map((r) => ({ key: r.key as number, label: r.label }))}
            onChange={setDays}
          />
          <div className="flex items-center gap-2">
            <span className="text-xs text-muted-foreground">口径</span>
            <SourceBadge source={q.data?.source} />
          </div>
        </div>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">留存矩阵</CardTitle>
            <CardDescription>
              单元格为 Dk 留存率（cohort+k 日仍活跃用户 / cohort 规模）
            </CardDescription>
          </CardHeader>
          <CardContent>
            {q.isLoading ? (
              <Skeleton className="h-64 w-full" />
            ) : q.error ? (
              <LoadError error={q.error} />
            ) : cohorts.length === 0 ? (
              <EmptyState
                title="暂无留存数据"
                description="留存矩阵基于 rollup worker 每小时重算的 user_days / first_seen 基座表。端 SDK 或摄入 API 上报事件并等待下个整点聚合后，这里会出现 cohort × D0–D14 矩阵。"
              />
            ) : (
              <div className="overflow-auto">
                <table className="w-full min-w-[720px] text-sm">
                  <thead>
                    <tr className="border-b text-left text-muted-foreground">
                      <th className="py-2 pr-4 font-medium">Cohort</th>
                      <th className="py-2 pr-4 font-medium">规模</th>
                      {Array.from({ length: RETENTION_SLOTS }).map((_, k) => (
                        <th key={k} className="py-2 px-2 font-medium text-center">
                          D{k}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {cohorts.map((c) => {
                      const size = num(c.size);
                      return (
                        <tr key={c.cohort} className="border-b last:border-0">
                          <td className="py-2 pr-4 whitespace-nowrap">
                            {formatBucket(c.cohort, "DAY")}
                          </td>
                          <td className="py-2 pr-4 tabular-nums">{size.toLocaleString()}</td>
                          {Array.from({ length: RETENTION_SLOTS }).map((_, k) => {
                            const retained = num(c.retained?.[k]);
                            const pct = retentionPercent(retained, size);
                            const future = retentionFutureDay(c.cohort, k);
                            return (
                              <td key={k} className="p-1 text-center">
                                {future ? (
                                  <span className="text-muted-foreground/40">—</span>
                                ) : pct === null ? (
                                  <span>-</span>
                                ) : (
                                  <span
                                    className="inline-block w-12 rounded px-1 py-0.5 text-xs tabular-nums"
                                    style={{
                                      backgroundColor: `rgba(37, 99, 235, ${Math.min(0.85, (pct / 100) * 0.85 + 0.03)})`,
                                      color: pct >= 45 ? "#fff" : "inherit",
                                    }}
                                  >
                                    {pct}%
                                  </span>
                                )}
                              </td>
                            );
                          })}
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  );
}
