import { Fragment, useState } from "react";
import { Link, NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/useAuth";
import { getCurrentAdmin } from "@/api/admins";
import { ProjectBootstrap } from "@/components/ProjectBootstrap";
import { ProjectSelector } from "@/components/ProjectSelector";
import { TimeBadge } from "@/components/TimeBadge";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { breadcrumbsFor } from "@/lib/routeTitles";
import { navSections } from "@/lib/nav";
import {
  ChevronRight,
  LogOut,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
  X,
} from "lucide-react";

// 桌面侧栏收起态持久化键：刷新后保持用户选择。
const SIDEBAR_COLLAPSED_KEY = "TORCHWOOD_console_sidebar_collapsed";

export function Layout() {
  const { logout } = useAuth();
  const navigate = useNavigate();
  const [mobileOpen, setMobileOpen] = useState(false);
  // 收起 = 图标栏（w-16）：仅保留图标/头像，hover 用 title 提示；移动端抽屉不受影响。
  const [collapsed, setCollapsed] = useState(
    () => localStorage.getItem(SIDEBAR_COLLAPSED_KEY) === "1"
  );
  // 当前管理员（共享 ["console-admin-me"] 缓存）：侧栏底部展示邮箱 + 账户设置入口。
  const { data: me } = useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
    retry: 1,
    staleTime: 60_000,
  });

  const handleLogout = async () => {
    await logout();
    navigate("/console/login");
  };

  const closeMobile = () => setMobileOpen(false);

  const toggleCollapsed = () =>
    setCollapsed((prev) => {
      localStorage.setItem(SIDEBAR_COLLAPSED_KEY, prev ? "0" : "1");
      return !prev;
    });

  return (
    <div className="flex h-screen bg-sidebar">
      <ProjectBootstrap />
      {/* Desktop sidebar */}
      <aside
        className={cn(
          "hidden shrink-0 flex-col border-r border-sidebar-border bg-sidebar transition-[width] duration-200 md:flex",
          collapsed ? "w-16" : "w-64"
        )}
      >
        <SidebarContent
          onNavigate={closeMobile}
          onLogout={handleLogout}
          email={me?.email}
          collapsed={collapsed}
          onToggleCollapsed={toggleCollapsed}
        />
      </aside>

      {/* Mobile overlay */}
      {mobileOpen && (
        <div
          className="fixed inset-0 z-40 bg-black/40 md:hidden"
          onClick={() => setMobileOpen(false)}
        />
      )}

      {/* Mobile sidebar */}
      <aside
        className={cn(
          "fixed inset-y-0 left-0 z-50 flex w-64 flex-col border-r border-sidebar-border bg-sidebar transition-transform duration-200 md:hidden",
          mobileOpen ? "translate-x-0" : "-translate-x-full"
        )}
      >
        <SidebarContent
          onNavigate={closeMobile}
          onLogout={handleLogout}
          email={me?.email}
        />
      </aside>

      <main className="flex min-h-0 min-w-0 flex-1 flex-col">
        <header className="flex h-16 shrink-0 items-center gap-2 px-4 md:px-6">
          <Button
            variant="ghost"
            size="icon"
            className="-ml-1 md:hidden"
            onClick={() => setMobileOpen(true)}
          >
            <Menu className="h-5 w-5" />
          </Button>
          <TopbarBreadcrumb />
          <div className="ml-auto flex items-center gap-2">
            <TimeBadge />
          </div>
        </header>
        <div className="min-h-0 flex-1 overflow-y-auto px-4 pb-4 md:px-6">
          <div className="min-h-[calc(100vh-5rem)] rounded-xl border bg-background p-4 shadow-sm md:p-6">
            <Outlet />
          </div>
        </div>
      </main>
    </div>
  );
}

function TopbarBreadcrumb() {
  const { pathname } = useLocation();
  const crumbs = breadcrumbsFor(pathname);

  return (
    <nav className="flex min-w-0 items-center gap-1.5 text-sm">
      {crumbs.map((crumb, idx) => {
        const isLast = idx === crumbs.length - 1;
        return (
          <Fragment key={crumb.to}>
            {idx > 0 && (
              <ChevronRight className="h-3.5 w-3.5 shrink-0 text-muted-foreground/60" />
            )}
            {isLast ? (
              <span className="truncate font-medium">{crumb.label}</span>
            ) : (
              <Link
                to={crumb.to}
                className="hidden truncate text-muted-foreground transition-colors hover:text-foreground sm:inline"
              >
                {crumb.label}
              </Link>
            )}
          </Fragment>
        );
      })}
    </nav>
  );
}

