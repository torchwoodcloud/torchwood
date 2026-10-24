// vendored from fleet-ui registry/query/keyset-pagination.tsx @0.1.1（2026-10-24
// keyset 切片）。拷贝适配（design.md §12.1）+ 本仓三处本地裁决：
// 1) token 方言 text-muted→text-muted-foreground；文案中文化；
// 2) 上游无页大小切换（demo 方言）——补 size Select（旧页有，PAGE_SIZE_OPTIONS
//    与服务端 clamp 上限一致）；setWindow 保游标改页大小 = keyset 语义自洽
//    （游标=位置，size=从该位置取多少行）；
// 3) audit-logs 网关附带 total_count——有则显示总数，无则按上游"仅本页行数"
//    降级（其余 torchwood 列表端点无 total）。
// Prev 语义与上游一致：advance 可算则本地算，服务端不透明游标不可算 →
// window.history.back()——游标进 URL 后历史栈就是分页栈。
import type { CollectionHandle } from "@lynx-go/ui-kit/react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";

export interface KeysetPaginationProps<T> {
  list: CollectionHandle<T>;
}

export function KeysetPagination<T>({ list }: KeysetPaginationProps<T>) {
  const rows = list.rows.length;
  const hasMore = list.segment?.nextCursor !== undefined;
  const total = list.total;
  const size = list.query.window.size;
  // 首页 = 无游标无 offset；此时 Prev 无处可退（advance 返 null 且历史
  // back 会离开本页），显式禁用——上游块无此判断，本仓补。
  const onFirstPage =
    list.query.window.cursor === undefined && list.query.window.range === undefined;

  return (
    <div className="flex items-center justify-between gap-3 px-3 py-2 text-xs text-muted-foreground">
      <div>
        {total !== undefined
          ? `共 ${total} 条 · 本页 ${rows} 条`
          : rows === 0
            ? "本页无记录"
            : `本页 ${rows} 条${hasMore ? "" : "（已到底）"}`}
      </div>
      <div className="flex items-center gap-2">
        <Select
          value={String(size)}
          onValueChange={(v) => {
            list.setWindow({ size: Number(v) });
          }}
        >
          <SelectTrigger className="h-8 w-24" aria-label="页大小">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            {PAGE_SIZE_OPTIONS.map((option) => (
              <SelectItem key={option} value={String(option)}>
                {option} 条/页
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Button
          variant="outline"
          size="sm"
          disabled={onFirstPage}
          onClick={() => {
            if (!list.prev()) window.history.back();
          }}
        >
          <ChevronLeft className="size-3.5" /> 上一页
        </Button>
        <Button variant="outline" size="sm" disabled={!hasMore} onClick={() => list.next()}>
          下一页 <ChevronRight className="size-3.5" />
        </Button>
      </div>
    </div>
  );
}
