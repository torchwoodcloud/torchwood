import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { useCollection } from "@lynx-go/uikit-core";
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
  type OverviewRange,
} from "./shared";
import { TrendChart } from "./metricChart";
import { StatCard } from "@/components/kit/values/stat-card";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import {
  eventDefinitionsResource,
  eventDefinitionsSearch,
} from "@/kit/analytics";

const OVERVIEW_RANGES: { key: OverviewRange; label: string }[] = [
  { key: "today", label: "Today" },
  { key: "7d", label: "Last 7 days" },
  { key: "30d", label: "Last 30 days" },
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
  return <p className="text-sm text-destructive">Failed to load: {errText(error)}</p>;
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
  const hasData = num(kpi?.total_events) > 0;
  const kpiLoading = overviewQ.isLoading;
  const trendLoading = trendQ.isLoading;

  return (
    <>
      <PageHeader
        title="Analytics"
        description="Event analytics overview (UTC day boundaries; time ranges follow server-side window guards)"
      />
      <AnalyticsTabs />
      <div className="space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <RangeSwitcher value={range} options={OVERVIEW_RANGES} onChange={setRange} />
          <div className="flex items-center gap-2">
            <span className="text-xs text-muted-foreground">KPI source</span>
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
              <StatCard label="Total events" value={num(kpi?.total_events).toLocaleString()} />
              <StatCard
                label="Active users (UV)"
                value={num(kpi?.unique_users).toLocaleString()}
              />
              <StatCard
                label="New users"
                value={num(kpi?.new_users).toLocaleString()}
                hint="First-reported basis (first_seen table)"
              />
              <StatCard
                label="Events per user"
                value={num(kpi?.events_per_user).toFixed(2)}
              />
              <StatCard
                label="Today (realtime)"
                value={`${num(today?.total_events).toLocaleString()} / ${num(today?.unique_users).toLocaleString()}`}
                hint="Events / UV (raw read, always realtime)"
              />
            </>
          )}
        </div>

        <Card>
          <CardHeader>
            <div className="flex flex-wrap items-center justify-between gap-2">
              <div>
                <CardTitle className="text-base">Trend</CardTitle>
                <CardDescription>
                  Events and active users ({granularity === "HOUR" ? "hourly" : "daily"}, UTC buckets)
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
                title="No event data yet"
                description="The trend line appears once events are reported via the client SDK or the ingestion API."
              />
            ) : (
              <TrendChart
                points={trendQ.data?.points ?? []}
                granularity={granularity}
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Top events</CardTitle>
            <CardDescription>Top 10 events by reported volume in the selected window</CardDescription>
          </CardHeader>
          <CardContent>
            {kpiLoading ? (
              <Skeleton className="h-40 w-full" />
            ) : overviewQ.error ? (
              <LoadError error={overviewQ.error} />
            ) : topEvents.length === 0 ? (
              <EmptyState
                title="No top events yet"
                description="No events have been reported in this window yet."
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Event name</TableHead>
                    <TableHead className="text-right">Volume</TableHead>
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
  const [filter, setFilter] = useState("");

  // 事件字典 fleet 化（§12.9-2）：游标/页大小进 URL；页内过滤保持本地态。
  const list = useCollection(eventDefinitionsResource, {
    from: "/console/analytics/events",
    search: eventDefinitionsSearch,

  });
  const filtered = filter
    ? list.rows.filter((d) => d.name.toLowerCase().includes(filter.toLowerCase()))
    : list.rows;

  return (
    <>
      <PageHeader
        title="Analytics"
        description="Event dictionary (sorted by most recent report; click an event name to view trend and property breakdown)"
      />
      <AnalyticsTabs />
      <Card>
        <CardHeader>
          <div className="flex flex-wrap items-center justify-between gap-2">
            <div>
              <CardTitle className="text-base">Event dictionary</CardTitle>
              <CardDescription>
                30-day totals are refreshed hourly by the rollup worker (counts for new event names appear after the next top of the hour)
              </CardDescription>
            </div>
            <Input
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              placeholder="Filter by event name (loaded pages)"
              className="max-w-xs"
            />
          </div>
        </CardHeader>
        <CardContent>
          {list.status === "loading" ? (
            <Skeleton className="h-40 w-full" />
          ) : list.status === "error" ? (
            <LoadError error={list.error} />
          ) : filtered.length === 0 ? (
            <EmptyState
              title="No events yet"
              description={
                filter
                  ? "No entries match the event name within loaded pages."
                  : "No events reported yet. The event dictionary is generated automatically once events are reported via the client SDK or the ingestion API (free-form reporting, discoverable afterwards)."
              }
            />
          ) : (
            <>
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Event name</TableHead>
                    <TableHead>First seen</TableHead>
                    <TableHead>Last seen</TableHead>
                    <TableHead className="text-right">Last 30 days</TableHead>
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
              <div className="mt-4">
                <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
              </div>
            </>
          )}
        </CardContent>
      </Card>
    </>
  );
}

const RETENTION_RANGES = [
  { key: 7, label: "Last 7 days" },
  { key: 30, label: "Last 30 days" },
  { key: 90, label: "Last 90 days" },
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
        description="Retention matrix (cohort × D0–D14, daily granularity, UTC day boundaries)"
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
            <span className="text-xs text-muted-foreground">Source</span>
            <SourceBadge source={q.data?.source} />
          </div>
        </div>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Retention matrix</CardTitle>
            <CardDescription>
              Each cell is the Dk retention rate (users still active on day cohort+k / cohort size)
            </CardDescription>
          </CardHeader>
          <CardContent>
            {q.isLoading ? (
              <Skeleton className="h-64 w-full" />
            ) : q.error ? (
              <LoadError error={q.error} />
            ) : cohorts.length === 0 ? (
              <EmptyState
                title="No retention data yet"
                description="The retention matrix is based on the user_days / first_seen base tables recomputed hourly by the rollup worker. The cohort × D0–D14 matrix appears once events are reported via the client SDK or the ingestion API and the next hourly aggregation completes."
              />
            ) : (
              <div className="overflow-auto">
                <table className="w-full min-w-[720px] text-sm">
                  <thead>
                    <tr className="border-b text-left text-muted-foreground">
                      <th className="py-2 pr-4 font-medium">Cohort</th>
                      <th className="py-2 pr-4 font-medium">Size</th>
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
