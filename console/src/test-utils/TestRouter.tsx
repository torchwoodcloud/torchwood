import {
  RouterProvider,
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
} from "@tanstack/react-router";
import { useMemo, type ReactNode } from "react";

// TestRouter：MemoryRouter+Routes+Route 声明式的 TanStack 替身（自 compat
// 退役迁入，§12.9-5）。routes 的 path 支持 ':param' 与 '$param' 双写；
// initialEntries 定位首地址。测试文件统一从 @/test-utils/TestRouter 导入。

function normalizeTestPath(path: string): string {
  return path.replace(/:([A-Za-z0-9_]+)/g, "$$$1").replace(/\/\*$/, "/$");
}

interface TestRouteSpec {
  path: string;
  element: ReactNode;
  /** 嵌套子路由（Layout/Outlet 形态测试）；path "/" = index */
  children?: ReadonlyArray<TestRouteSpec>;
}

export function TestRouter({
  routes,
  initialEntries = ["/"],
}: {
  routes: ReadonlyArray<TestRouteSpec>;
  initialEntries?: string[];
}) {
  const signature = `${initialEntries.join("|")};${routes.map((r) => r.path).join("|")}`;
  const router = useMemo(() => {
    const root = createRootRoute({});
    const children = routes.map((spec) => {
      const route = createRoute({
        getParentRoute: () => root,
        path: normalizeTestPath(spec.path),
        component: () => <>{spec.element}</>,
      });
      if (spec.children && spec.children.length > 0) {
        return route.addChildren(
          spec.children.map((child) =>
            createRoute({
              getParentRoute: () => route,
              path: normalizeTestPath(child.path),
              component: () => <>{child.element}</>,
            }),
          ),
        );
      }
      return route;
    });
    return createRouter({
      routeTree: root.addChildren(children),
      history: createMemoryHistory({ initialEntries }),
    });
    // 路由表/入口按签名缓存——element 闭包按首渲染快照（测试单次渲染语义）
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [signature]);

  return <RouterProvider router={router} />;
}

