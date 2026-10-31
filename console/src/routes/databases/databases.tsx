import { useCallback, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQueryClient } from "@tanstack/react-query";
import {
  useCollection,
  useCommand,
  useRecord,
  invalidateAfterCommand,
} from "@lynx-go/uikit-core";
import { toast } from "sonner";
import { Plus } from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import {
  deleteDatabase,
  deleteCollection,
  type Database,
  type Collection,
} from "@/api/databases";
import {
  createDatabaseCommand,
  removeCollectionCommand,
  removeDatabaseCommand,
  collectionsResource,
  collectionsSearch,
  databasesResource,
  databasesSearch,
} from "@/kit/databases";
import { describeApiError } from "@/kit/errors";
import { useAuth } from "@/hooks/useAuth";
import {
  useAdminRole,
  isPlatformAdmin,
} from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { CardTitle } from "@/components/ui/card";
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
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";

// 列表 fleet 化（§12.9-2）：databases 顶层列表与 Database 详情内嵌的
// collections 列表换轨 useCollection（游标/页大小进 URL，分页栈 = 浏览器
// 历史）；页内当前页搜索保持本地态（旧页语义）；选择态受控归页面。
// 建删命令化：kit 列表键随资源命令 ok 自动失效；存量前缀键由资源
// legacyKeys 声明桥（§12.10-②，页面 onSettled 手动桥退役）。批量 N 连发
// 保持聚合直调形态（Promise.allSettled + 汇总 toast；失效经
// invalidateAfterCommand 复用资源句柄声明——kit 键 + 存量前缀一并失效）。

export function DatabasesListPage() {
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const platformAdmin = isPlatformAdmin(role);

  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider
  // 单源 + 未就绪自动停用。
  const list = useCollection(databasesResource, {
    from: "/console/databases",
    search: databasesSearch,
  });

  // 行删除命令化：失效 = kit 键 + 存量前缀（resource.legacyKeys 声明）。
  // RowDeleteButton 无表单容器——结果分支 toast。
  const remove = useCommand(removeDatabaseCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Database deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (d: Database) => `${d.id} ${d.name}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((d) => getSearchText(d).includes(search.toLowerCase()));
  // 搜索过滤的句柄视图（页面投影——四态/分页以服务端句柄为准，表体吃过滤行）
  const shownList = useMemo(() => ({ ...list, rows: shown }), [list, shown]);

  const toggleRow = (id: string) =>
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const toggleAll = (visibleIds: string[]) =>
    setSelectedIds((prev) =>
      new Set(visibleIds.length > 0 && visibleIds.every((id) => prev.has(id)) ? [] : visibleIds)
    );
  const selectedDatabases = list.rows.filter((d) => selectedIds.has(d.id));

  const handleBulkDelete = async () => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selectedDatabases.map((d) => deleteDatabase(d.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`Delete completed: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${selectedDatabases.length} databases`);
      }
      // 直调批量路径无命令容器：失效经 invalidateAfterCommand 复用资源句柄
      // 声明（kit 键刷新列表 + 连带集合缓存失活，allScopes 级联）。
      invalidateAfterCommand(queryClient, removeDatabaseCommand, {
        projectId: projectId ?? undefined,
      });
      setSelectedIds(new Set());
    } finally {
      setBulkDeleting(false);
    }
  };

  const tableColumns: LegacyColumnDef<Database, unknown>[] = [
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
      id: "created",
      header: "Created",
      cell: ({ row }) => formatDateTime(row.original.created_at, tz),
    },
    ...(platformAdmin
      ? [
          {
            id: "actions",
            header: "Actions",
            cell: ({ row }: { row: { original: Database } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<Database, unknown>,
        ]
      : []),
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Databases" description="Manage databases and collections" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search by name or ID on this page…"
              className="h-8 w-56"
            />
            {selectedDatabases.length > 0 && platformAdmin ? (
              <BulkDeleteButton
                count={selectedDatabases.length}
                loading={bulkDeleting}
                onConfirm={() => void handleBulkDelete()}
              />
            ) : null}
            {platformAdmin ? (
              <Button asChild className="ml-auto">
                <Link to="/console/databases/new">
                  <Plus className="h-4 w-4 mr-2" />
                  New Database
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
        emptyTitle={search !== "" ? "No matching databases" : "No databases yet"}
        emptyDescription={
          search !== ""
            ? "Adjust your search, or clear it to restore all rows on this page"
            : "Create your first database"
        }
        emptyAction={
          platformAdmin ? (
            <Button asChild size="sm">
              <Link to="/console/databases/new">New Database</Link>
            </Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(d) => d.id}
            onRowClick={(d) => navigate({ to: `/console/databases/${d.id}` })}
            selection={
              platformAdmin
                ? { selectedIds, onToggle: toggleRow, onToggleAll: toggleAll }
                : undefined
            }
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function DatabaseNewPage() {
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [name, setName] = useState("");
  const [id, setId] = useState("");

  // 命令化：ok 后失效 = kit 键 + 存量前缀（resource.legacyKeys 声明）；
  // 结果分支 toast（FormPageWrapper 无结果盒）。
  const create = useCommand(createDatabaseCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Database created");
        navigate({ to: `/console/databases/${result.payload.id}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <FormPageWrapper
      title="New Database"
      backTo="/console/databases"
      submitLabel="Create"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ id, name });
      }}
      loading={create.pending}
      submitDisabled={!isPlatformAdmin(role)}
    >
      <FormField id="name" label="Name" value={name} onChange={setName} required placeholder="Production DB" />
      <FormField
        id="id"
        label="ID"
        value={id}
        onChange={setId}
        required
        placeholder="app"
        hint="Must start with a lowercase letter; lowercase letters and digits only, up to 28 characters. Cannot be changed after creation."
      />
    </FormPageWrapper>
  );
}

