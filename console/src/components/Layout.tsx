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
import { ChevronRight, LogOut, Menu, X } from "lucide-react";

export function Layout() {
  const { logout } = useAuth();
  const navigate = useNavigate();
  const [mobileOpen, setMobileOpen] = useState(false);
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

  return (
    <div className="flex h-screen bg-sidebar">
      <ProjectBootstrap />
      {/* Desktop sidebar */}
      <aside className="hidden w-64 shrink-0 flex-col border-r border-sidebar-border bg-sidebar md:flex">
        <SidebarContent
          onNavigate={closeMobile}
          onLogout={handleLogout}
          email={me?.email}
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
}: {
  onNavigate: () => void;
  onLogout: () => void;
  email?: string;
}) {
  const initial = (email ?? "?").slice(0, 1).toUpperCase();

  return (
    <>
      <div className="flex items-center gap-2.5 px-4 py-4">
        <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-sidebar-primary text-xs font-semibold text-sidebar-primary-foreground">
          T
        </span>
        <span className="flex min-w-0 flex-1 flex-col">
          <span className="truncate text-sm font-medium leading-tight">Torchwood</span>
          <span className="truncate text-[11px] leading-tight text-muted-foreground">
            Console
          </span>
        </span>
        <Button variant="ghost" size="icon" className="-mr-1 md:hidden" onClick={onNavigate}>
          <X className="h-5 w-5" />
        </Button>
      </div>
      <div className="px-3 pb-2">
        <ProjectSelector />
      </div>
      <nav className="min-h-0 flex-1 space-y-4 overflow-y-auto px-3 py-2">
        {navSections.map((section) => (
          <div key={section.title ?? "main"}>
            {section.title && (
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
                  className={({ isActive }) =>
                    cn(
                      "flex h-8 items-center gap-2 rounded-lg px-2 text-sm font-medium transition-colors",
                      isActive
                        ? "bg-sidebar-accent text-sidebar-accent-foreground"
                        : "text-muted-foreground hover:bg-sidebar-accent/60 hover:text-foreground"
                    )
                  }
                >
                  <item.icon className="h-4 w-4 shrink-0" />
                  <span className="truncate">{item.label}</span>
                </NavLink>
              ))}
            </div>
          </div>
        ))}
      </nav>
      <div className="space-y-0.5 border-t border-sidebar-border p-3">
        <NavLink
          to="/console/account/profile"
          onClick={onNavigate}
          className={({ isActive }) =>
            cn(
              "flex w-full items-center gap-2 rounded-lg px-2 py-1.5 text-left transition-colors hover:bg-sidebar-accent",
              isActive && "bg-sidebar-accent"
            )
          }
        >
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-sidebar-accent text-xs font-medium">
            {initial}
          </span>
          <span className="flex min-w-0 flex-1 flex-col">
            <span className="truncate text-sm font-medium leading-tight" title={email}>
              {email ?? "…"}
            </span>
            <span className="truncate text-[11px] leading-tight text-muted-foreground">
              账户设置
            </span>
          </span>
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
        </NavLink>
        <Button
          variant="ghost"
          size="sm"
          className="w-full justify-start gap-2 text-muted-foreground"
          onClick={onLogout}
        >
          <LogOut className="h-4 w-4" />
          Logout
        </Button>
      </div>
    </>
  );
}
