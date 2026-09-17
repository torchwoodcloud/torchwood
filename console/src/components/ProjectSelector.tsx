import { useQuery } from "@tanstack/react-query";
import { listProjects } from "@/api/projects";
import { useAuth } from "@/hooks/useAuth";
import { Skeleton } from "@/components/ui/skeleton";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

export function ProjectSelector() {
  const { projectId, selectProject } = useAuth();

  const { data: projects = [], isLoading } = useQuery({
    queryKey: ["projects"],
    queryFn: listProjects,
  });

  const selectedProjectId = projectId || "";
  const activeProject = projects.find((p) => p.id === selectedProjectId);

  if (isLoading) {
    return <Skeleton className="h-9 w-full" />;
  }

  if (projects.length === 0) {
    return (
      <p className="px-2 py-1.5 text-xs text-muted-foreground">
        暂无项目，请先在 Projects 中创建
      </p>
    );
  }

  return (
    <Select value={selectedProjectId} onValueChange={selectProject}>
      <SelectTrigger className="h-9 gap-2 border-sidebar-border bg-background px-2 text-sm font-medium focus:ring-1 focus:ring-ring/30">
        <span className="flex min-w-0 items-center gap-2">
          {activeProject && (
            <span className="flex size-5 shrink-0 items-center justify-center rounded-md bg-sidebar-accent text-[10px] font-semibold">
              {activeProject.name.slice(0, 1).toUpperCase()}
            </span>
          )}
          <SelectValue placeholder="选择项目" />
        </span>
      </SelectTrigger>
      <SelectContent>
        {projects.map((p) => (
          <SelectItem key={p.id} value={p.id}>
            {p.name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