function SidebarContent({
  onNavigate,
  onLogout,
  email,
  collapsed = false,
  onToggleCollapsed,
}: {
  onNavigate: () => void;
  onLogout: () => void;
  email?: string;
  collapsed?: boolean;
  onToggleCollapsed?: () => void;
}) {
  const initial = (email ?? "?").slice(0, 1).toUpperCase();

  return (
    <>
      <div
        className={cn(
          "flex items-center gap-2.5 py-4",
          collapsed ? "justify-center px-3" : "px-4"
        )}
      >
        {collapsed ? (
          <Button
            variant="ghost"
            size="icon"
            className="text-muted-foreground"
            onClick={onToggleCollapsed}
            title="展开菜单"
          >
            <PanelLeftOpen className="h-5 w-5" />
          </Button>
        ) : (
          <>
            <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-sidebar-primary text-xs font-semibold text-sidebar-primary-foreground">
              T
            </span>
            <span className="flex min-w-0 flex-1 flex-col">
              <span className="truncate text-sm font-medium leading-tight">Torchwood</span>
              <span className="truncate text-[11px] leading-tight text-muted-foreground">
                Console
              </span>
            </span>
            {onToggleCollapsed && (
              <Button
                variant="ghost"
                size="icon"
                className="-mr-1 shrink-0 text-muted-foreground"
                onClick={onToggleCollapsed}
                title="收起菜单"
              >
                <PanelLeftClose className="h-4 w-4" />
              </Button>
            )}
            <Button variant="ghost" size="icon" className="-mr-1 md:hidden" onClick={onNavigate}>
              <X className="h-5 w-5" />
            </Button>
          </>
        )}
      </div>
      <div className="px-3 pb-2">
        <ProjectSelector collapsed={collapsed} />
      </div>
      <nav
        className={cn(
          "min-h-0 flex-1 space-y-4 overflow-y-auto py-2",
          collapsed ? "px-2" : "px-3"
        )}
      >
        {navSections.map((section) => (
          <div key={section.title ?? "main"}>
            {section.title && !collapsed && (
              <div className="px-2 pb-1 text-xs font-medium text-muted-foreground">
                {section.title}
              </div>
            )}
            <div className="space-y-0.5">
              {section.items.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.to === "/console"}
                  onClick={onNavigate}
                  title={collapsed ? item.label : undefined}
                  className={({ isActive }) =>
                    cn(
                      "flex h-8 items-center gap-2 rounded-lg text-sm font-medium transition-colors",
                      collapsed ? "justify-center px-0" : "px-2",
                      isActive
                        ? "bg-sidebar-accent text-sidebar-accent-foreground"
                        : "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground"
                    )
                  }
                >
                  <item.icon className="h-4 w-4 shrink-0" />
                  {!collapsed && <span className="truncate">{item.label}</span>}
                </NavLink>
              ))}
            </div>
          </div>
        ))}
      </nav>
      <div
        className={cn(
          "space-y-0.5 border-t border-sidebar-border",
          collapsed ? "p-2" : "p-3"
        )}
      >
        <NavLink
          to="/console/account/profile"
          onClick={onNavigate}
          title={collapsed ? (email ?? "账户设置") : undefined}
          className={({ isActive }) =>
            cn(
              "flex w-full items-center gap-2 rounded-lg text-left transition-colors hover:bg-sidebar-accent",
              collapsed ? "justify-center px-0 py-1.5" : "px-2 py-1.5",
              isActive && "bg-sidebar-accent"
            )
          }
        >
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-sidebar-accent text-xs font-medium">
            {initial}
          </span>
          {!collapsed && (
            <>
              <span className="flex min-w-0 flex-1 flex-col">
                <span className="truncate text-sm font-medium leading-tight" title={email}>
                  {email ?? "…"}
                </span>
                <span className="truncate text-[11px] leading-tight text-muted-foreground">
                  账户设置
                </span>
              </span>
              <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
            </>
          )}
        </NavLink>
        <Button
          variant="ghost"
          size="sm"
          title="Logout"
          className={cn(
            "w-full text-muted-foreground",
            collapsed ? "justify-center px-0" : "justify-start gap-2"
          )}
          onClick={onLogout}
        >
          <LogOut className="h-4 w-4" />
          {!collapsed && "Logout"}
        </Button>
      </div>
    </>
  );
}
