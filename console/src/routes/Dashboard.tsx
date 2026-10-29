import { useQuery } from "@tanstack/react-query";
import { useCollectionData } from "@lynx-go/uikit-core";
import { Link } from "@tanstack/react-router";
import { ArrowRight, FolderKanban, FolderPlus } from "lucide-react";
import { getCurrentAdmin } from "@/api/admins";
import { projectsResource } from "@/kit/projects";
import { apiKeysResource } from "@/kit/apiKeys";
import { bucketsResource } from "@/kit/storage";
import { databasesResource } from "@/kit/databases";
import { StatCard } from "@/components/kit/values/stat-card";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate } from "@/lib/datetime";

export function Dashboard() {
  const { projectId } = useAuth();
  const tz = useUserTimezone();

  const { data: me } = useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
    retry: 1,
    staleTime: 60_000,
  });

  // 数据面 = kit 资源伴读绑定（useCollectionData 零 URL 形态——命令失效
  // 自动覆盖 kit 键）；统计口径 = 单页上限 100 计数（keyset 无总数，同旧页）。
  // keepPrevious: false 与原 useQuery 行为等价（项目切换回 skeleton 非旧数据）。
  const projects = useCollectionData(projectsResource, { size: 100, keepPrevious: false });

  const selectedProjectId = projectId || "";
  const activeProject = projects.rows.find((p) => p.id === selectedProjectId);

  const apiKeys = useCollectionData(apiKeysResource, {
    scope: { projectId: selectedProjectId },
    size: 100,
    enabled: !!selectedProjectId,
    keepPrevious: false,
  });
  const buckets = useCollectionData(bucketsResource, {
    scope: { projectId: selectedProjectId },
    size: 100,
    enabled: !!selectedProjectId,
    keepPrevious: false,
  });
  // keyset 分页下拿不到总数；统计卡取上限一页（100）计数，超出封顶。
  const databases = useCollectionData(databasesResource, {
    scope: { projectId: selectedProjectId },
    size: 100,
    enabled: !!selectedProjectId,
    keepPrevious: false,
  });

  const enabledKeys = apiKeys.rows.filter((k) => k.enabled).length;
  const publicBuckets = buckets.rows.filter((b) => b.public).length;
  const localPart = me?.email?.split("@")[0] ?? "";
  const displayName = localPart ? localPart.charAt(0).toUpperCase() + localPart.slice(1) : "";
  const recentProjects = [...projects.rows]
    .sort((a, b) => (b.created_at ?? "").localeCompare(a.created_at ?? ""))
    .slice(0, 6);
  const projectScope = activeProject ? `in ${activeProject.name}` : "no active project";

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <h1 className="text-3xl font-semibold tracking-tight">
          {displayName ? `Welcome back, ${displayName}` : "Welcome back"}
        </h1>
        <Button asChild variant="secondary" className="w-fit">
          <Link to="/console/projects">
            Go to projects
            <ArrowRight className="h-4 w-4" />
          </Link>
        </Button>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Projects"
          value={projects.status === "loading" ? <Skeleton className="h-9 w-16" /> : projects.rows.length}
          hint={activeProject ? `active: ${activeProject.name}` : "no active project"}
        />
        <StatCard label="Databases" value={databases.rows.length} hint={projectScope} />
        <StatCard
          label="Buckets"
          value={buckets.rows.length}
          hint={`${publicBuckets} public · ${projectScope}`}
        />
        <StatCard
          label="API Keys"
          value={apiKeys.rows.length}
          hint={`${enabledKeys} enabled · ${projectScope}`}
        />
      </div>

      <div className="rounded-xl border bg-background">
        <div className="flex items-center justify-between border-b px-5 py-4">
          <div className="flex items-center gap-2">
            <FolderKanban className="h-4 w-4 text-muted-foreground" />
            <h2 className="text-sm font-semibold">Recent projects</h2>
          </div>
          <Link
            to="/console/projects"
            className="text-xs text-muted-foreground transition-colors hover:text-foreground"
          >
            view all →
          </Link>
        </div>
        {projects.status === "loading" ? (
          <div className="space-y-3 p-5">
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
            <Skeleton className="h-10 w-full" />
          </div>
        ) : recentProjects.length === 0 ? (
          <div className="flex min-h-[320px] flex-col items-center justify-center gap-3 p-10 text-center text-sm text-muted-foreground">
            <FolderPlus className="h-8 w-8 opacity-40" />
            <span>No projects yet.</span>
          </div>
        ) : (
          <ul className="divide-y">
            {recentProjects.map((project) => (
              <li key={project.id}>
                <Link
                  to="/console/projects/$id" params={{ id: project.id }}
                  className="group flex items-center gap-4 px-5 py-4 transition-colors hover:bg-muted/40"
                >
                  <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-muted text-xs font-medium text-muted-foreground">
                    {project.name.slice(0, 1).toUpperCase()}
                  </span>
                  <div className="flex min-w-0 flex-1 flex-col">
                    <span className="flex items-center gap-2 truncate text-sm">
                      {project.name}
                      {project.id === selectedProjectId && (
                        <Badge variant="secondary" className="px-1.5 py-0 text-[10px]">
                          Active
                        </Badge>
                      )}
                    </span>
                    <span className="truncate text-xs text-muted-foreground">
                      {project.id}
                    </span>
                  </div>
                  <span className="hidden text-xs text-muted-foreground sm:inline">
                    {formatDate(project.created_at, tz)}
                  </span>
                  <span className="text-xs text-muted-foreground transition-colors group-hover:text-foreground">
                    open →
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}
