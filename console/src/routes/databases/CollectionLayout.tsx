import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { NavLink } from "@/router/nav-link";
import { Outlet } from "@/router/outlet-context.tsx";
import { useCommand, useRecord } from "@lynx-go/uikit-core";
import { ArrowLeft, FileText, Layers, Radio } from "lucide-react";
import { toast } from "sonner";
import { collectionsResource, databasesResource, removeCollectionCommand } from "@/kit/databases";
import { describeApiError } from "@/kit/errors";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, isPlatformAdmin } from "@/hooks/useAdminRole";
import { PageHeader } from "@/components/PageHeader";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { DeleteButton, DetailSkeleton, NotFound } from "@/components/resource/shared";
import { cn } from "@/lib/utils";

export type CollectionOutletContext = {
  dbId: string;
  collId: string;
};

const navLinkClass = ({ isActive }: { isActive: boolean }) =>
  cn(
    "inline-flex items-center gap-2 border-b-2 px-1 pb-3 pt-1 text-sm font-medium transition-colors",
    isActive
      ? "border-primary text-foreground"
      : "border-transparent text-muted-foreground hover:border-muted-foreground/40 hover:text-foreground"
  );

export function CollectionLayout() {
  const { dbId, collId } = useParams({ strict: false });
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const platformAdmin = isPlatformAdmin(role);

  // 父子详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧
  // enabled 同义）；scope 显式全量，projectId 未就绪由声明键自动停用。
  const databaseRecord = useRecord(databasesResource, {
    id: dbId ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const collectionRecord = useRecord(collectionsResource, {
    id: collId ?? undefined,
    scope: { projectId: projectId ?? undefined, dbId: dbId ?? undefined },
  });
  const database = databaseRecord.data;
  const collection = collectionRecord.data;
  const isLoading = collectionRecord.status === "loading";

  // 删除命令化：失效 = kit collections 键 + 存量前缀（resource.legacyKeys
  // 声明，§12.10-②）。DeleteButton 无表单容器——结果分支 toast。
  // 0.4.0 类型执法：显式命令 scope 必须覆盖声明键全集；参数/项目未就绪时
  // 命令不可达（下方 NotFound 兜底），scope 缺省走 Provider 解析链。
  const commandScope = projectId && dbId ? { projectId, dbId } : undefined;
  const remove = useCommand(removeCollectionCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Collection deleted");
        navigate({ to: `/console/databases/${dbId}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!collection || !dbId || !collId) {
    return <NotFound backTo={`/console/databases/${dbId}`} />;
  }

  const basePath = `/console/databases/${dbId}/collections/${collId}`;

  return (
    <div className="space-y-6">
      <PageHeader
        title={
          <span className="inline-flex items-center gap-2">
            {collection.name}
            {collection.disabled && <Badge variant="destructive">Disabled</Badge>}
            {/* 系统集合不经本页管理；徽章仅兜底 is_system。 */}
            {collection.is_system && <Badge variant="secondary">System</Badge>}
          </span>
        }
        description={database ? `${database.name} · ${collection.id}` : collection.id}
        actions={
          collection.is_system || !platformAdmin ? undefined : (
            <DeleteButton
              onConfirm={() => remove.mutate({ dbId: dbId!, collId: collId! })}
              loading={remove.pending}
            />
          )
        }
      />

      <div className="space-y-4">
        <Button variant="ghost" size="sm" className="w-fit" asChild>
          <Link to="/console/databases/$dbId" params={{ dbId: dbId }}>
            <ArrowLeft className="mr-2 h-4 w-4" />
            Back to database
          </Link>
        </Button>

        <nav className="-mb-px flex gap-6 border-b">
          <NavLink to={basePath} end className={navLinkClass}>
            <Layers className="h-4 w-4" />
            Schema
          </NavLink>
          <NavLink to={`${basePath}/documents`} className={navLinkClass}>
            <FileText className="h-4 w-4" />
            Documents
          </NavLink>
          {/* 试听：仅 platform admin；系统 / 停用集合 Realtime 订阅会被拒，不展示 */}
          {platformAdmin && !collection.is_system && !collection.disabled && (
            <NavLink to={`${basePath}/listen`} className={navLinkClass}>
              <Radio className="h-4 w-4" />
              Listen
            </NavLink>
          )}
        </nav>
      </div>

      <Outlet context={{ dbId, collId } satisfies CollectionOutletContext} />
    </div>
  );
}

export function CollectionStatCard({
  icon: Icon,
  label,
  value,
  mono,
}: {
  icon: React.ComponentType<{ className?: string }>;
  label: string;
  value: React.ReactNode;
  mono?: boolean;
}) {
  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Icon className="h-4 w-4" />
        {label}
      </div>
      <p className={cn("mt-2 text-lg font-semibold", mono && "font-mono text-sm break-all")}>
        {value}
      </p>
    </div>
  );
}
