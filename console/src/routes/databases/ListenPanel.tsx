import { useOutletContext } from "@/router/outlet-context";
import { useSubscription } from "@lynx-go/ui-kit";
import { RefreshCw } from "lucide-react";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatTimeOnly } from "@/lib/datetime";
import { collectionEvents, type EventFrame } from "@/fleet/realtime";
import { EventFeed } from "@/components/fleet/subscription/event-feed";
import { Button } from "@/components/ui/button";
import type { CollectionOutletContext } from "./CollectionLayout";

// payload 摘要（120 字截断）——事件名外的载荷细节看大概即可，完整 JSON 在
// 服务端审计/文档侧。
function summarize(payload: Record<string, unknown>): string {
  const text = JSON.stringify(payload);
  return text.length > 120 ? `${text.slice(0, 120)}…` : text;
}

// ListenPanel 集合「试听」面板（v2 设计 §4.2/§4.3；fleet-ui 订阅族切片
// design.md §12.4）：连接编排归订阅引擎（hello 绑 project_id → 订阅当前
// 集合频道 → 事件经环形缓冲入 EventFeed）；断线由引擎退避重连——先走
// console auth refresh 续会话再重订（不补历史），预算耗尽 stalled 显示
// 「已断开，点击重连」（sub.restart 重置预算）。
export function ListenPanel() {
  const { dbId, collId } = useOutletContext<CollectionOutletContext>();
  const { projectId } = useAuth();
  const tz = useUserTimezone();

  const sub = useSubscription(collectionEvents, {
    params: { dbId, collId },
    scope: { projectId: projectId ?? undefined },
    cursor: null, // 活尾即开；重连即重订（服务端不补历史，无游标语义）
  });

  const paused = sub.status === "paused";

  return (
    <div className="space-y-2">
      {/* 暂停/恢复（页面编排）：暂停后帧自游标缓冲，恢复续订——试听场景
          用于冻结画面对照 */}
      <div className="flex items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          onClick={() => (paused ? sub.resume() : sub.pause())}
        >
          {paused ? "恢复" : "暂停"}
        </Button>
      </div>
      <EventFeed
      sub={sub}
      getKey={(frame) => frame.seq}
      render={(frame: EventFrame) => (
        <>
          <time className="w-[92px] shrink-0 font-mono" dateTime={frame.at.toISOString()}>
            {formatTimeOnly(frame.at, tz)}
          </time>
          <span className="w-[240px] shrink-0 truncate font-mono">
            {typeof frame.payload.event === "string" ? frame.payload.event : "event"}
          </span>
          <span className="min-w-0 flex-1 break-all font-mono text-muted-foreground">
            {summarize(frame.payload)}
          </span>
        </>
      )}
      emptyText="暂无事件。对该集合执行写操作后将在此实时显示。"
        resyncAction={
          <Button variant="outline" size="sm" onClick={() => sub.restart()}>
            <RefreshCw className="mr-2 h-4 w-4" />
            已断开，点击重连
          </Button>
        }
      />
    </div>
  );
}
