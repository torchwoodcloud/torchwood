import { useQuery } from "@tanstack/react-query";
import { Link } from "react-router-dom";
import { Settings as SettingsIcon } from "lucide-react";
import { listProjects } from "@/api/projects";
import { listAPIKeys } from "@/api/apiKeys";
import { listUsers } from "@/api/users";
import { listBuckets } from "@/api/storage";
import { listDatabases } from "@/api/databases";
import { listGroups } from "@/api/groups";
import { useAuth } from "@/hooks/useAuth";
import { PageHeader } from "@/components/PageHeader";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { Card, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";

export function Dashboard() {
  const { projectId } = useAuth();

  const { data: projects = [], isLoading: projectsLoading } = useQuery({
    queryKey: ["projects"],
    queryFn: listProjects,
  });

  const selectedProjectId = projectId || "";

  const { data: apiKeys = [] } = useQuery({
    queryKey: ["api-keys", selectedProjectId],
    queryFn: listAPIKeys,
    enabled: !!selectedProjectId,
  });

  const { data: users = [] } = useQuery({
    queryKey: ["users", selectedProjectId],
    queryFn: listUsers,
    enabled: !!selectedProjectId,
  });

  const { data: buckets = [] } = useQuery({
    queryKey: ["buckets", selectedProjectId],
    queryFn: listBuckets,
    enabled: !!selectedProjectId,
  });

  const { data: databases = [] } = useQuery({
    queryKey: ["databases", selectedProjectId],
    queryFn: listDatabases,
    enabled: !!selectedProjectId,
  });

  const { data: groups = [] } = useQuery({
    queryKey: ["groups", selectedProjectId],
    queryFn: listGroups,
    enabled: !!selectedProjectId,
  });

  return (
    <div className="space-y-6">
      <PageHeader
        title="Dashboard"
        description="Overview of your Torchwood workspace. Switch the active project from the sidebar."
        actions={
          projectId ? (
            <Button asChild variant="outline" size="sm">
              <Link to={`/console/projects/${projectId}/settings`}>
                <SettingsIcon className="h-4 w-4 mr-2" />
                设置
              </Link>
            </Button>
          ) : undefined
        }
      />

      <div className="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
        <StatCard title="Projects" value={projects.length} isLoading={projectsLoading} />
        <StatCard title="API Keys" value={apiKeys.length} />
        <StatCard title="Users" value={users.length} />
        <StatCard title="Groups" value={groups.length} />
        <StatCard title="Buckets" value={buckets.length} />
        <StatCard title="Databases" value={databases.length} />
      </div>
    </div>
  );
}

function StatCard({ title, value, isLoading }: { title: string; value: number; isLoading?: boolean }) {
  return (
    <Card>
      <CardHeader className="pb-2">
        <CardDescription>{title}</CardDescription>
        {isLoading ? (
          <Skeleton className="h-9 w-16" />
        ) : (
          <CardTitle className="text-4xl">{value}</CardTitle>
        )}
      </CardHeader>
    </Card>
  );
}
