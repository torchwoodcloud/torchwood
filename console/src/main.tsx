import { StrictMode, type ReactNode } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ResourceScopeProvider } from "@lynx-go/ui-kit";
import { RouterProvider } from "@tanstack/react-router";
import { Toaster } from "sonner";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { router } from "./router";
// 路由类型注册（类型副作用导入——必须与 router 同批加载）
import "./router.register";
import "./index.css";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        {/* scope 线程单源（§12.10-③）：声明了 scope 键的资源绑定从这里取值；
            projectId 未就绪 ⇒ 查询自动停用（fail-safe） */}
        <KitScopeBinding>
          <ErrorBoundary>
          {/* lazy 页面的 Suspense 在路由树根组件内（router.tsx），加载中文案不变 */}
          <RouterProvider router={router} />
          </ErrorBoundary>
          <Toaster position="top-right" />
        </KitScopeBinding>
      </AuthProvider>
    </QueryClientProvider>
  </StrictMode>
);


/** 应用根的 scope 线程绑定：AuthProvider 的当前项目 → ResourceScopeProvider。 */
function KitScopeBinding({ children }: { children: ReactNode }) {
  const { projectId } = useAuth();
  return (
    <ResourceScopeProvider value={{ projectId: projectId ?? undefined }}>
      {children}
    </ResourceScopeProvider>
  );
}
