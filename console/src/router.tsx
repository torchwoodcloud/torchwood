import { lazy, Suspense, type ReactNode } from "react";
import {
  createRootRoute,
  createRoute,
  createRouter,
  Navigate,
  Outlet,
} from "@tanstack/react-router";
import { RouteErrorFallback } from "@/components/ErrorBoundary";
import { Layout } from "@/components/Layout";
import { useAuth } from "@/hooks/useAuth";
import { canWrite, isPlatformAdmin, useAdminRole } from "@/hooks/useAdminRole";
import { auditLogsSearch } from "@/fleet/auditLogs";
import { apiKeysSearch } from "@/fleet/apiKeys";

// 路由树（torchwood 批 A，design.md §12.2 裁决 1）：TanStack Router code-based
// 集中表——镜像原 App.tsx 路由表，路径形态/守卫语义/lazy 分包零变化。
// 守卫保留组件形态（RequireAuth/RequireRole）：auth 是 React context
// （AuthProvider + useQuery），beforeLoad 无 React 上下文可用——beforeLoad 化
// 随 compat 退场批次做（裁决 2 的实现期修订）。每路由错误兜底从
// RouteErrorBoundary key=pathname 技巧改为 TanStack 原生 errorComponent。
//
// 页面 lazy 保持 React.lazy + 根级 Suspense（加载中文案不变）。

