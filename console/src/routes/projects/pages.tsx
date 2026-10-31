import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useCollection, useCommand, useRecord } from "@lynx-go/uikit-core";
import { toast } from "sonner";
import { Plus, Settings } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { type Project } from "@/api/projects";
import {
  projectsResource,
  projectsSearch,
  createProjectCommand,
} from "@/kit/projects";
import { describeApiError } from "@/kit/errors";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { useProjectScopeSync } from "@/hooks/useProjectScopeSync";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
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
    { id: "name", header: "Name", cell: ({ row }) => row.original.name },
    {
      id: "status",
      header: "Status",
      cell: ({ row }) => (
        <Badge variant={row.original.status === "active" ? "default" : "secondary"}>
          {row.original.status}
        </Badge>
      ),
    },
    {
      id: "created",
      header: "Created",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Projects" description="Manage Torchwood projects" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search projects by name or ID on this page…"
              className="h-8 w-56"
            />
            {platformAdmin ? (
              <Button size="sm" asChild className="ml-auto">
                <Link to="/console/projects/new">
                  <Plus className="h-4 w-4 mr-2" />
                  New Project
                </Link>
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching projects" : "No projects yet"}
        emptyDescription={
          search !== ""
            ? "Adjust your search, or clear it to restore all rows on this page"
            : "Create your first project to get started"
        }
        emptyAction={
          platformAdmin ? (
            <Button size="sm" asChild>
              <Link to="/console/projects/new">New Project</Link>
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
      </KitListPage>
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
        toast.success("Project created");
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
      title="New Project"
      description="Add a new project to the platform"
      backTo="/console/projects"
      submitLabel="Create"
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
        hint="Starts with a lowercase letter; only lowercase letters and digits, up to 28 characters. Cannot be changed after creation."
      />
      <FormField id="name" label="Name" value={name} onChange={setName} required placeholder="My Project" />
      <FormField
        id="description"
        label="Description"
        value={description}
        onChange={setDescription}
        placeholder="Optional description"
      />
    </FormPageWrapper>
  );
}

// ProjectDetailPage 项目概览（只读）：项目级配置统一在设置区
// /console/projects/:id/settings（基本信息编辑、注册与登录、OAuth、危险区）。
export function ProjectDetailPage() {
  const { id } = useParams({ strict: false });
  const tz = useUserTimezone();
  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；404 → null 空态直出（recordOrNull 手写方言，不引 restRecord）。
  // projectsResource 平台级无 scope 声明——useRecord 不传 scope。
  const projectRecord = useRecord(projectsResource, { id: id ?? undefined });
  const project = projectRecord.data;
  const isLoading = projectRecord.status === "loading";

  // 浏览即选中：同步全局 selector（X-Torchwood-Project）到当前 :id，
  // 使离开本页后的项目作用域页面（Users/Storage 等）跟随最后浏览的项目。
  useProjectScopeSync(project?.id);

  if (isLoading) return <DetailSkeleton />;
  if (!project) return <NotFound backTo="/console/projects" />;

  return (
    <DetailPageWrapper
      title={project.name}
      description="Project details"
      backTo="/console/projects"
      actions={
        <Button asChild variant="outline" size="sm">
          <Link to="/console/projects/$id/settings" params={{ id: project.id }}>
            <Settings className="h-4 w-4 mr-2" />
            Project Settings
          </Link>
        </Button>
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: project.id, mono: true },
          { label: "Name", value: project.name },
          { label: "Description", value: project.description || "—" },
          { label: "Status", value: project.status },
          { label: "Created", value: formatDateTime(project.created_at, tz) },
          { label: "Updated", value: formatDateTime(project.updated_at, tz) },
        ]}
      />
    </DetailPageWrapper>
  );
}
