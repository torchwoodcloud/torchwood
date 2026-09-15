import { useEffect, useRef, useState } from "react";
import { useOutletContext } from "react-router-dom";
import { Radio, RefreshCw } from "lucide-react";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatTimeOnly } from "@/lib/datetime";
import {
  startCollectionListener,
  type CollectionListener,
  type ListenEvent,
  type ListenStatus,
} from "@/api/realtime";
import type { CollectionOutletContext } from "./CollectionLayout";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

// 事件列表最多保留条数（试听场景只看最新事件，重连不补历史）。
const MAX_EVENTS = 100;

const statusText: Record<ListenStatus, string> = {
  connecting: "连接中…",
  connected: "已连接",
  disconnected: "已断开",
};

function summarize(payload: Record<string, unknown>): string {
  const text = JSON.stringify(payload);
  return text.length > 120 ? `${text.slice(0, 120)}…` : text;
}

// ListenPanel 集合「试听」面板（v2 设计 §4.2/§4.3）：cookie 握手绑当前
// project_id，订阅当前集合频道，展示事件流；15m 会话到期断线后自动走
// console auth refresh 再重连重订，refresh 失败显示「已断开，点击重连」。
export function ListenPanel() {
  const { dbId, collId } = useOutletContext<CollectionOutletContext>();
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const [status, setStatus] = useState<ListenStatus>("connecting");
  const [events, setEvents] = useState<ListenEvent[]>([]);
  const listenerRef = useRef<CollectionListener | null>(null);

  useEffect(() => {
    if (!projectId) return;
    const listener = startCollectionListener({
      projectId,
      channel: `databases.${dbId}.collections.${collId}`,
      onEvent: (ev) => setEvents((prev) => [ev, ...prev].slice(0, MAX_EVENTS)),
      onStatus: setStatus,
    });
    listenerRef.current = listener;
    return () => {
      listenerRef.current = null;
      listener.close();
    };
  }, [projectId, dbId, collId]);

  return (
    <div className="space-y-4">
      <div className="flex items-center gap-3">
        <Radio className="h-4 w-4 text-muted-foreground" />
        <span className="text-sm font-medium">实时事件</span>
        <Badge
          variant={
            status === "connected"
              ? "default"
              : status === "connecting"
                ? "secondary"
                : "destructive"
          }
        >
          {statusText[status]}
        </Badge>
        {status === "disconnected" && (
          <Button
            variant="outline"
            size="sm"
            onClick={() => listenerRef.current?.reconnect()}
          >
            <RefreshCw className="mr-2 h-4 w-4" />
            已断开，点击重连
          </Button>
        )}
      </div>

      {events.length === 0 ? (
        <p className="py-8 text-center text-sm text-muted-foreground">
          暂无事件。对该集合执行写操作后将在此实时显示。
        </p>
      ) : (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead className="w-[180px]">时间</TableHead>
              <TableHead className="w-[220px]">事件</TableHead>
              <TableHead>Payload</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {events.map((ev, i) => (
              <TableRow key={`${ev.at.getTime()}-${i}`}>
                <TableCell className="font-mono text-xs">
                  {formatTimeOnly(ev.at, tz)}
                </TableCell>
                <TableCell className="font-mono text-xs">
                  {typeof ev.payload.event === "string" ? ev.payload.event : "event"}
                </TableCell>
                <TableCell className="font-mono text-xs break-all">
                  {summarize(ev.payload)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </div>
  );
}
