import type { router } from "./router";

// TanStack Router 类型注册（compat 退场的基石，§12.9-5）：注册后页面可直接
// import { Link, useNavigate } from "@tanstack/react-router" 且获得路由树
// 类型检查（to 面按真实路由收窄）。本文件只做模块增强，无运行时形状。
declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
