import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/ui-kit";
import { toast } from "sonner";
import { Plus, Settings } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { getProject, type Project } from "@/api/projects";
import {
  projectsResource,
  projectsSearch,
  createProjectCommand,
} from "@/fleet/projects";
import { describeApiError } from "@/fleet/errors";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useProjectScopeSync } from "@/hooks/useProjectScopeSync";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/fleet/query/data-table";
import { KeysetPagination } from "@/components/fleet/query/keyset-pagination";
import { ListPage as FleetListPage } from "@/components/fleet/query/list-page";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import {
  FormPageWrapper,
  FormField,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
} from "@/components/resource/shared";

export function ProjectsListPage() {
  const { role } = useAdminRole();
  const navigate = useNavigate();
  const tz = useUserTimezone();
  const platformAdmin = isPlatformAdmin(role);
  const [search, setSearch] = useState("");

  // 列表 fleet 化（§12.9-2）：projects 是平台级资源——不随项目作用域；
  // 游标/页大小进 URL（分页栈 = 浏览器历史；旧页无服务端过滤）；页内当前
  // 页搜索保持本地态（旧页语义）。
  const list = useCollection(projectsResource, {
    from: "/console/projects",
    search: projectsSearch,
  });

  const getSearchText = useCallback(
    (p: Project) => `${p.id} ${p.name} ${p.description ?? ""} ${p.status}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((p) => getSearchText(p).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const tableColumns: LegacyColumnDef<Project, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="inline-block max-w-[140px] truncate font-mono text-xs">
          {row.original.id}
        </span>
      ),
    },
    { id: "name", header: "名称", cell: ({ row }) => row.original.name },
    {
      id: "status",
      header: "状态",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "active" ? "default" : "secondary"}>
          {row.original.status}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "创建时间",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Projects" description="管理 Torchwood 项目" />

      <FleetListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索项目名称或 ID..."
              className="h-8 w-56"
            />
            {platformAdmin ? (
              <Button asChild className="ml-auto">
                <Link to="/console/projects/new">
                  <Plus className="h-4 w-4 mr-2" />
                  新建项目
                </Link>
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的项目" : "暂无项目"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : "创建第一个项目以开始使用"
        }
        emptyAction={
          platformAdmin ? (
            <Button asChild>
              <Link to="/console/projects/new">新建项目</Link>
            </Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(row) => row.id}
            onRowClick={(p) => navigate({ to: `/console/projects/${p.id}` })}
          />
        </div>
      </FleetListPage>
    </div>
  );
}

export function ProjectNewPage() {
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [id, setId] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");

  // 创建命令接线（§12.3）：挂 projects 资源（kit 集合键自动失效）+ 存量
  // ["projects"] 键经 resource.legacyKeys 宽前缀失效（§12.10-②）；
  // description 空 = 不发送（run 内收敛）。toast 单源 onSettled。
  const create = useCommand(createProjectCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("项目创建成功");
        navigate({ to: `/console/projects/${result.payload.id}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
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
        create.mutate({ id, name, description: description || undefined });
      }}
      loading={create.pending}
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
  const { id } = useParams({ strict: false });
  const tz = useUserTimezone();
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
          <Link to="/console/projects/$id/settings" params={{ id: project.id }}>
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
          { label: "创建时间", value: formatDateTime(project.created_at, tz) },
          { label: "更新时间", value: formatDateTime(project.updated_at, tz) },
        ]}
      />
    </DetailPageWrapper>
  );
}
