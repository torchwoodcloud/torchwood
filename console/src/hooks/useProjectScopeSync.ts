import { useEffect } from "react";
import { useAuth } from "./useAuth";

/**
 * useProjectScopeSync 把全局项目选择（侧边栏 selector，X-Torchwood-Project header）
 * 同步到项目上下文页面的 URL :id 上，使 header 作用域 API（如 oauth-providers，
 * URL 不含 project_id、项目取自 Principal）与显式路径的作用域一致。
 *
 * 传入 undefined（项目尚未加载成功 / 404）时不同步：避免无效 id 与
 * ProjectBootstrap 的兜底选择互相拉扯形成循环。
 *
 * 返回 synced：header 已与目标项目一致，调用方以此守卫 header 作用域面板
 * （不同步就渲染会以旧项目身份查询/保存，静默写错项目）。
 */
export function useProjectScopeSync(projectId: string | undefined): boolean {
  const { projectId: selected, selectProject } = useAuth();
  const synced = !!projectId && selected === projectId;

  useEffect(() => {
    if (projectId && selected !== projectId) {
      selectProject(projectId);
    }
  }, [projectId, selected, selectProject]);

  return synced;
}
