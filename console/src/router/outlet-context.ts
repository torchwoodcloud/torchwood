import { createContext, useContext } from "react";

// Outlet context（布局→子路由数据传递的 app 级形态，§12.9-5）：TanStack 走
// route context 不透传 props，本模块用 React context 复刻 <Outlet context>
// 语义。context 与 hook 居 .ts（react-refresh 只导出组件约束）；Outlet
// 组件在 outlet-context.tsx。

export const OutletContext = createContext<unknown>(null);

export function useOutletContext<T = unknown>(): T {
  return useContext(OutletContext) as T;
}
