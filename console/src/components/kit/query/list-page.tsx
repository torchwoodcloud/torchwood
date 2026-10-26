// vendored from @lynx-go/ui registry/query/list-page.tsx @0.2.1（2026-10-25 §12.10 余款 re-vendor（错误态文案透传面））
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground；cn 统一 @/lib/utils。
// 手工适配层（判断题）：①容器改全宽（本仓页面统一 PageHeader + 自有页宽）；
// ②错误态中文经 props 注入（errorTitle/describeError/retryLabel 缺省值），
// states 块保持上游纯拷贝。
import type { CollectionHandle } from "@lynx-go/uikit-core";
import type { ApiError } from "@lynx-go/uikit-core/kernel";
import { describeApiError } from "@/kit/errors";
import type { ReactNode } from "react";
import { Card } from "@/components/ui/card";
import { EmptyState, ErrorState, LoadingState } from "../values/states";

/**
 * ListPage（query 块）—— 列表页配方骨架：header × toolbar × 四态 × appendix。
 * 所有 part 都是 ReactNode（结构性逃生舱）：不满意任一部分就传自己的组件，
 * 只要消费 list 句柄。四态在此切换；DataTable 只渲染 success 表体。
 * header 可选（title/actions 全缺省时不渲染——消费方自有页头架构时
 * shell-agnostic 化，§12.10-⑤；fleetly/torchwood 先例回流）。
 * 错误态文案透传（describeError/title/retryLabel，§12.10 余款）：消费方
 * describeApiError 单源经此注入，states 块免于逐副本中文化。
 */
export interface ListPageProps<T> {
  title?: string;
  description?: string;
  actions?: ReactNode;
  toolbar?: ReactNode;
  pagination?: ReactNode;
  appendix?: ReactNode;
  list: CollectionHandle<T>;
  emptyTitle?: string;
  emptyDescription?: string;
  emptyAction?: ReactNode;
  describeError?: (error: ApiError) => string;
  errorTitle?: string;
  retryLabel?: string;
  children: ReactNode; // success 态内容（通常 <DataTable list={list} …/>）
}

export function ListPage<T>({
  title,
  description,
  actions,
  toolbar,
  pagination,
  appendix,
  list,
  emptyTitle = "暂无数据",
  emptyDescription,
  emptyAction,
  describeError,
  errorTitle = "出错了",
  retryLabel = "重试",
  children,
}: ListPageProps<T>) {
  const showHeader = title !== undefined || description !== undefined || actions !== undefined;
  return (
    <div className="w-full space-y-4">
      {showHeader ? (
        <header className="flex items-start justify-between gap-4">
          <div>
            {title ? <h1 className="text-lg font-semibold">{title}</h1> : null}
            {description ? <p className="mt-0.5 text-xs text-muted-foreground">{description}</p> : null}
          </div>
          {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
        </header>
      ) : null}
      <Card>
        {toolbar}
        {list.status === "loading" ? (
          <LoadingState />
        ) : list.status === "error" ? (
          <ErrorState
            error={list.error!}
            onRetry={list.refetch}
            describeError={describeError ?? describeApiError}
            title={errorTitle}
            retryLabel={retryLabel}
          />
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
