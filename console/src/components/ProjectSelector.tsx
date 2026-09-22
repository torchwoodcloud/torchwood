import { useQuery } from "@tanstack/react-query";
import { listProjects } from "@/api/projects";
import { useAuth } from "@/hooks/useAuth";
import { Skeleton } from "@/components/ui/skeleton";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { cn } from "@/lib/utils";

// collapsed 为侧栏图标栏形态：触发器只显示项目首字母方块（隐藏名称与下拉箭头），
// 下拉行为不变。
export function ProjectSelector({ collapsed = false }: { collapsed?: boolean }) {
  const { projectId, selectProject } = useAuth();

  const { data, isLoading } = useQuery({
    queryKey: ["projects"],
    queryFn: () => listProjects({ pageSize: 100 }),
  });
  const projects = data?.rows ?? [];

  const selectedProjectId = projectId || "";
  const activeProject = projects.find((p) => p.id === selectedProjectId);

  if (isLoading) {
    return (
      <Skeleton className={cn("h-9", collapsed ? "mx-auto w-9" : "w-full")} />
    );
  }

  if (projects.length === 0) {
    return (
      <p
        className={cn(
          "py-1.5 text-xs text-muted-foreground",
          collapsed ? "px-0 text-center" : "px-2"
        )}
        title={collapsed ? "暂无项目，请先在 Projects 中创建" : undefined}
      >
        {collapsed ? "—" : "暂无项目，请先在 Projects 中创建"}
      </p>
    );
  }

  const initial = activeProject?.name.slice(0, 1).toUpperCase();

  return (
    <Select value={selectedProjectId} onValueChange={selectProject}>
      <SelectTrigger
        title={collapsed ? activeProject?.name : undefined}
        className={cn(
          "h-9 gap-2 border-sidebar-border bg-background px-2 text-sm font-medium focus:ring-1 focus:ring-ring/30",
          collapsed && "justify-center px-0 [&>svg]:hidden"
        )}
      >
        <span className="flex min-w-0 items-center gap-2">
          <span className="flex size-5 shrink-0 items-center justify-center rounded-md bg-sidebar-accent text-[10px] font-semibold">
            {initial ?? "?"}
          </span>
          {!collapsed && <SelectValue placeholder="选择项目" />}
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
