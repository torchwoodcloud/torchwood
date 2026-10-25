import { Component, type ReactNode } from "react";
import { Button } from "@/components/ui/button";

interface ErrorBoundaryProps {
  children: ReactNode;
}

interface ErrorBoundaryState {
  error: Error | null;
}

// RouteErrorFallback：错误呈现单源——类边界（children 捕获形态）与 TanStack
// errorComponent（路由挂载形态）共用同一 UI（router.tsx 路由换轨批引入）。
// error 收 unknown（TanStack ErrorComponentProps 契约），非 Error 归一为字符串。
export function RouteErrorFallback({ error }: { error: unknown }) {
  const message = error instanceof Error ? error.message : String(error);
  return (
    <div className="flex min-h-screen items-center justify-center p-6">
      <div className="max-w-md space-y-4 text-center">
        <p className="text-lg font-semibold">页面渲染出错</p>
        <p className="text-xs text-destructive break-all">{message}</p>
        <Button onClick={() => window.location.reload()}>刷新页面</Button>
      </div>
    </div>
  );
}

// 全局错误边界：捕获路由/页面渲染异常，避免单个页面崩溃导致整个 Console 白屏。
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  render() {
    if (this.state.error) {
      return <RouteErrorFallback error={this.state.error} />;
    }
    return this.props.children;
  }
}
