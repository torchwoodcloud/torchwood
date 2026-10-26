import { useState } from "react";
import { useNavigate, useParams } from "@tanstack/react-router";
import { useOutletContext } from "@/router/outlet-context";
import { useQuery } from "@tanstack/react-query";
import { useCommand } from "@lynx-go/ui-kit";
import { toast } from "sonner";
import {
  Settings2,
  Shield,
  Hash,
  ListTree,
  Fingerprint,
  Calendar,
} from "lucide-react";
import {
  CollectionStatCard,
  type CollectionOutletContext,
} from "@/routes/databases/CollectionLayout";
import {
  getDatabase,
  getCollection,
} from "@/api/databases";
import {
  addCollectionAttributeCommand,
  addCollectionIndexCommand,
  createCollectionCommand,
  removeCollectionAttributeCommand,
  removeCollectionIndexCommand,
  updateCollectionPermissionsCommand,
  updateCollectionSettingsCommand,
} from "@/kit/databases";
import { describeApiError } from "@/kit/errors";
import { useAuth } from "@/hooks/useAuth";
import {
  useAdminRole,
  isPlatformAdmin,
} from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDate } from "@/lib/datetime";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  FormPageWrapper,
  FormField,
  DetailSkeleton,
  NotFound,
} from "@/components/resource/shared";
import { PermissionEditor } from "@/components/resource/PermissionEditor";

import {
  AttributeList,
  IndexList,
  AddAttributeDialog,
  AddIndexDialog,
  EditPermissionsDialog,
  EditCollectionDialog,
} from "./components";

