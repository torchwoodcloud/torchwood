import { lazy, Suspense, type ReactNode } from "react";
import { BrowserRouter, Navigate, Route, Routes, useLocation } from "react-router-dom";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Toaster } from "sonner";
import { AuthProvider, useAuth } from "@/hooks/useAuth";
import { ErrorBoundary } from "@/components/ErrorBoundary";
import { Layout } from "@/components/Layout";
import { useAdminRole, canWrite, isPlatformAdmin } from "@/hooks/useAdminRole";

const Login = lazy(() =>
  import("@/routes/Login").then((m) => ({ default: m.Login }))
);
const Dashboard = lazy(() =>
  import("@/routes/Dashboard").then((m) => ({ default: m.Dashboard }))
);
const ProjectsListPage = lazy(() =>
  import("@/routes/projects/pages").then((m) => ({ default: m.ProjectsListPage }))
);
const ProjectNewPage = lazy(() =>
  import("@/routes/projects/pages").then((m) => ({ default: m.ProjectNewPage }))
);
const ProjectDetailPage = lazy(() =>
  import("@/routes/projects/pages").then((m) => ({ default: m.ProjectDetailPage }))
);
const ProjectSettingsPage = lazy(() =>
  import("@/routes/projects/settings").then((m) => ({ default: m.ProjectSettingsPage }))
);
const SettingsRedirect = lazy(() =>
  import("@/routes/settings/pages").then((m) => ({ default: m.SettingsRedirect }))
);
const ApiKeysListPage = lazy(() =>
  import("@/routes/api-keys/pages").then((m) => ({ default: m.ApiKeysListPage }))
);
const ApiKeyNewPage = lazy(() =>
  import("@/routes/api-keys/pages").then((m) => ({ default: m.ApiKeyNewPage }))
);
const ApiKeyDetailPage = lazy(() =>
  import("@/routes/api-keys/pages").then((m) => ({ default: m.ApiKeyDetailPage }))
);
const UsersListPage = lazy(() =>
  import("@/routes/users/pages").then((m) => ({ default: m.UsersListPage }))
);
const UserDetailPage = lazy(() =>
  import("@/routes/users/pages").then((m) => ({ default: m.UserDetailPage }))
);
const UserEditPage = lazy(() =>
  import("@/routes/users/pages").then((m) => ({ default: m.UserEditPage }))
);
const CreateUserPage = lazy(() =>
  import("@/routes/users/pages").then((m) => ({ default: m.CreateUserPage }))
);
const StorageListPage = lazy(() =>
  import("@/routes/storage/pages").then((m) => ({ default: m.StorageListPage }))
);
const BucketNewPage = lazy(() =>
  import("@/routes/storage/pages").then((m) => ({ default: m.BucketNewPage }))
);
const BucketDetailPage = lazy(() =>
  import("@/routes/storage/pages").then((m) => ({ default: m.BucketDetailPage }))
);
const FileDetailPage = lazy(() =>
  import("@/routes/storage/pages").then((m) => ({ default: m.FileDetailPage }))
);
const FunctionsListPage = lazy(() =>
  import("@/routes/functions/pages").then((m) => ({ default: m.FunctionsListPage }))
);
const FunctionDetailPage = lazy(() =>
  import("@/routes/functions/pages").then((m) => ({ default: m.FunctionDetailPage }))
);
const GroupsListPage = lazy(() =>
  import("@/routes/groups/pages").then((m) => ({ default: m.GroupsListPage }))
);
const GroupNewPage = lazy(() =>
  import("@/routes/groups/pages").then((m) => ({ default: m.GroupNewPage }))
);
const GroupDetailPage = lazy(() =>
  import("@/routes/groups/pages").then((m) => ({ default: m.GroupDetailPage }))
);
const DatabasesListPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.DatabasesListPage }))
);
const DatabaseNewPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.DatabaseNewPage }))
);
const DatabaseDetailPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.DatabaseDetailPage }))
);
const CollectionNewPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.CollectionNewPage }))
);
const CollectionDetailPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.CollectionDetailPage }))
);
const DocumentsListPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.DocumentsListPage }))
);
const DocumentNewPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.DocumentNewPage }))
);
const DocumentDetailPage = lazy(() =>
  import("@/routes/databases/pages").then((m) => ({ default: m.DocumentDetailPage }))
);
const CollectionLayout = lazy(() =>
  import("@/routes/databases/CollectionLayout").then((m) => ({ default: m.CollectionLayout }))
);
const ListenPanel = lazy(() =>
  import("@/routes/databases/ListenPanel").then((m) => ({ default: m.ListenPanel }))
);
const AnalyticsOverviewPage = lazy(() =>
  import("@/routes/analytics/pages").then((m) => ({ default: m.AnalyticsOverviewPage }))
);
const AnalyticsEventsPage = lazy(() =>
  import("@/routes/analytics/pages").then((m) => ({ default: m.AnalyticsEventsPage }))
);
const AnalyticsRetentionPage = lazy(() =>
  import("@/routes/analytics/pages").then((m) => ({ default: m.AnalyticsRetentionPage }))
);
const AnalyticsEventDetailPage = lazy(() =>
  import("@/routes/analytics/EventDetailPage").then((m) => ({ default: m.AnalyticsEventDetailPage }))
);
const AnalyticsUserActivityPage = lazy(() =>
  import("@/routes/analytics/UserActivityPage").then((m) => ({ default: m.AnalyticsUserActivityPage }))
);
const AdminsListPage = lazy(() =>
  import("@/routes/admins/pages").then((m) => ({ default: m.AdminsListPage }))
);
const LeaderboardsListPage = lazy(() =>
  import("@/routes/leaderboards/pages").then((m) => ({ default: m.LeaderboardsListPage }))
);
const LeaderboardBoardDetailPage = lazy(() =>
  import("@/routes/leaderboards/pages").then((m) => ({ default: m.BoardDetailPage }))
);
const AuditLogsListPage = lazy(() =>
  import("@/routes/audit-logs/pages").then((m) => ({ default: m.AuditLogsListPage }))
);
const OrdersListPage = lazy(() =>
  import("@/routes/payments/pages").then((m) => ({ default: m.OrdersListPage }))
);
const OrderDetailPage = lazy(() =>
  import("@/routes/payments/pages").then((m) => ({ default: m.OrderDetailPage }))
);
const AssetDefsListPage = lazy(() =>
  import("@/routes/assets/pages").then((m) => ({ default: m.AssetDefsListPage }))
);
const AssetDefNewPage = lazy(() =>
  import("@/routes/assets/pages").then((m) => ({ default: m.AssetDefNewPage }))
);
const AssetDefDetailPage = lazy(() =>
  import("@/routes/assets/pages").then((m) => ({ default: m.AssetDefDetailPage }))
);
const UserAssetsPage = lazy(() =>
  import("@/routes/assets/pages").then((m) => ({ default: m.UserAssetsPage }))
);
const PlansListPage = lazy(() =>
  import("@/routes/subscriptions/pages").then((m) => ({ default: m.PlansListPage }))
);
const PlanNewPage = lazy(() =>
  import("@/routes/subscriptions/pages").then((m) => ({ default: m.PlanNewPage }))
);
const PlanDetailPage = lazy(() =>
  import("@/routes/subscriptions/pages").then((m) => ({ default: m.PlanDetailPage }))
);
const SubscriptionsListPage = lazy(() =>
  import("@/routes/subscriptions/pages").then((m) => ({ default: m.SubscriptionsListPage }))
);
const SubscriptionDetailPage = lazy(() =>
  import("@/routes/subscriptions/pages").then((m) => ({ default: m.SubscriptionDetailPage }))
);

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

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

