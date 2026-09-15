import { useState } from "react";
import { Link, NavLink, Outlet, useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";
import { useAuth } from "@/hooks/useAuth";
import { getCurrentAdmin } from "@/api/admins";
import { ProjectBootstrap } from "@/components/ProjectBootstrap";
import { ProjectSelector } from "@/components/ProjectSelector";
import { PreferencesDialog } from "@/components/PreferencesDialog";
import { Button } from "@/components/ui/button";
import { LayoutDashboard, Key, Users, Database, HardDrive, LogOut, Menu, X, UsersRound, ShieldCheck, FolderKanban, FunctionSquare, BarChart3, Receipt, Coins, CreditCard, ScrollText, Trophy, Settings2, type LucideIcon } from "lucide-react";

interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
}

const navSections: { title?: string; items: NavItem[] }[] = [
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

export function Layout() {
  const { logout } = useAuth();
  const navigate = useNavigate();
  const [mobileOpen, setMobileOpen] = useState(false);
  const [prefsOpen, setPrefsOpen] = useState(false);
  // 当前管理员（共享 ["console-admin-me"] 缓存）：侧栏底部展示邮箱 + 偏好入口。
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
    <div className="flex h-screen bg-background">
      <ProjectBootstrap />
      <PreferencesDialog open={prefsOpen} onOpenChange={setPrefsOpen} current={me?.timezone} />
      {/* Desktop sidebar */}
      <aside className="hidden md:flex w-64 border-r bg-card flex-col">
        <SidebarContent
          onNavigate={closeMobile}
          onLogout={handleLogout}
          email={me?.email}
          onOpenPrefs={() => setPrefsOpen(true)}
        />
      </aside>

      {/* Mobile overlay */}
      {mobileOpen && (
        <div
          className="fixed inset-0 z-40 bg-black/50 md:hidden"
          onClick={() => setMobileOpen(false)}
        />
      )}

      {/* Mobile sidebar */}
      <aside
        className={`fixed inset-y-0 left-0 z-50 w-64 border-r bg-card flex-col transform transition-transform duration-200 md:hidden ${
          mobileOpen ? "translate-x-0" : "-translate-x-full"
        }`}
      >
        <SidebarContent
          onNavigate={closeMobile}
          onLogout={handleLogout}
          email={me?.email}
          onOpenPrefs={() => setPrefsOpen(true)}
        />
      </aside>

      <main className="flex-1 overflow-auto">
        <div className="flex items-center justify-between border-b bg-card px-4 py-3 md:hidden">
          <Link to="/console" className="text-lg font-bold tracking-tight">
            Torchwood Console
          </Link>
          <Button variant="ghost" size="icon" onClick={() => setMobileOpen(true)}>
            <Menu className="h-5 w-5" />
          </Button>
        </div>
        <div className="p-4 md:p-8">
          <Outlet />
        </div>
      </main>
    </div>
  );
}

function SidebarContent({
  onNavigate,
  onLogout,
  email,
  onOpenPrefs,
}: {
  onNavigate: () => void;
  onLogout: () => void;
  email?: string;
  onOpenPrefs: () => void;
}) {
  return (
    <>
      <div className="flex items-center justify-between p-6 border-b">
        <Link to="/console" className="text-xl font-bold tracking-tight">
          Torchwood Console
        </Link>
        <Button variant="ghost" size="icon" className="md:hidden" onClick={onNavigate}>
          <X className="h-5 w-5" />
        </Button>
      </div>
      <div className="px-6 pt-4 pb-4 border-b">
        <ProjectSelector />
      </div>
      <nav className="flex-1 p-4 space-y-1">
        {navSections.map((section) => (
          <div key={section.title ?? "main"} className="space-y-1">
            {section.title && (
              <div className="px-3 pt-4 pb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground/70">
                {section.title}
              </div>
            )}
            {section.items.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.to === "/console"}
                onClick={onNavigate}
                className={({ isActive }) =>
                  `flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors ${
                    isActive
                      ? "bg-primary text-primary-foreground"
                      : "text-muted-foreground hover:bg-muted hover:text-foreground"
                  }`
                }
              >
                <item.icon className="h-4 w-4" />
                {item.label}
              </NavLink>
            ))}
          </div>
        ))}
      </nav>
      <div className="p-4 border-t space-y-1">
        <div className="flex items-center gap-2 px-3 py-1.5 text-sm text-muted-foreground">
          <span className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-xs font-medium text-primary">
            {(email ?? "?").slice(0, 1).toUpperCase()}
          </span>
          <span className="truncate" title={email}>
            {email ?? "…"}
          </span>
        </div>
        <Button variant="ghost" className="w-full justify-start gap-2" onClick={onOpenPrefs}>
          <Settings2 className="h-4 w-4" />
          偏好设置
        </Button>
        <Button variant="ghost" className="w-full justify-start gap-2" onClick={onLogout}>
          <LogOut className="h-4 w-4" />
          Logout
        </Button>
      </div>
    </>
  );
}
