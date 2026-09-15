import {
  useCallback,
  useState,
} from "react";
import {
  Link,
  useNavigate,
  useParams,
} from "react-router-dom";
import {
  useQuery,
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import {
  listDatabases,
  getDatabase,
  createDatabase,
  deleteDatabase,
  listCollections,
  deleteCollection,
  type Database,
  type Collection,
} from "@/api/databases";
import { useAuth } from "@/hooks/useAuth";
import {
  useAdminRole,
  isPlatformAdmin,
} from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
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
  BulkDeleteButton,
  RowDeleteButton,
  DeleteButton,
} from "@/components/resource/shared";

// 模块级 columns 无法用 hook，工厂化注入管理员时区偏好。
const dbColumns = (tz: string): ColumnDef<Database>[] => [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (d) => d.id,
  },
  { key: "name", header: "名称", cell: (d) => d.name },
  {
    key: "created",
    header: "创建时间",
    cell: (d) => formatDateTime(d.created_at, tz),
  },
];

export function DatabasesListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const platformAdmin = isPlatformAdmin(role);

  const { data: databases = [], isLoading } = useQuery({
    queryKey: ["databases", projectId],
    queryFn: listDatabases,
    enabled: !!projectId,
  });

  const remove = useMutation({
    mutationFn: (id: string) => deleteDatabase(id),
    onSuccess: () => {
      toast.success("Database 已删除");
      queryClient.invalidateQueries({ queryKey: ["databases", projectId] });
    },
  });

  const getSearchText = useCallback((d: Database) => `${d.id} ${d.name}`, []);

  const handleBulkDelete = async (selected: Database[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((d) => deleteDatabase(d.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个 Database`);
      }
      queryClient.invalidateQueries({ queryKey: ["databases", projectId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  return (
    <ResourceListPage
      title="Databases"
      description="管理数据库与集合"
      searchPlaceholder="搜索数据库名称或 ID..."
      isLoading={isLoading}
      items={databases}
      columns={dbColumns(tz)}
      getSearchText={getSearchText}
      detailPath={(d) => `/console/databases/${d.id}`}
      toolbarActions={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/databases/new">
              <Plus className="h-4 w-4 mr-2" />
              新建 Database
            </Link>
          </Button>
        ) : undefined
      }
      selectionActions={
        platformAdmin
          ? (selected, clear) => (
              <BulkDeleteButton
                count={selected.length}
                loading={bulkDeleting}
                onConfirm={() => handleBulkDelete(selected, clear)}
              />
            )
          : undefined
      }
      rowActions={
        platformAdmin
          ? (d) => (
              <RowDeleteButton onConfirm={() => remove.mutate(d.id)} loading={remove.isPending} />
            )
          : undefined
      }
      emptyTitle="暂无 Database"
      emptyDescription="创建第一个 Database"
      emptyAction={
        platformAdmin ? (
          <Button asChild>
            <Link to="/console/databases/new">新建 Database</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function DatabaseNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [name, setName] = useState("");
  const [id, setId] = useState("");

  const mutation = useMutation({
    mutationFn: createDatabase,
    onSuccess: (db) => {
      toast.success("Database 创建成功");
      queryClient.invalidateQueries({ queryKey: ["databases", projectId] });
      navigate(`/console/databases/${db.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建 Database"
      backTo="/console/databases"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        mutation.mutate({
          id,
          name,
        });
      }}
      loading={mutation.isPending}
      submitDisabled={!isPlatformAdmin(role)}
    >
      <FormField id="name" label="名称" value={name} onChange={setName} required placeholder="Production DB" />
      <FormField
        id="id"
        label="ID"
        value={id}
        onChange={setId}
        required
        placeholder="app"
        hint="小写字母开头，仅小写字母与数字，最长 28。创建后不可改。"
      />
    </FormPageWrapper>
  );
}

export function DatabaseDetailPage() {
  const { dbId } = useParams<{ dbId: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const platformAdmin = isPlatformAdmin(role);

  const { data: database, isLoading: dbLoading } = useQuery({
    queryKey: ["databases", dbId],
    queryFn: () => getDatabase(dbId!),
    enabled: !!dbId,
  });

  const { data: collections = [], isLoading: collLoading } = useQuery({
    queryKey: ["collections", dbId],
    queryFn: () => listCollections(dbId!),
    enabled: !!dbId,
  });

  const removeDb = useMutation({
    mutationFn: (id: string) => deleteDatabase(id),
    onSuccess: () => {
      toast.success("Database 已删除");
      queryClient.invalidateQueries({ queryKey: ["databases", projectId] });
      navigate("/console/databases");
    },
  });

  const removeColl = useMutation({
    mutationFn: (collId: string) => deleteCollection(dbId!, collId),
    onSuccess: () => {
      toast.success("Collection 已删除");
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
    },
  });

  const collColumns: ColumnDef<Collection>[] = [
    {
      key: "id",
      header: "ID",
      className: "font-mono text-xs max-w-[140px] truncate",
      cell: (c) => (
        <span className="flex items-center gap-2">
          <span className="truncate">{c.id}</span>
          {/* 系统集合已不经 Databases 页列出（在 tw_<project>，走 Users/Storage/Groups）；徽章仅兜底 is_system。 */}
          {c.is_system && <Badge variant="secondary">系统</Badge>}
        </span>
      ),
    },
    { key: "name", header: "名称", cell: (c) => c.name },
    {
      key: "attributes",
      header: "Attributes",
      cell: (c) => <Badge variant="secondary">{c.attributes.length}</Badge>,
    },
    {
      key: "indexes",
      header: "Indexes",
      cell: (c) => <Badge variant="secondary">{c.indexes.length}</Badge>,
    },
  ];

  const getCollSearchText = useCallback(
    (c: Collection) => `${c.id} ${c.name}`,
    []
  );

  const handleBulkDeleteColl = async (selected: Collection[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      const deletable = selected.filter((c) => !c.is_system);
      if (deletable.length === 0) {
        clear();
        return;
      }
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        deletable.map((c) => deleteCollection(dbId!, c.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${deletable.length} 个 Collection`);
      }
      queryClient.invalidateQueries({ queryKey: ["collections", dbId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  if (dbLoading) return <DetailSkeleton />;
  if (!database) return <NotFound backTo="/console/databases" />;

  return (
    <div className="space-y-6">
      <DetailPageWrapper
        title={database.name}
        description="Database 详情与 Collection 管理"
        backTo="/console/databases"
        actions={
          platformAdmin ? (
            <DeleteButton
              onConfirm={() => removeDb.mutate(database.id)}
              loading={removeDb.isPending}
            />
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: database.id, mono: true },
            { label: "名称", value: database.name },
            { label: "创建时间", value: formatDateTime(database.created_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <ResourceListPage
        title=""
        cardTitle="Collections"
        searchPlaceholder="搜索 Collection..."
        isLoading={collLoading}
        items={collections}
        columns={collColumns}
        getSearchText={getCollSearchText}
        isRowSelectable={(c) => !c.is_system}
        detailPath={(c) => `/console/databases/${dbId}/collections/${c.id}`}
        toolbarActions={
          platformAdmin ? (
            <Button asChild>
              <Link to={`/console/databases/${dbId}/collections/new`}>
                <Plus className="h-4 w-4 mr-2" />
                新建 Collection
              </Link>
            </Button>
          ) : undefined
        }
        selectionActions={
          platformAdmin
            ? (selected, clear) => (
                <BulkDeleteButton
                  count={selected.filter((c) => !c.is_system).length}
                  loading={bulkDeleting}
                  onConfirm={() => handleBulkDeleteColl(selected, clear)}
                />
              )
            : undefined
        }
        rowActions={
          platformAdmin
            ? (c) =>
                c.is_system ? null : (
                  <RowDeleteButton
                    onConfirm={() => removeColl.mutate(c.id)}
                    loading={removeColl.isPending}
                  />
                )
            : undefined
        }
        emptyTitle="暂无 Collection"
        emptyDescription="在此 Database 中创建 Collection"
        emptyAction={
          platformAdmin ? (
            <Button asChild>
              <Link to={`/console/databases/${dbId}/collections/new`}>新建 Collection</Link>
            </Button>
          ) : undefined
        }
      />
    </div>
  );
}