const Login = lazy(() => import("@/routes/Login").then((m) => ({ default: m.Login })));
const Dashboard = lazy(() => import("@/routes/Dashboard").then((m) => ({ default: m.Dashboard })));
const ProjectsListPage = lazy(() => import("@/routes/projects/pages").then((m) => ({ default: m.ProjectsListPage })));
const ProjectNewPage = lazy(() => import("@/routes/projects/pages").then((m) => ({ default: m.ProjectNewPage })));
const ProjectDetailPage = lazy(() => import("@/routes/projects/pages").then((m) => ({ default: m.ProjectDetailPage })));
const ProjectSettingsPage = lazy(() => import("@/routes/projects/settings").then((m) => ({ default: m.ProjectSettingsPage })));
const SettingsRedirect = lazy(() => import("@/routes/settings/pages").then((m) => ({ default: m.SettingsRedirect })));
const AccountLayout = lazy(() => import("@/routes/account/AccountLayout").then((m) => ({ default: m.AccountLayout })));
const AccountProfilePage = lazy(() => import("@/routes/account/pages").then((m) => ({ default: m.AccountProfilePage })));
const AccountPreferencesPage = lazy(() => import("@/routes/account/pages").then((m) => ({ default: m.AccountPreferencesPage })));
const ApiKeysListPage = lazy(() => import("@/routes/api-keys/pages").then((m) => ({ default: m.ApiKeysListPage })));
const ApiKeyNewPage = lazy(() => import("@/routes/api-keys/pages").then((m) => ({ default: m.ApiKeyNewPage })));
const ApiKeyDetailPage = lazy(() => import("@/routes/api-keys/pages").then((m) => ({ default: m.ApiKeyDetailPage })));
const UsersListPage = lazy(() => import("@/routes/users/pages").then((m) => ({ default: m.UsersListPage })));
const UserDetailPage = lazy(() => import("@/routes/users/pages").then((m) => ({ default: m.UserDetailPage })));
const UserEditPage = lazy(() => import("@/routes/users/pages").then((m) => ({ default: m.UserEditPage })));
const CreateUserPage = lazy(() => import("@/routes/users/pages").then((m) => ({ default: m.CreateUserPage })));
const StorageListPage = lazy(() => import("@/routes/storage/pages").then((m) => ({ default: m.StorageListPage })));
const BucketNewPage = lazy(() => import("@/routes/storage/pages").then((m) => ({ default: m.BucketNewPage })));
const BucketDetailPage = lazy(() => import("@/routes/storage/pages").then((m) => ({ default: m.BucketDetailPage })));
const FileDetailPage = lazy(() => import("@/routes/storage/pages").then((m) => ({ default: m.FileDetailPage })));
const FunctionsListPage = lazy(() => import("@/routes/functions/pages").then((m) => ({ default: m.FunctionsListPage })));
const FunctionDetailPage = lazy(() => import("@/routes/functions/pages").then((m) => ({ default: m.FunctionDetailPage })));
const WebhooksListPage = lazy(() => import("@/routes/webhooks/pages").then((m) => ({ default: m.WebhooksListPage })));
const WebhookDetailPage = lazy(() => import("@/routes/webhooks/pages").then((m) => ({ default: m.WebhookDetailPage })));
const VarSetsListPage = lazy(() => import("@/routes/runtime-vars/pages").then((m) => ({ default: m.VarSetsListPage })));
const VarSetDetailPage = lazy(() => import("@/routes/runtime-vars/pages").then((m) => ({ default: m.VarSetDetailPage })));
const GroupsListPage = lazy(() => import("@/routes/groups/pages").then((m) => ({ default: m.GroupsListPage })));
const GroupNewPage = lazy(() => import("@/routes/groups/pages").then((m) => ({ default: m.GroupNewPage })));
const GroupDetailPage = lazy(() => import("@/routes/groups/pages").then((m) => ({ default: m.GroupDetailPage })));
const DatabasesListPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.DatabasesListPage })));
const DatabaseNewPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.DatabaseNewPage })));
const DatabaseDetailPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.DatabaseDetailPage })));
const CollectionNewPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.CollectionNewPage })));
const CollectionDetailPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.CollectionDetailPage })));
const DocumentsListPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.DocumentsListPage })));
const DocumentNewPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.DocumentNewPage })));
const DocumentDetailPage = lazy(() => import("@/routes/databases/pages").then((m) => ({ default: m.DocumentDetailPage })));
const CollectionLayout = lazy(() => import("@/routes/databases/CollectionLayout").then((m) => ({ default: m.CollectionLayout })));
const ListenPanel = lazy(() => import("@/routes/databases/ListenPanel").then((m) => ({ default: m.ListenPanel })));
const AnalyticsOverviewPage = lazy(() => import("@/routes/analytics/pages").then((m) => ({ default: m.AnalyticsOverviewPage })));
const AnalyticsEventsPage = lazy(() => import("@/routes/analytics/pages").then((m) => ({ default: m.AnalyticsEventsPage })));
const AnalyticsRetentionPage = lazy(() => import("@/routes/analytics/pages").then((m) => ({ default: m.AnalyticsRetentionPage })));
const AnalyticsEventDetailPage = lazy(() => import("@/routes/analytics/EventDetailPage").then((m) => ({ default: m.AnalyticsEventDetailPage })));
const AnalyticsUserActivityPage = lazy(() => import("@/routes/analytics/UserActivityPage").then((m) => ({ default: m.AnalyticsUserActivityPage })));
const AdminsListPage = lazy(() => import("@/routes/admins/pages").then((m) => ({ default: m.AdminsListPage })));
const LeaderboardsListPage = lazy(() => import("@/routes/leaderboards/pages").then((m) => ({ default: m.LeaderboardsListPage })));
const LeaderboardBoardDetailPage = lazy(() => import("@/routes/leaderboards/pages").then((m) => ({ default: m.BoardDetailPage })));
const AuditLogsListPage = lazy(() => import("@/routes/audit-logs/pages").then((m) => ({ default: m.AuditLogsListPage })));
const OrdersListPage = lazy(() => import("@/routes/payments/pages").then((m) => ({ default: m.OrdersListPage })));
const OrderDetailPage = lazy(() => import("@/routes/payments/pages").then((m) => ({ default: m.OrderDetailPage })));
const AssetDefsListPage = lazy(() => import("@/routes/assets/pages").then((m) => ({ default: m.AssetDefsListPage })));
const AssetDefNewPage = lazy(() => import("@/routes/assets/pages").then((m) => ({ default: m.AssetDefNewPage })));
const AssetDefDetailPage = lazy(() => import("@/routes/assets/pages").then((m) => ({ default: m.AssetDefDetailPage })));
const UserAssetsPage = lazy(() => import("@/routes/assets/pages").then((m) => ({ default: m.UserAssetsPage })));
const PlansListPage = lazy(() => import("@/routes/subscriptions/pages").then((m) => ({ default: m.PlansListPage })));
const PlanNewPage = lazy(() => import("@/routes/subscriptions/pages").then((m) => ({ default: m.PlanNewPage })));
const PlanDetailPage = lazy(() => import("@/routes/subscriptions/pages").then((m) => ({ default: m.PlanDetailPage })));
const SubscriptionsListPage = lazy(() => import("@/routes/subscriptions/pages").then((m) => ({ default: m.SubscriptionsListPage })));
const SubscriptionDetailPage = lazy(() => import("@/routes/subscriptions/pages").then((m) => ({ default: m.SubscriptionDetailPage })));