export function CollectionNewPage() {
  const { dbId } = useParams({ strict: false });
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [name, setName] = useState("");
  const [id, setId] = useState("");
  const [permissions, setPermissions] = useState<string[]>([]);

  // 父资源（Database）不存在时渲染 NotFound（R11-P2-9）。
  const { data: database, isLoading: dbLoading, isError: dbError } = useQuery({
    queryKey: ["databases", dbId],
    queryFn: () => getDatabase(dbId!),
    enabled: !!dbId,
  });

  // 命令化：schema 定义表单的提交形收敛（ID 空则从名称派生、权限空不发送）
  // 在 descriptor run 内；ok 后失效 = kit collections 键 + 存量前缀
  //（resource.legacyKeys 声明，§12.10-②）。
  const create = useCommand(createCollectionCommand, {
    scope: { projectId: projectId ?? undefined, dbId: dbId ?? undefined },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Collection 创建成功");
        navigate({ to: `/console/databases/${dbId}/collections/${result.payload.id}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (dbLoading) return <DetailSkeleton />;
  if (dbError || !database) return <NotFound backTo="/console/databases" />;

  return (
    <FormPageWrapper
      title="新建 Collection"
      backTo={`/console/databases/${dbId}`}
      backLabel="返回 Database"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ dbId: dbId!, id, name, permissions });
      }}
      loading={create.pending}
      submitDisabled={!isPlatformAdmin(role)}
    >
      <FormField id="name" label="名称" value={name} onChange={setName} required placeholder="posts" />
      <FormField id="id" label="ID（可选）" value={id} onChange={setId} placeholder="posts" />
      <div className="pt-2 border-t">
        <PermissionEditor permissions={permissions} onChange={setPermissions} />
      </div>
    </FormPageWrapper>
  );
}

export function CollectionDetailPage() {
  const { dbId, collId } = useOutletContext<CollectionOutletContext>();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const [attrDialogOpen, setAttrDialogOpen] = useState(false);
  const [indexDialogOpen, setIndexDialogOpen] = useState(false);
  const [permDialogOpen, setPermDialogOpen] = useState(false);
  const [settingsDialogOpen, setSettingsDialogOpen] = useState(false);

  const { data: collection, isLoading } = useQuery({
    queryKey: ["collections", dbId, collId],
    queryFn: () => getCollection(dbId, collId),
  });

  // 命令接线（§12.3）：schema 编辑器写路径挂 collections 资源——失效 =
  // kit collections 键 + 存量前缀（resource.legacyKeys 声明，覆盖详情键
  // ["collections", dbId, collId] 与 db 前缀列表键，§12.10-②）；提交形
  // 收敛（索引 orders 全 asc）在 descriptor run 内。toast 单源 onSettled。
  const commandScope = { projectId: projectId ?? undefined, dbId: dbId ?? undefined };

  const updatePerms = useCommand(updateCollectionPermissionsCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("权限已更新");
        setPermDialogOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const addAttribute = useCommand(addCollectionAttributeCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Attribute 已添加");
        setAttrDialogOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const addIndex = useCommand(addCollectionIndexCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Index 已添加");
        setIndexDialogOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const removeAttribute = useCommand(removeCollectionAttributeCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Attribute 已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const removeIndex = useCommand(removeCollectionIndexCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Index 已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const updateSettings = useCommand(updateCollectionSettingsCommand, {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("集合设置已更新");
        setSettingsDialogOpen(false);
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!collection) return <NotFound backTo={`/console/databases/${dbId}`} />;

  const readonly = collection.is_system || !isPlatformAdmin(role);

  return (
    <>
      <div className="space-y-6">
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          <CollectionStatCard icon={Fingerprint} label="Collection ID" value={collection.id} mono />
          <CollectionStatCard
            icon={ListTree}
            label="Attributes"
            value={collection.attributes.length}
          />
          <CollectionStatCard icon={Hash} label="Indexes" value={collection.indexes.length} />
          <CollectionStatCard
            icon={Calendar}
            label="创建时间"
            value={formatDate(collection.created_at, tz)}
          />
        </div>

        <Card>
          <CardHeader className="flex flex-row items-start justify-between space-y-0">
            <div>
              <CardTitle className="flex items-center gap-2 text-base">
                <Settings2 className="h-4 w-4" />
                集合设置
              </CardTitle>
              <p className="mt-1 text-sm text-muted-foreground">
                名称与可用状态；停用后客户端不可读写该集合
              </p>
            </div>
            <Button
              size="sm"
              variant="outline"
              onClick={() => setSettingsDialogOpen(true)}
              disabled={readonly}
            >
              <Settings2 className="mr-1 h-4 w-4" />
              编辑
            </Button>
          </CardHeader>
          <CardContent>
            <dl className="grid gap-4 sm:grid-cols-2">
              <div>
                <dt className="text-sm text-muted-foreground">名称</dt>
                <dd className="mt-1 font-medium">{collection.name}</dd>
              </div>
              <div>
                <dt className="text-sm text-muted-foreground">状态</dt>
                <dd className="mt-1">
                  {collection.disabled ? (
                    <Badge variant="destructive">已停用</Badge>
                  ) : (
                    <Badge variant="secondary">启用</Badge>
                  )}
                </dd>
              </div>
            </dl>
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex flex-row items-start justify-between space-y-0">
            <div>
              <CardTitle className="flex items-center gap-2 text-base">
                <Shield className="h-4 w-4" />
                权限规则
              </CardTitle>
              <p className="mt-1 text-sm text-muted-foreground">
                集合级权限；无文档级权限的文档将回退到此规则
              </p>
            </div>
            <Button size="sm" variant="outline" onClick={() => setPermDialogOpen(true)} disabled={readonly}>
              <Settings2 className="mr-1 h-4 w-4" />
              编辑
            </Button>
          </CardHeader>
          <CardContent>
            {collection.permissions.length > 0 ? (
              <div className="flex flex-wrap gap-2">
                {collection.permissions.map((p) => (
                  <Badge key={p} variant="secondary" className="font-mono text-xs">
                    {p}
                  </Badge>
                ))}
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">
                未设置自定义权限规则，使用系统默认策略。
              </p>
            )}
          </CardContent>
        </Card>

        <div className="grid gap-6 lg:grid-cols-2">
          <AttributeList
            attributes={collection.attributes}
            onAdd={() => setAttrDialogOpen(true)}
            onRemove={
              readonly
                ? undefined
                : (attr) =>
                    removeAttribute.mutate({ dbId, collId, key: attr.key })
            }
            disabled={readonly}
          />
          <IndexList
            indexes={collection.indexes}
            canAdd={collection.attributes.length > 0}
            onAdd={() => setIndexDialogOpen(true)}
            onRemove={
              readonly
                ? undefined
                : (index) =>
                    removeIndex.mutate({ dbId, collId, indexId: index.id })
            }
            disabled={readonly}
          />
        </div>
      </div>

      <AddAttributeDialog
        open={attrDialogOpen}
        onOpenChange={setAttrDialogOpen}
        loading={addAttribute.pending}
        onSubmit={(input) => addAttribute.mutate({ dbId, collId, ...input })}
      />
      <AddIndexDialog
        open={indexDialogOpen}
        onOpenChange={setIndexDialogOpen}
        loading={addIndex.pending}
        attributes={collection.attributes}
        onSubmit={(input) => addIndex.mutate({ dbId, collId, ...input })}
      />
      <EditPermissionsDialog
        open={permDialogOpen}
        onOpenChange={setPermDialogOpen}
        loading={updatePerms.pending}
        initialPermissions={collection.permissions}
        onSubmit={(perms) => updatePerms.mutate({ dbId, collId, permissions: perms })}
      />
      <EditCollectionDialog
        open={settingsDialogOpen}
        onOpenChange={setSettingsDialogOpen}
        loading={updateSettings.pending}
        initialName={collection.name}
        initialDisabled={collection.disabled ?? false}
        onSubmit={(input) => updateSettings.mutate({ dbId, collId, ...input })}
      />
    </>
  );
}

