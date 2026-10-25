// vendored from fleet-ui registry/subscription/event-feed.tsx @0.1.1（2026-10-25 订阅族切片）
// 拷贝适配（design.md §12.4-6）：token 方言映射 border-line→border-border、
// bg-panel→bg-card、text-muted→text-muted-foreground、divide-line→divide-border、
// warn 系→amber 系（torchwood 无 warn token）；demo Badge tone→shadcn variant
// （live=default、error=destructive、其余 secondary+amber 字色）；文案中文化；
// 本地扩展 emptyText 空态文案槽（上游改进候选——空态业务句归页面）。其余与上游一致。
import type { SubscriptionEngine } from "@lynx-go/ui-kit/react";
import { PauseCircle } from "lucide-react";
import type { ReactNode } from "react";
import { useSyncExternalStore } from "react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

const STATUS_LABEL: Record<string, string> = {
  live: "已连接",
  reconnecting: "连接中…",
  paused: "已暂停",
  gapped: "保留间隙",
  stalled: "已断开",
  ended: "已结束",
  error: "错误",
};

function statusBadge(status: string): { variant: "default" | "secondary" | "destructive"; className?: string } {
  if (status === "live") return { variant: "default" };
  if (status === "error") return { variant: "destructive" };
  return { variant: "secondary", className: "text-amber-700 dark:text-amber-400" };
}

/**
 * EventFeed（subscription 块）—— useSubscription 输出的参照实现（EventFeed↔Subscription）。
 * 倒序（最新在上）+ gapped/stalled 提示条（resyncs 信号 / resyncAction 槽）+ 暂停指示灯。
 * backlog 行与首个 live 帧的去重按 getKey——本块不认业务键。
 */

export interface EventFeedProps<F> {
  sub: SubscriptionEngine<F>;
  /**
   * backlog Query 的行（静态前缀；与 live 帧按 getKey 去重，锚点交接见 design.md §2.3）。
   * 约定时间正序（旧→新）传入——collection 页通常最新在前，方言语序翻转由页面吸收。
   */
  backlog?: ReadonlyArray<F>;
  /** 帧去重键（cursor/id 复合语义由调用方吸收进单一键） */
  getKey: (frame: F) => string | number;
  /** 行渲染；缺省 String(frame) */
  render?: (frame: F) => ReactNode;
  /** gapped/stalled 提示条上的动作槽（页面的重连/resync 编排按钮） */
  resyncAction?: ReactNode;
  /** 空态文案（本地扩展槽：业务句归页面；缺省「暂无事件」） */
  emptyText?: string;
  className?: string;
}

export function EventFeed<F>({
  sub,
  backlog,
  getKey,
  render,
  resyncAction,
  emptyText,
  className,
}: EventFeedProps<F>) {
  const frames = useSyncExternalStore(sub.subscribeFrames, sub.getFrames, sub.getFrames);
  const state = useSyncExternalStore(sub.subscribeStatus, sub.getState, sub.getState);

  // 合并 backlog 前缀与 live 帧（同键去重：backlog 定位、live 值覆盖）
  const merged = new Map<string | number, F>();
  for (const row of backlog ?? []) merged.set(getKey(row), row);
  for (const frame of frames) merged.set(getKey(frame), frame);
  const rows = [...merged.values()].reverse(); // 最新在上

  const showGapBar = state.status === "gapped" || state.status === "stalled";
  const rowRender = render ?? ((frame: F) => String(frame));
  const badge = statusBadge(state.status);

  return (
    <div className={cn("overflow-hidden rounded-md border border-border", className)}>
      <div className="flex items-center justify-between gap-2 border-b border-border bg-card px-3 py-1.5">
        <div className="flex items-center gap-2">
          <span className="text-xs font-medium text-muted-foreground">实时事件</span>
          <Badge variant={badge.variant} className={badge.className} data-testid="sub-status">
            {STATUS_LABEL[state.status]}
          </Badge>
          {state.resyncs > 0 && state.status !== "gapped" ? (
            <span
              className="rounded-full bg-amber-500/15 px-2 py-0.5 text-[11px] text-amber-700 dark:text-amber-400"
              data-testid="resynced-chip"
            >
              已重锚 ×{state.resyncs}
            </span>
          ) : null}
        </div>
        <div className="flex items-center gap-2">
          {state.status === "paused" ? (
            <span
              className="flex items-center gap-1 text-[11px] text-muted-foreground"
              data-testid="paused-indicator"
            >
              <PauseCircle className="size-3.5" />
              已暂停——恢复后自游标补帧
            </span>
          ) : null}
          <span className="text-[11px] text-muted-foreground">{rows.length} 条事件</span>
        </div>
      </div>
      {showGapBar ? (
        <div
          className="flex items-center justify-between gap-2 border-b border-amber-500/50 bg-amber-500/10 px-3 py-1.5 text-xs text-amber-700 dark:text-amber-400"
          data-testid="gap-bar"
          role="status"
        >
          <span>
            {state.status === "gapped"
              ? "保留窗出现间隙——重锚后继续。"
              : "重连预算已耗尽——请手动重连。"}
          </span>
          {resyncAction}
        </div>
      ) : null}
      <ul className="max-h-96 divide-y divide-border overflow-y-auto">
        {rows.length === 0 ? (
          <li className="px-3 py-6 text-center text-xs text-muted-foreground">
            {emptyText ?? "暂无事件"}
          </li>
        ) : (
          rows.map((frame) => (
            <li
              key={getKey(frame)}
              className="flex items-center gap-3 px-3 py-1.5 text-xs"
              data-key={getKey(frame)}
            >
              {rowRender(frame)}
            </li>
          ))
        )}
      </ul>
    </div>
  );
}
