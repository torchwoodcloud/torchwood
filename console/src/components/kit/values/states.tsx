// vendored from @lynx-go/ui registry/values/states.tsx @0.2.1（2026-10-25 §12.10 余款 re-vendor（describeError 面，内置中文 describe 退役））
// 拷贝适配（scripts/devtool.mjs --profile torchwood）：
// token 方言映射 text-muted→text-muted-foreground、text-err→text-destructive；cn 统一 @/lib/utils。
// 其余与上游一致。
import type { ApiError } from "@lynx-go/ui-kit/kernel";
import { RotateCw, SearchX } from "lucide-react";
import type { ReactNode } from "react";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";

/**
 * 四态的三个渲染分支（loading / empty / error；success 态由调用方渲染数据）。
 * 归 ListPage/页级使用——DataTable 只渲染 success 态的表体。
 */

export function LoadingState({ rows = 5 }: { rows?: number }) {
  return (
    <div className="space-y-2 p-4" aria-busy="true" aria-label="Loading">
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-9 w-full" />
      ))}
    </div>
  );
}

export interface EmptyStateProps {
  title: string;
  description?: string;
  action?: ReactNode;
}

export function EmptyState({ title, description, action }: EmptyStateProps) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 p-12 text-center">
      <SearchX className="size-6 text-muted-foreground" />
      <div className="text-sm font-medium">{title}</div>
      {description ? <div className="text-xs text-muted-foreground">{description}</div> : null}
      {action}
    </div>
  );
}

export interface ErrorStateProps {
  error: ApiError;
  /** 重试动作（refetch）；不传则只展示 */
  onRetry?: () => void;
  /** 失败文案注入（消费方 describeApiError 单源；缺省块内英文六分支） */
  describeError?: (error: ApiError) => string;
  title?: string;
  retryLabel?: string;
}

function describe(error: ApiError): string {
  switch (error.kind) {
    case "network":
      return "Network unreachable. Check your connection and retry.";
    case "auth":
      return "Session expired.";
    case "forbidden":
      return "You do not have access to this resource.";
    case "not-found":
      return "Resource not found.";
    case "decode":
      return `Unexpected response format (${error.detail}).`;
    case "server":
      return `Server error ${error.status}${error.message ? `: ${error.message}` : ""}.`;
  }
}

export function ErrorState({
  error,
  onRetry,
  describeError,
  title = "Something went wrong",
  retryLabel = "Retry",
}: ErrorStateProps) {
  return (
    <div className="flex flex-col items-center justify-center gap-3 p-12 text-center" role="alert">
      <div className="text-sm font-medium text-destructive">{title}</div>
      <div className="max-w-md text-xs text-muted-foreground">{describeError?.(error) ?? describe(error)}</div>
      {onRetry ? (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RotateCw className="size-3.5" /> {retryLabel}
        </Button>
      ) : null}
    </div>
  );
}
