import { useEffect, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { listProjects } from "@/api/projects";
import { useAuth } from "@/hooks/useAuth";

/** Ensures admin console requests have X-Torchwood-Project before project-scoped APIs run. */
export function ProjectBootstrap() {
  const { projectId, selectProject } = useAuth();

  // 引导用途取一大页即可（项目量级 = 管理员可管理的项目数，上限内取全）。
  const { data } = useQuery({
    queryKey: ["projects"],
    queryFn: () => listProjects({ pageSize: 100 }),
  });
  // rows 需要稳定引用：作为 useEffect 依赖参与项目选择同步。
  const projects = useMemo(() => data?.rows ?? [], [data]);

  useEffect(() => {
    if (projects.length === 0) return;
    const firstId = projects[0].id;
    if (!projectId) {
      selectProject(firstId);
      return;
    }
    if (!projects.some((p) => p.id === projectId)) {
      selectProject(firstId);
    }
  }, [projectId, projects, selectProject]);

  return null;
}