function RequireAuth({ children }: { children: ReactNode }) {
  const { isAuthenticated, loading } = useAuth();
  if (loading) {
    // 正在用 refresh cookie 探测初始会话状态，避免闪现登录页。
    return null;
  }
  if (!isAuthenticated) {
    return <Navigate to="/console/login" replace />;
  }
  return <>{children}</>;
}

// G8-1 路由级权限守卫：viewer 访问写路由、非平台 admin 访问平台级写路由时
// 重定向回首页（与各页面按钮 gating 对齐）。角色查询中返回 null 避免误跳转；
// 查询失败（role 为 undefined）时 fail-closed 拒绝。
function RequireRole({
  children,
  mode = "write",
}: {
  children: ReactNode;
  mode?: "write" | "platformAdmin";
}) {
  const { role, isLoading } = useAdminRole();
  if (isLoading) {
    return null;
  }
  const allowed =
    role !== undefined && (mode === "write" ? canWrite(role) : isPlatformAdmin(role));
  if (!allowed) {
    return <Navigate to="/console" replace />;
  }
  return <>{children}</>;
}

// 路由组件工厂：错误兜底从 key=pathname 边界技巧改为 TanStack errorComponent
// （导航即重置，语义等价；守卫组件自身的渲染错误同被它接住——原内层
// RouteErrorBoundary 不再需要）。组件本体保持与原 element= 相同的包裹。
function guarded(el: ReactNode) {
  return {
    component: () => <>{el}</>,
    errorComponent: ({ error }: { error: unknown }) => <RouteErrorFallback error={error} />,
  };
}

const roleGuarded = (mode: "write" | "platformAdmin", el: ReactNode) =>
  guarded(<RequireRole mode={mode}>{el}</RequireRole>);

// ---- 路由树 ----

const rootRoute = createRootRoute({
  component: () => (
    <Suspense
      fallback={
        <div className="flex justify-center py-16 text-sm text-muted-foreground">
          加载中…
        </div>
      }
    >
      <Outlet />
    </Suspense>
  ),
  notFoundComponent: () => <Navigate to="/console" replace />,
  errorComponent: ({ error }) => <RouteErrorFallback error={error} />,
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/console/login",
  ...guarded(<Login />),
});

const consoleRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/console",
  component: () => (
    <RequireAuth>
      <Layout />
    </RequireAuth>
  ),
});

const dashboardRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/",
  ...guarded(<Dashboard />),
});

const projectsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/projects", ...guarded(<ProjectsListPage />) });
const projectNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/projects/new", ...roleGuarded("platformAdmin", <ProjectNewPage />) });
const projectDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/projects/$id", ...guarded(<ProjectDetailPage />) });
const projectSettingsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/projects/$id/settings", ...guarded(<ProjectSettingsPage />) });

const settingsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/settings", ...guarded(<SettingsRedirect />) });

// 账户设置区（/console/account/*）：全角色可用，不设 RequireRole。
const accountRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/account", ...guarded(<AccountLayout />) });
const accountIndexRoute = createRoute({
  getParentRoute: () => accountRoute,
  path: "/",
  component: () => <Navigate to="/console/account/profile" replace />,
});
const accountProfileRoute = createRoute({ getParentRoute: () => accountRoute, path: "/profile", ...guarded(<AccountProfilePage />) });
const accountPreferencesRoute = createRoute({ getParentRoute: () => accountRoute, path: "/preferences", ...guarded(<AccountPreferencesPage />) });

const adminsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/admins", ...guarded(<AdminsListPage />) });

const leaderboardsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/leaderboards", ...guarded(<LeaderboardsListPage />) });
const leaderboardDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/leaderboards/$boardId", ...guarded(<LeaderboardBoardDetailPage />) });

// 审计日志只读面：服务端 admin_roles 门 = admin/owner（与 isPlatformAdmin
// 同集），路由侧 platformAdmin 守卫对齐。validateSearch = keyset 切片的
// URL 即真相面（filter/sort/cursor/size；与页面 useCollection 共用同一
// schema 实例）。
const auditLogsRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/audit-logs",
  validateSearch: auditLogsSearch.toSearchSchema(),
  ...roleGuarded("platformAdmin", <AuditLogsListPage />),
});

// validateSearch = 列表 fleet 化（§12.9）：游标/页大小/enabled 过滤进 URL。
const apiKeysRoute = createRoute({
  getParentRoute: () => consoleRoute,
  path: "/api-keys",
  validateSearch: apiKeysSearch.toSearchSchema(),
  ...guarded(<ApiKeysListPage />),
});
const apiKeyNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/api-keys/new", ...roleGuarded("platformAdmin", <ApiKeyNewPage />) });
const apiKeyDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/api-keys/$id", ...guarded(<ApiKeyDetailPage />) });

const usersRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/users", ...guarded(<UsersListPage />) });
const userNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/users/new", ...roleGuarded("write", <CreateUserPage />) });
const userDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/users/$id", ...guarded(<UserDetailPage />) });
const userEditRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/users/$id/edit", ...roleGuarded("write", <UserEditPage />) });

const groupsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/groups", ...guarded(<GroupsListPage />) });
const groupNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/groups/new", ...roleGuarded("write", <GroupNewPage />) });
const groupDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/groups/$id", ...guarded(<GroupDetailPage />) });

const storageRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/storage", ...guarded(<StorageListPage />) });
const bucketNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/storage/new", ...roleGuarded("write", <BucketNewPage />) });
const bucketDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/storage/$bucketId", ...guarded(<BucketDetailPage />) });
const fileDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/storage/$bucketId/files/$fileId", ...guarded(<FileDetailPage />) });

const functionsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/functions", ...guarded(<FunctionsListPage />) });
const functionDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/functions/$functionId", ...guarded(<FunctionDetailPage />) });

// Webhooks：读对全角色开放，写在页面按钮层 canWrite 收口（与 Functions 同策略）。
const webhooksRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/webhooks", ...guarded(<WebhooksListPage />) });
const webhookDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/webhooks/$webhookId", ...guarded(<WebhookDetailPage />) });

// RuntimeVars：读动词全角色会话开放；写动词在页面按钮层收口（无独立 new 路由）。
const runtimeVarsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/runtime-vars", ...guarded(<VarSetsListPage />) });
const varSetDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/runtime-vars/$varSetId", ...guarded(<VarSetDetailPage />) });

const databasesRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases", ...guarded(<DatabasesListPage />) });
const databaseNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases/new", ...roleGuarded("platformAdmin", <DatabaseNewPage />) });
const databaseDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases/$dbId", ...guarded(<DatabaseDetailPage />) });
const collectionNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases/$dbId/collections/new", ...roleGuarded("platformAdmin", <CollectionNewPage />) });
const documentNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases/$dbId/collections/$collId/documents/new", ...roleGuarded("write", <DocumentNewPage />) });
const documentDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases/$dbId/collections/$collId/documents/$docId", ...guarded(<DocumentDetailPage />) });

const collectionLayoutRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/databases/$dbId/collections/$collId", ...guarded(<CollectionLayout />) });
const collectionIndexRoute = createRoute({ getParentRoute: () => collectionLayoutRoute, path: "/", ...guarded(<CollectionDetailPage />) });
const documentsRoute = createRoute({ getParentRoute: () => collectionLayoutRoute, path: "/documents", ...guarded(<DocumentsListPage />) });
const listenRoute = createRoute({ getParentRoute: () => collectionLayoutRoute, path: "/listen", ...roleGuarded("platformAdmin", <ListenPanel />) });

