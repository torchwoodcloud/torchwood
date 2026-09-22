import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { ArrowRight, FolderKanban, FolderPlus } from "lucide-react";
import { listProjects } from "@/api/projects";
import { listAPIKeys } from "@/api/apiKeys";
import { listBuckets } from "@/api/storage";
import { listDatabases } from "@/api/databases";
import { getCurrentAdmin } from "@/api/admins";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import { formatDate } from "@/lib/datetime";

function StatCard({
  label,
  value,
  delta,
  isLoading,
}: {
  label: string;
  value: number;
  delta?: string;
  isLoading?: boolean;
}) {
  return (
    <div className="flex min-h-[140px] flex-col justify-between rounded-xl border bg-background p-5">
      <span className="text-xs uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <div className="flex flex-col gap-1">
        {isLoading ? (
          <Skeleton className="h-9 w-16" />
        ) : (
          <span className="text-3xl font-semibold tabular-nums tracking-tight">
            {value}
          </span>
        )}
        {delta && <span className="text-xs text-muted-foreground">{delta}</span>}
      </div>
    </div>
  );
}

export function Dashboard() {
  const { projectId } = useAuth();
  const tz = useUserTimezone();

  const { data: me } = useQuery({
    queryKey: ["console-admin-me"],
    queryFn: getCurrentAdmin,
    retry: 1,
    staleTime: 60_000,
  });

  const { data, isLoading: projectsLoading } = useQuery({
    queryKey: ["projects"],
    queryFn: () => listProjects({ pageSize: 100 }),
  });
  const projects = data?.rows ?? [];

  const selectedProjectId = projectId || "";
  const activeProject = projects.find((p) => p.id === selectedProjectId);

  const { data: apiKeyPage } = useQuery({
    queryKey: ["api-keys", selectedProjectId],
    queryFn: () => listAPIKeys({ pageSize: 100 }),
    enabled: !!selectedProjectId,
  });
  const apiKeys = apiKeyPage?.rows ?? [];

  const { data: bucketPage } = useQuery({
    queryKey: ["buckets", selectedProjectId],
    queryFn: () => listBuckets({ pageSize: 100 }),
    enabled: !!selectedProjectId,
  });
  const buckets = bucketPage?.rows ?? [];

  // keyset 分页下拿不到总数；统计卡取上限一页（100）计数，超出封顶。
  const { data: dbPage } = useQuery({
    queryKey: ["databases", selectedProjectId, 100],
    queryFn: () => listDatabases({ pageSize: 100 }),
    enabled: !!selectedProjectId,
  });
  const databases = dbPage?.rows ?? [];

  const enabledKeys = apiKeys.filter((k) => k.enabled).length;
  const publicBuckets = buckets.filter((b) => b.public).length;
  const localPart = me?.email?.split("@")[0] ?? "";
  const displayName = localPart ? localPart.charAt(0).toUpperCase() + localPart.slice(1) : "";
  const recentProjects = [...projects]
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
          value={projects.length}
          delta={activeProject ? `active: ${activeProject.name}` : "no active project"}
          isLoading={projectsLoading}
        />
        <StatCard label="Databases" value={databases.length} delta={projectScope} />
        <StatCard
          label="Buckets"
          value={buckets.length}
          delta={`${publicBuckets} public · ${projectScope}`}
        />
        <StatCard
          label="API Keys"
          value={apiKeys.length}
          delta={`${enabledKeys} enabled · ${projectScope}`}
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
        {projectsLoading ? (
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
                  to={`/console/projects/${project.id}`}
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
