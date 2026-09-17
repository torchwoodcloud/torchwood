import { createElement, type ReactNode } from "react";
import {
  BarChart3,
  Coins,
  CreditCard,
  Database,
  FolderKanban,
  FunctionSquare,
  HardDrive,
  Key,
  LayoutDashboard,
  Receipt,
  ScrollText,
  ShieldCheck,
  SlidersHorizontal,
  Trophy,
  Users,
  UsersRound,
  type LucideIcon,
} from "lucide-react";

export interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
}

export const navSections: { title?: string; items: NavItem[] }[] = [
  {
    items: [{ to: "/console", label: "Dashboard", icon: LayoutDashboard }],
  },
  {
    title: "Develop",
    items: [
      { to: "/console/api-keys", label: "API Keys", icon: Key },
      { to: "/console/databases", label: "Databases", icon: Database },
      { to: "/console/storage", label: "Storage", icon: HardDrive },
      { to: "/console/functions", label: "Functions", icon: FunctionSquare },
      // RuntimeVars：项目级运行时配置下发（集合/可见性/版本回滚），
      // 与 API Keys 同为凭证相邻的开发者资源。
      { to: "/console/runtime-vars", label: "Runtime Vars", icon: SlidersHorizontal },
      // Analytics：与 Databases/Storage/Functions 并列的一等公民服务
      // （docs/design/analytics.md §9，roadmap 独立一节）。
      { to: "/console/analytics", label: "Analytics", icon: BarChart3 },
    ],
  },
  {
    title: "Auth",
    items: [
      { to: "/console/users", label: "Users", icon: Users },
      { to: "/console/groups", label: "Groups", icon: UsersRound },
    ],
  },
  {
    title: "Economy",
    items: [
      { to: "/console/orders", label: "Orders", icon: Receipt },
      { to: "/console/assets", label: "Assets", icon: Coins },
      { to: "/console/subscriptions/plans", label: "Subscriptions", icon: CreditCard },
      { to: "/console/leaderboards", label: "Leaderboards", icon: Trophy },
    ],
  },
  {
    title: "System",
    items: [
      { to: "/console/projects", label: "Projects", icon: FolderKanban },
      { to: "/console/admins", label: "Admins", icon: ShieldCheck },
      { to: "/console/audit-logs", label: "Audit Logs", icon: ScrollText },
    ],
  },
];

// iconForPath 取与当前路径前缀匹配最深的导航项图标（页头图标随路由自动派生）。
export function iconForPath(pathname: string): LucideIcon | undefined {
  let best: NavItem | undefined;
  for (const section of navSections) {
    for (const item of section.items) {
      const matches = pathname === item.to || pathname.startsWith(`${item.to}/`);
      if (matches && (!best || item.to.length > best.to.length)) {
        best = item;
      }
    }
  }
  return best?.icon;
}

// pageIconElement 渲染页头图标元素；用 createElement 规避 eslint
// react-hooks/static-components 对"渲染期解析组件类型"的误报。
export function pageIconElement(
  pathname: string,
  className: string
): ReactNode {
  const Icon = iconForPath(pathname);
  return Icon ? createElement(Icon, { className }) : null;
}
