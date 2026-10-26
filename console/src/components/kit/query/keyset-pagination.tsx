// vendored from @lynx-go/ui registry/query/keyset-pagination.tsx @0.2.1（2026-10-25 §12.10-⑤ 上游回流 re-vendor）
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground、bg-panel→bg-card、border-line→border-border、text-fg→text-foreground；cn 统一 @/lib/utils。
// 其余与上游一致。
import type { CollectionHandle } from "@lynx-go/uikit-core/react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "@/components/ui/button";

/**
 * KeysetPagination（query 块）—— keyset 方言：有 total（Segment.total）则显示
 * 总数，无则仅本页行数（torchwood 全站实证无 total；audit-logs 有——降级展示，
 * §12.10-⑤ 回流）；空尾页仍渲染分页栏（只能靠上一页退出）；
 * Next 依赖 segment.nextCursor（空页停发令牌=到底）；
 * Prev：advance 可算则本地算（客户端伪造游标），不可算（服务端不透明游标）走浏览器
 * 历史——游标进 URL 后历史栈就是分页栈。首页（无游标无 offset）显式禁用 Prev：
 * 无处可退，历史 back 会离开本页（上游 bug，torchwood 实证回流）。
 *
 * pageSize：可选页大小切换（keyset 语义自洽——游标=位置，size=从该位置取多少行；
 * setWindow({size}) 经 URL 补丁保游标）。选项经 prop 注入（消费方持服务端 clamp
 * 上限的约定）；labels 注入消费方文案（默认英文）。
 */
export interface KeysetPaginationLabels {
  describe: (info: { rows: number; total?: number; hasMore: boolean }) => string;
  prev: string;
  next: string;
  pageSizeAria: string;
  pageSizeOption: (size: number) => string;
}

const DEFAULT_LABELS: KeysetPaginationLabels = {
  describe: ({ rows, total, hasMore }) =>
    total !== undefined
      ? `${total} total · ${rows} this page`
      : rows === 0
        ? "No rows this page"
        : `Showing ${rows} rows${hasMore ? "" : " (end)"}`,
  prev: "Prev",
  next: "Next",
  pageSizeAria: "Page size",
  pageSizeOption: (size) => `${size} / page`,
};

export interface KeysetPaginationProps<T> {
  list: CollectionHandle<T>;
  /** 页大小选项（如 [20, 50, 100]）；不传则不渲染页大小切换 */
  pageSizeOptions?: ReadonlyArray<number>;
  labels?: Partial<KeysetPaginationLabels>;
}

export function KeysetPagination<T>({
  list,
  pageSizeOptions,
  labels,
}: KeysetPaginationProps<T>) {
  const rows = list.rows.length;
  const hasMore = list.segment?.nextCursor !== undefined;
  const total = list.total;
  const size = list.query.window.size;
  const onFirstPage =
    list.query.window.cursor === undefined && list.query.window.range === undefined;
  const t = { ...DEFAULT_LABELS, ...labels };

  return (
    <div className="flex items-center justify-between gap-3 px-3 py-2 text-xs text-muted-foreground">
      <div>{t.describe({ rows, total, hasMore })}</div>
      <div className="flex items-center gap-2">
        {pageSizeOptions && pageSizeOptions.length > 0 ? (
          /* 原生 select（fields 块先例）：页大小微控件不耦合消费方 Select 基元方言 */
          <select
            className="h-8 w-24 rounded-md border border-border bg-card px-2 text-foreground focus-visible:outline-2 focus-visible:outline-accent"
            aria-label={t.pageSizeAria}
            value={String(size)}
            onChange={(e) => {
              const next = Number(e.target.value);
              if (Number.isFinite(next) && next > 0) list.setWindow({ size: next });
            }}
          >
            {pageSizeOptions.map((option) => (
              <option key={option} value={String(option)}>
                {t.pageSizeOption(option)}
              </option>
            ))}
          </select>
        ) : null}
        <Button
          variant="outline"
          size="sm"
          disabled={onFirstPage}
          onClick={() => {
            if (!list.prev()) window.history.back();
          }}
        >
          <ChevronLeft className="size-3.5" /> {t.prev}
        </Button>
        <Button variant="outline" size="sm" disabled={!hasMore} onClick={() => list.next()}>
          {t.next} <ChevronRight className="size-3.5" />
        </Button>
      </div>
    </div>
  );
}
