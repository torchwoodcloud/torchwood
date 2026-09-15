import { useState } from "react";
import {
  useNavigate,
  useParams,
  useOutletContext,
} from "react-router-dom";
import {
  useQuery,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
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
  createCollection,
  updateCollection,
  createAttribute,
  createIndex,
  deleteAttribute,
  deleteIndex,
  type Collection,
  type Attribute,
} from "@/api/databases";
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
  const { dbId } = useParams<{ dbId: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
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

  const mutation = useMutation({
    mutationFn: () =>
      createCollection(dbId!, {
        id: id || name.toLowerCase().replace(/\s+/g, "_"),
        name,
        permissions: permissions.length > 0 ? permissions : undefined,
      }),
    onSuccess: (coll) => {
      toast.success("Collection 创建成功");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
      navigate(`/console/databases/${dbId}/collections/${coll.id}`);
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
        mutation.mutate();
      }}
      loading={mutation.isPending}
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
  const queryClient = useQueryClient();
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

  const updatePerms = useMutation({
    mutationFn: (input: { permissions: string[] }) =>
      updateCollection(dbId, collId, input),
    onSuccess: () => {
      toast.success("权限已更新");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId, collId] });
      setPermDialogOpen(false);
    },
  });

  const addAttribute = useMutation({
    mutationFn: (input: {
      key: string;
      type: string;
      size?: number;
      required: boolean;
      array: boolean;
    }) => createAttribute(dbId, collId, input),
    onSuccess: () => {
      toast.success("Attribute 已添加");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId, collId] });
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
      setAttrDialogOpen(false);
    },
  });

  const addIndex = useMutation({
    mutationFn: (input: { id: string; type: string; attributes: string[] }) =>
      createIndex(dbId, collId, {
        ...input,
        orders: input.attributes.map(() => "asc"),
      }),
    onSuccess: () => {
      toast.success("Index 已添加");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId, collId] });
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
      setIndexDialogOpen(false);
    },
  });

  const removeAttribute = useMutation({
    mutationFn: (attr: Attribute) => deleteAttribute(dbId, collId, attr.key),
    onSuccess: () => {
      toast.success("Attribute 已删除");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId, collId] });
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
    },
  });

  const removeIndex = useMutation({
    mutationFn: (index: Collection["indexes"][number]) => deleteIndex(dbId, collId, index.id),
    onSuccess: () => {
      toast.success("Index 已删除");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId, collId] });
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
    },
  });

  const updateSettings = useMutation({
    mutationFn: (input: { name?: string; disabled?: boolean }) =>
      updateCollection(dbId, collId, input),
    onSuccess: () => {
      toast.success("集合设置已更新");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId, collId] });
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
      setSettingsDialogOpen(false);
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
            onRemove={readonly ? undefined : (attr) => removeAttribute.mutate(attr)}
            disabled={readonly}
          />
          <IndexList
            indexes={collection.indexes}
            canAdd={collection.attributes.length > 0}
            onAdd={() => setIndexDialogOpen(true)}
            onRemove={readonly ? undefined : (index) => removeIndex.mutate(index)}
            disabled={readonly}
          />
        </div>
      </div>

      <AddAttributeDialog
        open={attrDialogOpen}
        onOpenChange={setAttrDialogOpen}
        loading={addAttribute.isPending}
        onSubmit={(input) => addAttribute.mutate(input)}
      />
      <AddIndexDialog
        open={indexDialogOpen}
        onOpenChange={setIndexDialogOpen}
        loading={addIndex.isPending}
        attributes={collection.attributes}
        onSubmit={(input) => addIndex.mutate(input)}
      />
      <EditPermissionsDialog
        open={permDialogOpen}
        onOpenChange={setPermDialogOpen}
        loading={updatePerms.isPending}
        initialPermissions={collection.permissions}
        onSubmit={(perms) => updatePerms.mutate({ permissions: perms })}
      />
      <EditCollectionDialog
        open={settingsDialogOpen}
        onOpenChange={setSettingsDialogOpen}
        loading={updateSettings.isPending}
        initialName={collection.name}
        initialDisabled={collection.disabled ?? false}
        onSubmit={(input) => updateSettings.mutate(input)}
      />
    </>
  );
}

