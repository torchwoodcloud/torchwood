import { Link as TanLink, useLocation } from "@tanstack/react-router";
import type { ReactNode } from "react";

// NavLink（app 级 UI 模式，§12.9-5）：TanStack Link 无 NavLink 形——本组件
// 以 pathname 判激活（end = 精确匹配；否则前缀匹配），className 支持静态串
// 与 ({isActive}) render-fn 两形态（侧栏/页签用法）。to 走类型化 Link。
export function NavLink({
  to,
  end,
  className,
  children,
  ...rest
}: {
  to: string;
  end?: boolean;
  className?: string | ((state: { isActive: boolean }) => string);
  children?: ReactNode;
  [key: string]: unknown;
}) {
  const pathname = useLocation().pathname;
  const active = end
    ? pathname === to
    : pathname === to || pathname.startsWith(`${to}/`);
  const resolved =
    typeof className === "function"
      ? (className as (state: { isActive: boolean }) => string)({ isActive: active })
      : className;
  return (
    <TanLink
      to={to as never}
      className={resolved}
      data-status={active ? "active" : undefined}
      {...(rest as object)}
    >
      {children}
    </TanLink>
  );
}
