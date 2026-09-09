import { Navigate } from "react-router-dom";
import { useAuth } from "@/hooks/useAuth";

// /console/settings 全局 Settings 页已退役：项目配置统一收敛至
// /console/projects/:id/settings（显式项目作用域，见 routes/projects/settings.tsx）。
// 旧路径按当前选中项目重定向；未选中项目时回落 Projects 列表。
export function SettingsRedirect() {
  const { projectId } = useAuth();
  if (projectId) {
    return <Navigate to={`/console/projects/${projectId}/settings`} replace />;
  }
  return <Navigate to="/console/projects" replace />;
}
