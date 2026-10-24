// vendored from fleet-ui registry/query/list-page.tsx @0.1.1（2026-10-24 keyset 切片）
// 拷贝适配（design.md §12.1，fleetly 先例——不吃 fleet 块的页头钮位）：去
// title/description/actions 页头（本仓页面统一用 PageHeader）；容器改全宽。
// 四态骨架/appendix 语义与上游一致。
import type { CollectionHandle } from "@lynx-go/ui-kit/react";
import type { ReactNode } from "react";
import { Card } from "@/components/ui/card";
import { EmptyState, ErrorState, LoadingState } from "../values/states";

/**
 * ListPage（query 块）—— 列表页配方骨架：toolbar × 四态 × pagination × appendix。
 * 所有 part 都是 ReactNode（结构性逃生舱）：不满意任一部分就传自己的组件，
 * 只要消费 list 句柄。四态在此切换；DataTable 只渲染 success 表体。
 */
export interface ListPageProps<T> {
  toolbar?: ReactNode;
  pagination?: ReactNode;
  appendix?: ReactNode;
  list: CollectionHandle<T>;
  emptyTitle?: string;
  emptyDescription?: string;
  emptyAction?: ReactNode;
  children: ReactNode; // success 态内容（通常 <DataTable list={list} …/>）
}

export function ListPage<T>({
  toolbar,
  pagination,
  appendix,
  list,
  emptyTitle = "暂无数据",
  emptyDescription,
  emptyAction,
  children,
}: ListPageProps<T>) {
  return (
    <div className="w-full space-y-4">
      <Card>
        {toolbar}
        {list.status === "loading" ? (
          <LoadingState />
        ) : list.status === "error" ? (
          <ErrorState error={list.error!} onRetry={list.refetch} />
        ) : list.status === "empty" ? (
          <EmptyState title={emptyTitle} description={emptyDescription} action={emptyAction} />
        ) : (
          children
        )}
        {list.status !== "loading" && list.status !== "error" ? pagination : null}
      </Card>
      {appendix}
    </div>
  );
}