export function DatabaseDetailPage() {
  const { dbId } = useParams({ strict: false });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const platformAdmin = isPlatformAdmin(role);

  // 详情读换轨 useRecord（子视图 kit 化批）：id 未就绪即停用（与旧 enabled
  // 同义）；projectId 未就绪由声明键自动停用；404 → null 走 NotFound 分支。
  const databaseRecord = useRecord(databasesResource, {
    id: dbId ?? undefined,
    scope: { projectId: projectId ?? undefined },
  });
  const database = databaseRecord.data;
  const dbLoading = databaseRecord.status === "loading";

  // 嵌套 from = Database 详情完整路径；dbId 进 scope（显式全量——嵌套 scope
  // 是替换语义；projectId 未就绪由资源声明的 scope 键自动停用）。
  const collList = useCollection(collectionsResource, {
    from: "/console/databases/$dbId",
    search: collectionsSearch,
    scope: { projectId: projectId ?? undefined, dbId: dbId ?? undefined },
  });

  // 删除命令化：失效 = kit 键 + 存量前缀（两资源各自 legacyKeys 声明）。
  // 结果分支 toast。
  const removeDb = useCommand(removeDatabaseCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Database deleted");
        navigate({ to: "/console/databases" });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const removeColl = useCommand(removeCollectionCommand, {
    // 0.4.0 类型执法：显式命令 scope 必须覆盖声明键全集；参数/项目未就绪时
    // 命令不可达，scope 缺省走 Provider 解析链。
    scope: projectId && dbId ? { projectId, dbId } : undefined,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Collection deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const collColumns: LegacyColumnDef<Collection, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="flex items-center gap-2">
          <span className="inline-block max-w-[140px] truncate font-mono text-xs">
            {row.original.id}
          </span>
          {/* 系统集合已不经 Databases 页列出（在 tw_<project>，走 Users/Storage/Groups）；徽章仅兜底 is_system。 */}
          {row.original.is_system && <Badge variant="secondary">System</Badge>}
        </span>
      ),
    },
    { id: "name", header: "Name", cell: ({ row }) => row.original.name },
    {
      id: "attributes",
      header: "Attributes",
      cell: ({ row }) => (
        <Badge variant="secondary">{row.original.attributes.length}</Badge>
      ),
    },
    {
      id: "indexes",
      header: "Indexes",
      cell: ({ row }) => <Badge variant="secondary">{row.original.indexes.length}</Badge>,
    },
    ...(platformAdmin
      ? [
          {
            id: "actions",
            header: "Actions",
            cell: ({ row }: { row: { original: Collection } }) =>
              row.original.is_system ? null : (
                <RowDeleteButton
                  onConfirm={() =>
                    removeColl.mutate({ dbId: dbId!, collId: row.original.id })
                  }
                  loading={removeColl.pending}
                />
              ),
          } satisfies LegacyColumnDef<Collection, unknown>,
        ]
      : []),
  ];

  const getCollSearchText = useCallback(
    (c: Collection) => `${c.id} ${c.name}`.toLowerCase(),
    []
  );
  const shownColls =
    search === ""
      ? collList.rows
      : collList.rows.filter((c) => getCollSearchText(c).includes(search.toLowerCase()));
  const shownCollList = useMemo(
    () => ({ ...collList, rows: shownColls }),
    [collList, shownColls]
  );

  const toggleRow = (id: string) =>
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const toggleAll = (visibleIds: string[]) =>
    setSelectedIds((prev) =>
      new Set(visibleIds.length > 0 && visibleIds.every((id) => prev.has(id)) ? [] : visibleIds)
    );
  const selectedCollections = collList.rows.filter((c) => selectedIds.has(c.id));

  const handleBulkDeleteColl = async () => {
    setBulkDeleting(true);
    try {
      // 系统集合不可删（选择面已禁选，此处兜底过滤）。
      const deletable = selectedCollections.filter((c) => !c.is_system);
      if (deletable.length === 0) {
        setSelectedIds(new Set());
        return;
      }
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        deletable.map((c) => deleteCollection(dbId!, c.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`Delete completed: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${deletable.length} collections`);
      }
      // 直调批量路径无命令容器：失效经 invalidateAfterCommand 复用资源句柄
      // 声明（kit collections 键刷新列表与详情 record）。
      invalidateAfterCommand(queryClient, removeCollectionCommand, {
        projectId: projectId ?? undefined,
        dbId: dbId ?? undefined,
      });
      setSelectedIds(new Set());
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
        description="Database details and collection management"
        backTo="/console/databases"
        actions={
          platformAdmin ? (
            <DeleteButton
              onConfirm={() => removeDb.mutate({ id: database.id })}
              loading={removeDb.pending}
            />
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: database.id, mono: true },
            { label: "Name", value: database.name },
            { label: "Created", value: formatDateTime(database.created_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <KitListPage
        list={shownCollList}
        toolbar={
          <div className="space-y-4 border-b px-4 py-4">
            <CardTitle className="text-base">Collections</CardTitle>
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={search}
                onChange={(e) => setSearch(e.target.value)}
                placeholder="Search by name or ID on this page…"
                className="h-8 w-56"
              />
              {selectedCollections.length > 0 && platformAdmin ? (
                <BulkDeleteButton
                  count={selectedCollections.filter((c) => !c.is_system).length}
                  loading={bulkDeleting}
                  onConfirm={() => void handleBulkDeleteColl()}
                />
              ) : null}
              {platformAdmin ? (
                <Button asChild className="ml-auto">
                  <Link to="/console/databases/$dbId/collections/new" params={{ dbId: dbId! }}>
                    <Plus className="h-4 w-4 mr-2" />
                    New Collection
                  </Link>
                </Button>
              ) : null}
            </div>
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={collList} pageSizeOptions={PAGE_SIZE_OPTIONS} />
          </div>
        }
        emptyTitle={search !== "" ? "No matching collections" : "No collections yet"}
        emptyDescription={
          search !== ""
            ? "Adjust your search, or clear it to restore all rows on this page"
            : "Create a collection in this database"
        }
        emptyAction={
          platformAdmin ? (
            <Button asChild size="sm">
              <Link to="/console/databases/$dbId/collections/new" params={{ dbId: dbId! }}>New Collection</Link>
            </Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownCollList}
            columns={collColumns}
            getRowId={(c) => c.id}
            onRowClick={(c) => navigate({ to: `/console/databases/${dbId}/collections/${c.id}` })}
            selection={
              platformAdmin
                ? {
                    selectedIds,
                    onToggle: toggleRow,
                    onToggleAll: toggleAll,
                    isRowSelectable: (c: Collection) => !c.is_system,
                  }
                : undefined
            }
          />
        </div>
      </KitListPage>
    </div>
  );
}