// RouteErrorBoundary：按路径分段重置错误边界，单页崩溃不影响其他路由；
// 同时兜住 React.lazy 分包加载失败（chunk 加载错误）。
function RouteErrorBoundary({ children }: { children: ReactNode }) {
  const location = useLocation();
  return <ErrorBoundary key={location.pathname}>{children}</ErrorBoundary>;
}

function AppRoutes() {
  return (
    <Routes>
      <Route
        path="/console/login"
        element={
          <RouteErrorBoundary>
            <Login />
          </RouteErrorBoundary>
        }
      />
      <Route
        path="/console"
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route
          index
          element={
            <RouteErrorBoundary>
              <Dashboard />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="projects"
          element={
            <RouteErrorBoundary>
              <ProjectsListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="projects/new"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <ProjectNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="projects/:id"
          element={
            <RouteErrorBoundary>
              <ProjectDetailPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="projects/:id/settings"
          element={
            <RouteErrorBoundary>
              <ProjectSettingsPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="settings"
          element={
            <RouteErrorBoundary>
              <SettingsRedirect />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="admins"
          element={
            <RouteErrorBoundary>
              <AdminsListPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="leaderboards"
          element={
            <RouteErrorBoundary>
              <LeaderboardsListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="leaderboards/:boardId"
          element={
            <RouteErrorBoundary>
              <LeaderboardBoardDetailPage />
            </RouteErrorBoundary>
          }
        />

        {/* 审计日志只读面：服务端 admin_roles 门 = admin/owner（与
            isPlatformAdmin 同集），路由侧用 platformAdmin 守卫对齐。 */}
        <Route
          path="audit-logs"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <AuditLogsListPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />

        <Route
          path="api-keys"
          element={
            <RouteErrorBoundary>
              <ApiKeysListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="api-keys/new"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <ApiKeyNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="api-keys/:id"
          element={
            <RouteErrorBoundary>
              <ApiKeyDetailPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="users"
          element={
            <RouteErrorBoundary>
              <UsersListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="users/new"
          element={
            <RequireRole>
              <RouteErrorBoundary>
                <CreateUserPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="users/:id"
          element={
            <RouteErrorBoundary>
              <UserDetailPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="users/:id/edit"
          element={
            <RequireRole>
              <RouteErrorBoundary>
                <UserEditPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />

        <Route
          path="groups"
          element={
            <RouteErrorBoundary>
              <GroupsListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="groups/new"
          element={
            <RequireRole>
              <RouteErrorBoundary>
                <GroupNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="groups/:id"
          element={
            <RouteErrorBoundary>
              <GroupDetailPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="storage"
          element={
            <RouteErrorBoundary>
              <StorageListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="storage/new"
          element={
            <RequireRole>
              <RouteErrorBoundary>
                <BucketNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="storage/:bucketId"
          element={
            <RouteErrorBoundary>
              <BucketDetailPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="storage/:bucketId/files/:fileId"
          element={
            <RouteErrorBoundary>
              <FileDetailPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="functions"
          element={
            <RouteErrorBoundary>
              <FunctionsListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="functions/:functionId"
          element={
            <RouteErrorBoundary>
              <FunctionDetailPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="databases"
          element={
            <RouteErrorBoundary>
              <DatabasesListPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="databases/new"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <DatabaseNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="databases/:dbId"
          element={
            <RouteErrorBoundary>
              <DatabaseDetailPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="databases/:dbId/collections/new"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <CollectionNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="databases/:dbId/collections/:collId"
          element={
            <RouteErrorBoundary>
              <CollectionLayout />
            </RouteErrorBoundary>
          }
        >
          <Route
            index
            element={
              <RouteErrorBoundary>
                <CollectionDetailPage />
              </RouteErrorBoundary>
            }
          />
          <Route
            path="documents"
            element={
              <RouteErrorBoundary>
                <DocumentsListPage />
              </RouteErrorBoundary>
            }
          />
          <Route
            path="listen"
            element={
              <RequireRole mode="platformAdmin">
                <RouteErrorBoundary>
                  <ListenPanel />
                </RouteErrorBoundary>
              </RequireRole>
            }
          />
        </Route>
        <Route
          path="databases/:dbId/collections/:collId/documents/new"
          element={
            <RequireRole>
              <RouteErrorBoundary>
                <DocumentNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="databases/:dbId/collections/:collId/documents/:docId"
          element={
            <RouteErrorBoundary>
              <DocumentDetailPage />
            </RouteErrorBoundary>
          }
        />

        {/* Analytics（docs/design/analytics.md §9）：读方法对 Console admin
            全角色会话开放（viewer 含），不设 RequireRole。 */}
        <Route
          path="analytics"
          element={
            <RouteErrorBoundary>
              <AnalyticsOverviewPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="analytics/events"
          element={
            <RouteErrorBoundary>
              <AnalyticsEventsPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="analytics/events/:name"
          element={
            <RouteErrorBoundary>
              <AnalyticsEventDetailPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="analytics/retention"
          element={
            <RouteErrorBoundary>
              <AnalyticsRetentionPage />
            </RouteErrorBoundary>
          }
        />
        <Route
          path="analytics/users/:userId"
          element={
            <RouteErrorBoundary>
              <AnalyticsUserActivityPage />
            </RouteErrorBoundary>
          }
        />

        <Route
          path="orders"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <OrdersListPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="orders/:id"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <OrderDetailPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />

        <Route
          path="assets"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <AssetDefsListPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="assets/defs/new"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <AssetDefNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="assets/defs/:id"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <AssetDefDetailPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="assets/users"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <UserAssetsPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />

        <Route
          path="subscriptions/plans"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <PlansListPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="subscriptions/plans/new"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <PlanNewPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="subscriptions/plans/:id"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <PlanDetailPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="subscriptions"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <SubscriptionsListPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
        <Route
          path="subscriptions/:id"
          element={
            <RequireRole mode="platformAdmin">
              <RouteErrorBoundary>
                <SubscriptionDetailPage />
              </RouteErrorBoundary>
            </RequireRole>
          }
        />
      </Route>
      <Route path="*" element={<Navigate to="/console" replace />} />
    </Routes>
  );
}

function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <BrowserRouter>
          <ErrorBoundary>
            <Suspense
              fallback={
                <div className="flex justify-center py-16 text-sm text-muted-foreground">
                  加载中…
                </div>
              }
            >
              <AppRoutes />
            </Suspense>
          </ErrorBoundary>
        </BrowserRouter>
        <Toaster position="top-right" />
      </AuthProvider>
    </QueryClientProvider>
  );
}

export default App;
