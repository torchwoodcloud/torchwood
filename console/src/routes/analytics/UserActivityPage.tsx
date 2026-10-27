import { useMemo, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { useInfiniteQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/useAuth";
import { PageHeader } from "@/components/PageHeader";
import { EmptyState } from "@/components/EmptyState";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { listUserEvents, type AnalyticsUserEvent } from "@/api/analytics";
import { SourceBadge } from "./components";
import { errText } from "./shared";

function EventItem({ ev }: { ev: AnalyticsUserEvent }) {
  const props = ev.props && Object.keys(ev.props).length > 0 ? ev.props : null;
  return (
    <div className="relative pl-6 pb-6 last:pb-0">
      <span className="absolute left-1 top-1.5 h-2 w-2 rounded-full bg-primary" aria-hidden />
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-mono text-sm font-medium">{ev.name}</span>
        {ev.source && (
          <Badge variant="secondary" title="Ingestion channel (client-side session / trusted server-side report)">
            {ev.source}
          </Badge>
        )}
        <span className="text-xs text-muted-foreground">
          {ev.occurred_at ? new Date(ev.occurred_at).toLocaleString() : "-"}
        </span>
      </div>
      <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
        {ev.platform && <span>platform: {ev.platform}</span>}
        {ev.app_version && <span>app_version: {ev.app_version}</span>}
        {ev.session_id && <span className="font-mono">session: {ev.session_id}</span>}
        {ev.ingested_at && (
          <span title="Server receive time (a difference from the report time indicates offline backfill or clock skew)">
            ingested: {new Date(ev.ingested_at).toLocaleString()}
          </span>
        )}
      </div>
      {props && (
        <details className="mt-1">
          <summary className="cursor-pointer text-xs text-muted-foreground">props</summary>
          <pre className="mt-1 max-h-40 overflow-auto rounded bg-muted p-2 font-mono text-xs">
            {JSON.stringify(props, null, 2)}
          </pre>
        </details>
      )}
    </div>
  );
}

// AnalyticsUserActivityPage 用户行为轨迹（ListUserEvents raw keyset 分页，
// 窗口 ≤92 天缺省全保留期倒序；事件名过滤为已加载页内的客户端过滤）。
export function AnalyticsUserActivityPage() {
  const { userId = "" } = useParams({ strict: false });
  const { projectId } = useAuth();
  const [nameFilter, setNameFilter] = useState("");

  const q = useInfiniteQuery({
    queryKey: ["analytics", "user-events", projectId, userId],
    queryFn: ({ pageParam }) =>
      listUserEvents({ userId, pageSize: 50, pageToken: pageParam || undefined }),
    initialPageParam: "",
    getNextPageParam: (last) => last.meta?.next_page_token || undefined,
    enabled: !!projectId && !!userId,
  });

  const events = useMemo(
    () => q.data?.pages.flatMap((p) => p.events ?? []) ?? [],
    [q.data]
  );
  const filtered = nameFilter
    ? events.filter((e) => e.name.toLowerCase().includes(nameFilter.toLowerCase()))
    : events;
  const source = q.data?.pages?.[0]?.source;

  return (
    <>
      <PageHeader title="Activity timeline" description={`Event timeline for user ${userId} (raw realtime scan)`} />
      <div className="space-y-4">
        <div className="flex flex-wrap items-center gap-2">
          <Button asChild variant="outline" size="sm">
            <Link to="/console/users/$id" params={{ id: encodeURIComponent(userId) }}>Back to user details</Link>
          </Button>
          <Input
            value={nameFilter}
            onChange={(e) => setNameFilter(e.target.value)}
            placeholder="Filter by event name (loaded pages)"
            className="max-w-xs"
          />
          <div className="flex items-center gap-2">
            <SourceBadge source={source} />
          </div>
        </div>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">Timeline</CardTitle>
            <CardDescription>
              Sorted by occurred_at descending (server receive time differing from report time indicates offline backfill or clock skew)
            </CardDescription>
          </CardHeader>
          <CardContent>
            {q.isLoading ? (
              <div className="space-y-3">
                {Array.from({ length: 5 }).map((_, i) => (
                  <Skeleton key={i} className="h-10 w-full" />
                ))}
              </div>
            ) : q.error ? (
              <p className="text-sm text-destructive">Failed to load: {errText(q.error)}</p>
            ) : events.length === 0 ? (
              <EmptyState
                title="No events for this user yet"
                description="This user has no reported events yet (or the events have fallen outside the raw retention window)."
              />
            ) : filtered.length === 0 ? (
              <EmptyState
                title="No matching events"
                description="No event names in the loaded pages match the filter; load more or change the keyword."
              />
            ) : (
              <>
                <div className="relative">
                  <span
                    className="absolute left-[7px] top-1 bottom-1 w-px bg-border"
                    aria-hidden
                  />
                  {filtered.map((ev) => (
                    <EventItem key={`${ev.id}-${ev.occurred_at ?? ""}`} ev={ev} />
                  ))}
                </div>
                {q.hasNextPage && (
                  <div className="mt-4 flex justify-center">
                    <Button
                      variant="outline"
                      size="sm"
                      onClick={() => q.fetchNextPage()}
                      disabled={q.isFetchingNextPage}
                    >
                      {q.isFetchingNextPage ? "Loading…" : "Load more"}
                    </Button>
                  </div>
                )}
              </>
            )}
          </CardContent>
        </Card>
      </div>
    </>
  );
}
