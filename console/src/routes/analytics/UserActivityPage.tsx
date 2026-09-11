import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
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
          <Badge variant="secondary" title="摄入通道（client 端会话 / server 可信代报）">
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
          <span title="服务端接收时间（与上报时间不同说明存在离线补传或时钟偏差）">
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
  const { userId = "" } = useParams<{ userId: string }>();
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
      <PageHeader title="行为轨迹" description={`用户 ${userId} 的事件时间线（raw 实时扫描）`} />
      <div className="space-y-4">
        <div className="flex flex-wrap items-center gap-2">
          <Button asChild variant="outline" size="sm">
            <Link to={`/console/users/${encodeURIComponent(userId)}`}>返回用户详情</Link>
          </Button>
          <Input
            value={nameFilter}
            onChange={(e) => setNameFilter(e.target.value)}
            placeholder="按事件名过滤（已加载页内）"
            className="max-w-xs"
          />
          <div className="flex items-center gap-2">
            <SourceBadge source={source} />
          </div>
        </div>

        <Card>
          <CardHeader>
            <CardTitle className="text-base">时间线</CardTitle>
            <CardDescription>
              按 occurred_at 倒序（服务端接收时间与上报时间不同说明存在离线补传或时钟偏差）
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
              <p className="text-sm text-destructive">加载失败：{errText(q.error)}</p>
            ) : events.length === 0 ? (
              <EmptyState
                title="该用户暂无行为事件"
                description="用户还没有产生任何事件上报（或事件已超出 raw 保留期）。"
              />
            ) : filtered.length === 0 ? (
              <EmptyState
                title="没有匹配的事件"
                description="已加载页内没有匹配过滤条件的事件名；加载更多或更换关键字。"
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
                      {q.isFetchingNextPage ? "加载中…" : "加载更多"}
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
