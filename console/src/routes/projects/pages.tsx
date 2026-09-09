import { useCallback, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Settings } from "lucide-react";
import {
  listProjects,
  getProject,
  createProject,
  type Project,
} from "@/api/projects";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useProjectScopeSync } from "@/hooks/useProjectScopeSync";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import type { ColumnDef } from "@/components/list/DataTable";
import {
  FormPageWrapper,
  FormField,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
} from "@/components/resource/shared";

const columns: ColumnDef<Project>[] = [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (p) => p.id,
  },
  {
    key: "name",
    header: "名称",
    cell: (p) => p.name,
  },
  {
    key: "status",
    header: "状态",
    cell: (p) => (
      <Badge variant={p.status === "active" ? "default" : "secondary"}>{p.status}</Badge>
    ),
  },
  {
    key: "created",
    header: "创建时间",
    cell: (p) => new Date(p.created_at).toLocaleString(),
  },
];

export function ProjectsListPage() {
  const { role } = useAdminRole();
  const platformAdmin = isPlatformAdmin(role);

  const { data: projects = [], isLoading } = useQuery({
    queryKey: ["projects"],
    queryFn: listProjects,
  });

  const getSearchText = useCallback(
    (p: Project) => `${p.id} ${p.name} ${p.description ?? ""} ${p.status}`,
    []
  );

  return (
    <ResourceListPage
      title="Projects"
      description="管理 Torchwood 项目"
      searchPlaceholder="搜索项目名称或 ID..."
      isLoading={isLoading}
      items={projects}
      columns={columns}
      getSearchText={getSearchText}
      detailPath={(p) => `/console/projects/${p.id}`}
      toolbarActions={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/projects/new">
              <Plus className="h-4 w-4 mr-2" />
              新建项目
            </Link>
          </Button>
        ) : undefined
      }
      emptyTitle="暂无项目"
      emptyDescription="创建第一个项目以开始使用"
      emptyAction={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/projects/new">新建项目</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function ProjectNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  const mutation = useMutation({
    mutationFn: createProject,
    onSuccess: (project) => {
      toast.success("项目创建成功");
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      navigate(`/console/projects/${project.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建项目"
      description="添加新项目到平台"
      backTo="/console/projects"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        mutation.mutate({ id, name, description: description || undefined });
      }}
      loading={mutation.isPending}
      submitDisabled={!isPlatformAdmin(role)}
    >
      <FormField
        id="id"
        label="ID"
        value={id}
        onChange={setId}
        required
        placeholder="shop"
        hint="小写字母开头，仅小写字母与数字，最长 28。创建后不可改。"
      />
      <FormField id="name" label="名称" value={name} onChange={setName} required placeholder="My Project" />
      <FormField
        id="description"
        label="描述"
        value={description}
        onChange={setDescription}
        placeholder="可选描述"
      />
    </FormPageWrapper>
  );
}

// ProjectDetailPage 项目概览（只读）：项目级配置统一在设置区
// /console/projects/:id/settings（基本信息编辑、注册与登录、OAuth、危险区）。
export function ProjectDetailPage() {
  const { id } = useParams<{ id: string }>();
  const { data: project, isLoading } = useQuery({
    queryKey: ["projects", id],
    queryFn: () => getProject(id!),
    enabled: !!id,
  });

  // 浏览即选中：同步全局 selector（X-Torchwood-Project）到当前 :id，
  // 使离开本页后的项目作用域页面（Users/Storage 等）跟随最后浏览的项目。
  useProjectScopeSync(project?.id);

  if (isLoading) return <DetailSkeleton />;
  if (!project) return <NotFound backTo="/console/projects" />;

  return (
    <DetailPageWrapper
      title={project.name}
      description="项目详情"
      backTo="/console/projects"
      actions={
        <Button asChild variant="outline" size="sm">
          <Link to={`/console/projects/${project.id}/settings`}>
            <Settings className="h-4 w-4 mr-2" />
            项目设置
          </Link>
        </Button>
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: project.id, mono: true },
          { label: "名称", value: project.name },
          { label: "描述", value: project.description || "—" },
          { label: "状态", value: project.status },
          { label: "创建时间", value: new Date(project.created_at).toLocaleString() },
          { label: "更新时间", value: new Date(project.updated_at).toLocaleString() },
        ]}
      />
    </DetailPageWrapper>
  );
}
