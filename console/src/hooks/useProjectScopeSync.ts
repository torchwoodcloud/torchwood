import { useEffect, useRef } from "react";
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
 *
 * 回写只跟随「URL 导航」，不跟随 selected 的其它变化：lastPushedRef 记录已
 * 推送过的 URL id，跨标签 storage 事件同步进来的 selected、或用户在本页直接
 * 切 selector，都不会把 URL id 顶回全局——否则两个都停在项目页的标签会互相
 * 把对方的选择写回 localStorage，形成 storage 事件乒乓循环。离开项目页
 * （projectId 变 undefined）即重置，「浏览即选中」的导航语义不变（离开再
 * 回来仍会重新同步）。
 */
export function useProjectScopeSync(projectId: string | undefined): boolean {
  const { projectId: selected, selectProject } = useAuth();
  const lastPushedRef = useRef<string | undefined>(undefined);
  const synced = !!projectId && selected === projectId;

  useEffect(() => {
    if (!projectId) {
      lastPushedRef.current = undefined;
      return;
    }
    if (lastPushedRef.current === projectId) {
      return;
    }
    lastPushedRef.current = projectId;
    if (selected !== projectId) {
      selectProject(projectId);
    }
  }, [projectId, selected, selectProject]);

  return synced;
}
