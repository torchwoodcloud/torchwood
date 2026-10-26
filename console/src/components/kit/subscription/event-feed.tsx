// vendored from @lynx-go/ui registry/subscription/event-feed.tsx @0.2.1（2026-10-25 §12.10-⑤ 上游回流 re-vendor（labels 面））
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground、bg-panel→bg-card、border-line→border-border、divide-line→divide-border；cn 统一 @/lib/utils。
// 手工步骤（判断题，工具不自动改）：
// - 未映射方言 token text-warn ×2——每块语境判断（torchwood amber/emerald、fleetly status vars 先例）
// - 未映射方言 token bg-warn ×2——每块语境判断（torchwood amber/emerald、fleetly status vars 先例）
// - 未映射方言 token border-warn ×1——每块语境判断（torchwood amber/emerald、fleetly status vars 先例）
// - Badge tone= ×1——shadcn Badge 无 tone prop，需手工 variant+className 适配（参考 torchwood event-feed 先例）
// 其余与上游一致。
// 手工适配层（判断题，devtool 交接单）：① demo Badge tone → 本仓 shadcn
// variant（live=default、error=destructive、其余 secondary+amber 字色）；
// ② warn 系 token 本仓无 → amber 惯例；③ 瞬态文案经 labels 由 ListenPanel 注入。
import type { SubscriptionEngine } from "@lynx-go/uikit-core/react";
import { PauseCircle } from "lucide-react";
import type { ReactNode } from "react";
import { useSyncExternalStore } from "react";

function statusBadge(status: string): {
  variant: "default" | "secondary" | "destructive";
  className?: string;
} {
  if (status === "live") return { variant: "default" };
  if (status === "error") return { variant: "destructive" };
  return { variant: "secondary", className: "text-amber-700 dark:text-amber-400" };
}
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

/**
 * EventFeed（subscription 块）—— useSubscription 输出的参照实现（EventFeed↔Subscription）。
 * 倒序（最新在上）+ gapped 黄条（resyncs 信号）+ 暂停指示灯。
 * backlog 行与首个 live 帧的去重按 (cursor, id)——getKey 由页面给（events 的 seq、
 * documents changes 的 revision），本块不认业务键。
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
  /** gapped/stalled 黄条上的动作槽（页面的 resync 编排按钮） */
  resyncAction?: ReactNode;
  /** 头部标题；缺省 "Events" */
  title?: string;
  /** 空态文案槽（业务句归页面；缺省通用句） */
  emptyText?: string;
  /** 状态徽章文案注入（消费方语言）；缺省原样 status */
  statusLabels?: Partial<Record<string, string>>;
  /** 瞬态提示文案注入（gapped/stalled 黄条、暂停指示、计数） */
  labels?: Partial<EventFeedLabels>;
  className?: string;
}

export interface EventFeedLabels {
  gapGapped: string;
  gapStalled: string;
  paused: string;
  count: (n: number) => string;
}

const DEFAULT_LABELS: EventFeedLabels = {
  gapGapped: "Retention gap — re-anchor the backlog to continue.",
  gapStalled: "Backoff budget exhausted — reconnect or switch to polling fallback.",
  paused: "Paused — frames buffered from cursor on resume",
  count: (n) => `${n} events`,
};

export function EventFeed<F>({
  sub,
  backlog,
  getKey,
  render,
  resyncAction,
  title = "Events",
  emptyText = "No events yet",
  statusLabels,
  labels,
  className,
}: EventFeedProps<F>) {
  const t = { ...DEFAULT_LABELS, ...labels };
  const frames = useSyncExternalStore(sub.subscribeFrames, sub.getFrames, sub.getFrames);
  const state = useSyncExternalStore(sub.subscribeStatus, sub.getState, sub.getState);

  // 合并 backlog 前缀与 live 帧（同键去重：backlog 定位、live 值覆盖）
  const merged = new Map<string | number, F>();
  for (const row of backlog ?? []) merged.set(getKey(row), row);
  for (const frame of frames) merged.set(getKey(frame), frame);
  const rows = [...merged.values()].reverse(); // 最新在上

  const showGapBar = state.status === "gapped" || state.status === "stalled";
  const rowRender = render ?? ((frame: F) => String(frame));
  const statusLabel = statusLabels?.[state.status] ?? state.status;
  const badge = statusBadge(state.status);

  return (
    <div className={cn("overflow-hidden rounded-md border border-border", className)}>
      <div className="flex items-center justify-between gap-2 border-b border-border bg-card px-3 py-1.5">
        <div className="flex items-center gap-2">
          <span className="text-xs font-medium text-muted-foreground">{title}</span>
          <Badge
            variant={badge.variant}
            className={badge.className}
            data-testid="sub-status"
          >
            {statusLabel}
          </Badge>
          {state.resyncs > 0 && state.status !== "gapped" ? (
            <span
              className="rounded-full bg-amber-500/15 px-2 py-0.5 text-[11px] text-amber-700 dark:text-amber-400"
              data-testid="resynced-chip"
            >
              re-synced ×{state.resyncs}
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
              {t.paused}
            </span>
          ) : null}
          <span className="text-[11px] text-muted-foreground">{t.count(rows.length)}</span>
        </div>
      </div>
      {showGapBar ? (
        <div
          className="flex items-center justify-between gap-2 border-b border-amber-500/50 bg-amber-500/10 px-3 py-1.5 text-xs text-amber-700 dark:text-amber-400"
          data-testid="gap-bar"
          role="status"
        >
          <span>{state.status === "gapped" ? t.gapGapped : t.gapStalled}</span>
          {resyncAction}
        </div>
      ) : null}
      <ul className="max-h-96 divide-y divide-border overflow-y-auto">
        {rows.length === 0 ? (
          <li className="px-3 py-6 text-center text-xs text-muted-foreground">{emptyText}</li>
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
