import { NavLink, Outlet } from "react-router-dom";
import { CircleUserRound, SlidersHorizontal, UserRound } from "lucide-react";
import { PageHeader } from "@/components/PageHeader";
import { cn } from "@/lib/utils";

// 账户设置区（/console/account/*）：Profile 与 Preferences 分离的子导航布局，
// 后续新增自助分区（如安全/会话）在此加页签与子路由即可。
const navLinkClass = ({ isActive }: { isActive: boolean }) =>
  cn(
    "inline-flex items-center gap-2 border-b-2 px-1 pb-3 pt-1 text-sm font-medium transition-colors",
    isActive
      ? "border-primary text-foreground"
      : "border-transparent text-muted-foreground hover:border-muted-foreground/40 hover:text-foreground"
  );

export function AccountLayout() {
  return (
    <div className="space-y-6">
      <PageHeader
        title="账户设置"
        description="管理你的账户资料与偏好"
        icon={CircleUserRound}
      />

      <nav className="-mb-px flex gap-6 border-b">
        <NavLink to="/console/account/profile" className={navLinkClass}>
          <UserRound className="h-4 w-4" />
          Profile
        </NavLink>
        <NavLink to="/console/account/preferences" className={navLinkClass}>
          <SlidersHorizontal className="h-4 w-4" />
          Preferences
        </NavLink>
      </nav>

      <Outlet />
    </div>
  );
}