// Analytics：读方法对全角色会话开放，不设 RequireRole。
const analyticsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/analytics", ...guarded(<AnalyticsOverviewPage />) });
const analyticsEventsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/analytics/events", ...guarded(<AnalyticsEventsPage />) });
const analyticsEventDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/analytics/events/$name", ...guarded(<AnalyticsEventDetailPage />) });
const analyticsRetentionRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/analytics/retention", ...guarded(<AnalyticsRetentionPage />) });
const analyticsUserActivityRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/analytics/users/$userId", ...guarded(<AnalyticsUserActivityPage />) });

const ordersRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/orders", ...roleGuarded("platformAdmin", <OrdersListPage />) });
const orderDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/orders/$id", ...roleGuarded("platformAdmin", <OrderDetailPage />) });

const assetsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/assets", ...roleGuarded("platformAdmin", <AssetDefsListPage />) });
const assetDefNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/assets/defs/new", ...roleGuarded("platformAdmin", <AssetDefNewPage />) });
const assetDefDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/assets/defs/$id", ...roleGuarded("platformAdmin", <AssetDefDetailPage />) });
const userAssetsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/assets/users", ...roleGuarded("platformAdmin", <UserAssetsPage />) });

const plansRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/subscriptions/plans", ...roleGuarded("platformAdmin", <PlansListPage />) });
const planNewRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/subscriptions/plans/new", ...roleGuarded("platformAdmin", <PlanNewPage />) });
const planDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/subscriptions/plans/$id", ...roleGuarded("platformAdmin", <PlanDetailPage />) });
const subscriptionsRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/subscriptions", ...roleGuarded("platformAdmin", <SubscriptionsListPage />) });
const subscriptionDetailRoute = createRoute({ getParentRoute: () => consoleRoute, path: "/subscriptions/$id", ...roleGuarded("platformAdmin", <SubscriptionDetailPage />) });

const routeTree = rootRoute.addChildren([
  loginRoute,
  consoleRoute.addChildren([
    dashboardRoute,
    projectsRoute,
    projectNewRoute,
    projectDetailRoute,
    projectSettingsRoute,
    settingsRoute,
    accountRoute.addChildren([accountIndexRoute, accountProfileRoute, accountPreferencesRoute]),
    adminsRoute,
    leaderboardsRoute,
    leaderboardDetailRoute,
    auditLogsRoute,
    apiKeysRoute,
    apiKeyNewRoute,
    apiKeyDetailRoute,
    usersRoute,
    userNewRoute,
    userDetailRoute,
    userEditRoute,
    groupsRoute,
    groupNewRoute,
    groupDetailRoute,
    storageRoute,
    bucketNewRoute,
    bucketDetailRoute,
    fileDetailRoute,
    functionsRoute,
    functionDetailRoute,
    webhooksRoute,
    webhookDetailRoute,
    runtimeVarsRoute,
    varSetDetailRoute,
    databasesRoute,
    databaseNewRoute,
    databaseDetailRoute,
    collectionNewRoute,
    collectionLayoutRoute.addChildren([collectionIndexRoute, documentsRoute, listenRoute]),
    documentNewRoute,
    documentDetailRoute,
    analyticsRoute,
    analyticsEventsRoute,
    analyticsEventDetailRoute,
    analyticsRetentionRoute,
    analyticsUserActivityRoute,
    ordersRoute,
    orderDetailRoute,
    assetsRoute,
    assetDefNewRoute,
    assetDefDetailRoute,
    userAssetsRoute,
    plansRoute,
    planNewRoute,
    planDetailRoute,
    subscriptionsRoute,
    subscriptionDetailRoute,
  ]),
]);

export const router = createRouter({ routeTree });

// 类型注册（消费方 useSearch/Link 的类型面；compat 层内部已按 loose 形态使用）。
declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
