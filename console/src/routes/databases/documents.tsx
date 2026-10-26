import {
  useCallback,
  useRef,
  useState,
  useEffect,
  useMemo,
} from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useOutletContext } from "@/router/outlet-context";
import {
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { useCollection, useCommand } from "@lynx-go/uikit-core";
import { toast } from "sonner";
import {
  ArrowLeft,
  Pencil,
  Plus,
  Settings2,
} from "lucide-react";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { type CollectionOutletContext } from "@/routes/databases/CollectionLayout";
import {
  getCollection,
  getDocument,
  bulkUpdateDocuments,
  bulkDeleteDocuments,
  type Attribute,
  type Document,
} from "@/api/databases";
import {
  useAdminRole,
  canWrite,
} from "@/hooks/useAdminRole";
import { useAuth } from "@/hooks/useAuth";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { cn } from "@/lib/utils";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import { JsonEditor } from "@/components/ui/json-editor";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  Card,
  CardContent,
} from "@/components/ui/card";
import {
  FormField,
  DetailPageWrapper,
  DetailGrid,
  DetailSkeleton,
  NotFound,
  BulkDeleteButton,
  RowDeleteButton,
  DeleteButton,
} from "@/components/resource/shared";
import { PermissionEditor } from "@/components/resource/PermissionEditor";
import { Form as CommandForm } from "@/components/kit/command/form";
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS, PAGINATION_ZH } from "@/components/kit/query/pagination-zh";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import {
  createDocumentCommand,
  documentsResource,
  documentsSearch,
  invalidateDocumentsAfterBulkWrite,
  removeDocumentCommand,
  updateDocumentCommand,
} from "@/kit/documents";
import { describeApiError } from "@/kit/errors";

import { documentToValues, MAX_BULK_OPERATIONS } from "./collectionMeta";

// 列表 fleet 化（§12.9-2）：DocumentListSection 换轨 useCollection（嵌套
// from = 完整路径，dbId/collId 进 scope；游标/页大小进 URL，分页栈 = 浏览器
// 历史）；页内当前页搜索保持本地态（旧页语义）；只读态不给 selection/操作
// 列；批量更新对话框与行删命令（§12.5-8）原样保留。

function DocumentListSection({
  dbId,
  collId,
  attributes,
  readonly,
}: {
  dbId: string;
  collId: string;
  attributes: Attribute[];
  readonly?: boolean;
}) {
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [bulkUpdating, setBulkUpdating] = useState(false);
  const [bulkUpdateOpen, setBulkUpdateOpen] = useState(false);
  const [bulkUpdateIds, setBulkUpdateIds] = useState<string[]>([]);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const tz = useUserTimezone();
  const navigate = useNavigate();

  // 嵌套 from = 完整路径；scope 显式全量（嵌套是替换语义；projectId 未就绪
  // 由资源声明的 scope 键自动停用）。
  const list = useCollection(documentsResource, {
    from: "/console/databases/$dbId/collections/$collId/documents",
    search: documentsSearch,
    scope: { projectId: projectId ?? undefined, dbId, collId },
  });

  // 命令/失效共用的嵌套 scope（与列表绑定同形——kit selector 的 scope 相交
  // 按全量对象匹配）。
  const commandScope = { projectId: projectId ?? undefined, dbId, collId };

  // 行删除命令化（§12.5-8）：无表单容器——结果分支以 toast 呈现；失效 =
  // descriptor 声明（kit documents 键自动刷新列表 + 存量 ["documents"]
  // 前缀，§12.10-②——手动桥与 list.refetch 退役）。
  const remove = useCommand(removeDocumentCommand(dbId, collId), {
    scope: commandScope,
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Document 已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const columns: LegacyColumnDef<Document, unknown>[] = [
    {
      id: "id",
      header: "ID",
      cell: ({ row }) => (
        <span className="inline-block max-w-[160px] truncate font-mono text-xs">
          {row.original.id}
        </span>
      ),
    },
    {
      id: "updated",
      header: "更新时间",
      cell: ({ row }) => formatDateTime(row.original.updated_at, tz),
    },
    ...attributes.slice(0, 4).map((attr) => ({
      id: attr.key,
      header: attr.key,
      cell: ({ row }: { row: { original: Document } }) => {
        const val = row.original.data?.[attr.key];
        if (val == null) return "—";
        const text = typeof val === "object" ? JSON.stringify(val) : String(val);
        return text.length > 48 ? `${text.slice(0, 48)}…` : text;
      },
    })),
    ...(!readonly
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: Document } }) => (
              <RowDeleteButton
                onConfirm={() =>
                  remove.mutate({ docId: row.original.id, version: row.original.version })
                }
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<Document, unknown>,
        ]
      : []),
  ];

  const getSearchText = useCallback(
    (d: Document) => `${d.id} ${JSON.stringify(d.data ?? {})}`.toLowerCase(),
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
  const selectedDocuments = list.rows.filter((d) => selectedIds.has(d.id));
  const clearSelection = () => setSelectedIds(new Set());

  const handleBulkDelete = async (selected: Document[]) => {
    if (selected.length > MAX_BULK_OPERATIONS) {
      toast.error(`单次批量操作最多 ${MAX_BULK_OPERATIONS} 个 Document`);
      return;
    }
    setBulkDeleting(true);
    try {
      const affected = await bulkDeleteDocuments(
        dbId,
        collId,
        selected.map((d) => d.id)
      );
      toast.success(`已删除 ${affected} 个 Document`);
      // 直调批量路径无命令容器：复用文档命令的失效声明（§12.10-②）。
      invalidateDocumentsAfterBulkWrite(queryClient, commandScope);
      clearSelection();
    } finally {
      setBulkDeleting(false);
    }
  };

  const handleBulkUpdate = async (ids: string[], data: Record<string, unknown>) => {
    if (ids.length > MAX_BULK_OPERATIONS) {
      toast.error(`单次批量操作最多 ${MAX_BULK_OPERATIONS} 个 Document`);
      return;
    }
    setBulkUpdating(true);
    try {
      const affected = await bulkUpdateDocuments(dbId, collId, {
        document_ids: ids,
        data,
      });
      toast.success(`已更新 ${affected} 个 Document`);
      // 直调批量路径无命令容器：复用文档命令的失效声明（§12.10-②）。
      invalidateDocumentsAfterBulkWrite(queryClient, commandScope);
      clearSelection();
      setBulkUpdateOpen(false);
    } finally {
      setBulkUpdating(false);
    }
  };

  const documentsNewPath = `/console/databases/${dbId}/collections/${collId}/documents/new`;

  return (
    <>
      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索 Document ID 或字段内容..."
              className="h-8 w-56"
            />
            {!readonly && selectedDocuments.length > 0 ? (
              <>
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    setBulkUpdateIds(selectedDocuments.map((d) => d.id));
                    setBulkUpdateOpen(true);
                  }}
                >
                  <Settings2 className="mr-2 h-4 w-4" />
                  批量更新 ({selectedDocuments.length})
                </Button>
                <BulkDeleteButton
                  count={selectedDocuments.length}
                  loading={bulkDeleting}
                  onConfirm={() => void handleBulkDelete(selectedDocuments)}
                />
              </>
            ) : null}
            {!readonly ? (
              <Button asChild size="sm" className="ml-auto">
                <Link to={documentsNewPath}>
                  <Plus className="mr-2 h-4 w-4" />
                  新建 Document
                </Link>
              </Button>
            ) : null}
          </div>
        }
        pagination={
          <div className="px-4 pb-4">
            <KeysetPagination list={list} pageSizeOptions={PAGE_SIZE_OPTIONS} labels={PAGINATION_ZH} />
          </div>
        }
        emptyTitle={search !== "" ? "没有匹配的 Document" : "暂无 Document"}
        emptyDescription={
          search !== ""
            ? "调整搜索词，或清空恢复当前页全部行"
            : readonly
              ? undefined
              : "在此 Collection 中创建第一条文档记录"
        }
        emptyAction={
          readonly ? undefined : (
            <Button asChild size="sm">
              <Link to={documentsNewPath}>新建 Document</Link>
            </Button>
          )
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={columns}
            getRowId={(d) => d.id}
            onRowClick={(d) =>
              navigate({ to: `/console/databases/${dbId}/collections/${collId}/documents/${d.id}` })
            }
            selection={
              readonly
                ? undefined
                : { selectedIds, onToggle: toggleRow, onToggleAll: toggleAll }
            }
          />
        </div>
      </KitListPage>
      <BulkUpdateDialog
        open={bulkUpdateOpen}
        onOpenChange={setBulkUpdateOpen}
        loading={bulkUpdating}
        count={bulkUpdateIds.length}
        onSubmit={(data) => void handleBulkUpdate(bulkUpdateIds, data)}
      />
    </>
  );
}

function BulkUpdateDialog({
  open,
  onOpenChange,
  loading,
  count,
  onSubmit,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  loading: boolean;
  count: number;
  onSubmit: (data: Record<string, unknown>) => void;
}) {
  const [json, setJson] = useState("{}");

  useEffect(() => {
    if (open) setJson("{}");
  }, [open]);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="max-w-lg">
        <DialogHeader>
          <DialogTitle>批量更新 Document</DialogTitle>
          <DialogDescription>
            将 JSON 中的字段合并写入选中的 {count} 个 Document（单次最多 {MAX_BULK_OPERATIONS} 个）；留空的字段不会改动。
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            let data: unknown;
            try {
              data = JSON.parse(json);
            } catch {
              toast.error("JSON 格式无效");
              return;
            }
            if (data === null || typeof data !== "object" || Array.isArray(data)) {
              toast.error("必须是 JSON 对象，如 {\"status\":\"published\"}");
              return;
            }
            if (Object.keys(data).length === 0) {
              toast.error("至少填写一个字段");
              return;
            }
            onSubmit(data as Record<string, unknown>);
          }}
          className="space-y-4"
        >
          <div className="space-y-2">
            <Label htmlFor="bulk-json">字段 (JSON)</Label>
            <JsonEditor
              id="bulk-json"
              value={json}
              onChange={setJson}
              minHeightClass="min-h-[160px]"
              maxHeightClass="max-h-[50vh]"
            />
          </div>
          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              取消
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? "更新中..." : "更新"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

export function DocumentsListPage() {
  const { dbId, collId } = useOutletContext<CollectionOutletContext>();
  const { role } = useAdminRole();

  const { data: collection, isLoading } = useQuery({
    queryKey: ["collections", dbId, collId],
    queryFn: () => getCollection(dbId, collId),
  });

  if (isLoading) return <DetailSkeleton />;
  if (!collection) return <NotFound backTo={`/console/databases/${dbId}`} />;

  return (
    <DocumentListSection
      dbId={dbId}
      collId={collId}
      attributes={collection.attributes}
      readonly={collection.is_system || !canWrite(role)}
    />
  );
}

// 字段级校验文案（rejected.fieldErrors——客户端收敛是 fieldErrors 的唯一
// 真实来源，§12.5-5）；errors 键与 attr.key 对齐（增量区为 "keyΔ"）。
function FieldError({ message }: { message?: string }) {
  if (!message) return null;
  return <p className="text-xs text-destructive">{message}</p>;
}

function DocumentFormFields({
  attributes,
  values,
  onChange,
  errors,
}: {
  attributes: Attribute[];
  values: Record<string, string>;
  onChange: (key: string, value: string) => void;
  errors?: Record<string, string>;
}) {
  if (attributes.length === 0) {
    return (
      <div className="space-y-2">
        <Label htmlFor="payload">Data (JSON)</Label>
        <JsonEditor
          id="payload"
          value={values.__json ?? "{}"}
          onChange={(v) => onChange("__json", v)}
        />
        <FieldError message={errors?.__json} />
      </div>
    );
  }

  return (
    <>
      {attributes.map((attr) =>
        attr.type === "json" ? (
          <div key={attr.key} className="space-y-2">
            <Label htmlFor={attr.key}>
              {attr.key} ({attr.type})
            </Label>
            <JsonEditor
              id={attr.key}
              value={values[attr.key] ?? ""}
              onChange={(v) => onChange(attr.key, v)}
              required={attr.required}
            />
            <FieldError message={errors?.[attr.key]} />
          </div>
        ) : (
          <div key={attr.key} className="max-w-lg">
            <FormField
              id={attr.key}
              label={`${attr.key} (${attr.type})`}
              value={values[attr.key] ?? ""}
              onChange={(v) => onChange(attr.key, v)}
              required={attr.required}
              type={attr.type === "integer" || attr.type === "float" ? "number" : "text"}
            />
            <FieldError message={errors?.[attr.key]} />
          </div>
        )
      )}
    </>
  );
}

// 只读视图：与编辑态共用 documentToValues 的字符串值，JSON 以等宽代码块呈现，
// 空值显示占位符；默认展示此组件，点「编辑」才切换到表单。
function DocumentReadonlyFields({
  attributes,
  values,
}: {
  attributes: Attribute[];
  values: Record<string, string>;
}) {
  const boxClass = "rounded-md border bg-muted/40 px-3 py-2 text-sm";
  if (attributes.length === 0) {
    return (
      <div className="space-y-2">
        <Label>Data (JSON)</Label>
        <pre
          className={cn(
            boxClass,
            "max-h-[65vh] overflow-auto whitespace-pre-wrap break-words font-mono"
          )}
        >
          {values.__json || "{}"}
        </pre>
      </div>
    );
  }
  return (
    <>
      {attributes.map((attr) => {
        const value = values[attr.key] ?? "";
        return (
          <div
            key={attr.key}
            className={cn("space-y-2", attr.type !== "json" && "max-w-lg")}
          >
            <Label>
              {attr.key} ({attr.type})
            </Label>
            {attr.type === "json" ? (
              <pre
                className={cn(
                  boxClass,
                  "max-h-[65vh] overflow-auto whitespace-pre-wrap break-words font-mono"
                )}
              >
                {value === "" ? "—" : value}
              </pre>
            ) : (
              <p
                className={cn(
                  boxClass,
                  "break-words whitespace-pre-wrap",
                  value === "" && "text-muted-foreground"
                )}
              >
                {value === "" ? "—" : value}
              </p>
            )}
          </div>
        );
      })}
    </>
  );
}

export function DocumentNewPage() {
  const { dbId, collId } = useParams({ strict: false });
  const navigate = useNavigate();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [values, setValues] = useState<Record<string, string>>({ __json: "{}" });
  const [permissions, setPermissions] = useState<string[]>([]);

  const { data: collection, isLoading } = useQuery({
    queryKey: ["collections", dbId, collId],
    queryFn: () => getCollection(dbId!, collId!),
    enabled: !!dbId && !!collId,
  });

  // 创建命令化（§12.5）：表单值展示形在 descriptor.run 内收敛；rejected 的
  // fieldErrors 回流字段级红字；conflict（重复 ID）/其余 rejected 走结果盒。
  // 失效 = descriptor 声明（kit 键 + 存量前缀，§12.10-②——scope 传全量嵌套
  // 对象与列表绑定同形相交）。
  const create = useCommand(createDocumentCommand(dbId!, collId!, collection?.attributes ?? []), {
    scope: { projectId: projectId ?? undefined, dbId: dbId!, collId: collId! },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Document 已创建");
        navigate({ to: `/console/databases/${dbId}/collections/${collId}/documents/${result.payload.id}` });
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });
  const fieldErrors =
    create.result?.status === "rejected" ? create.result.fieldErrors : undefined;

  if (isLoading) return <DetailSkeleton />;
  if (!collection) {
    return <NotFound backTo={`/console/databases/${dbId}/collections/${collId}/documents`} />;
  }

  const documentsPath = `/console/databases/${dbId}/collections/${collId}/documents`;
  const submit = () => {
    create.mutate({ values, permissions });
  };

  return (
    <div className="mx-auto max-w-4xl space-y-6 p-6">
      <div className="flex items-start gap-4">
        <Button variant="ghost" size="icon" asChild aria-label="返回">
          <Link to={documentsPath}>
            <ArrowLeft className="h-4 w-4" />
          </Link>
        </Button>
        <div>
          <h1 className="text-lg font-semibold">新建 Document</h1>
          <p className="text-sm text-muted-foreground">Collection: {collection.name}</p>
        </div>
      </div>
      {/* CommandForm 内芯持有提交（FormPageWrapper 的 onSubmit 外壳与之互斥，
          §12.3 方言③——api-keys 新建页同款结构）；无写权限不给可提交面 */}
      <div className="max-w-3xl space-y-4">
        {canWrite(role) ? (
          <CommandForm
            form={{ handleSubmit: submit }}
            submitting={create.pending}
            result={create.result}
            submitLabel="创建"
            submittingLabel="创建中…"
            cancelLabel="取消"
            onCancel={() =>
                    void navigate({
                      to: "/console/databases/$dbId/collections/$collId/documents",
                      params: { dbId: dbId!, collId: collId! },
                    })}
          >
            <DocumentFormFields
              attributes={collection.attributes}
              values={values}
              onChange={(key, value) => setValues((prev) => ({ ...prev, [key]: value }))}
              errors={fieldErrors}
            />
            <div className="pt-2 border-t">
              <PermissionEditor permissions={permissions} onChange={setPermissions} />
            </div>
          </CommandForm>
        ) : (
          <p className="text-sm text-muted-foreground">当前角色没有该集合的写权限。</p>
        )}
      </div>
    </div>
  );
}

export function DocumentDetailPage() {
  const { dbId, collId, docId } = useParams({ strict: false });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const { projectId } = useAuth();
  const tz = useUserTimezone();
  const [values, setValues] = useState<Record<string, string>>({});
  const [increments, setIncrements] = useState<Record<string, string>>({});
  const [editing, setEditing] = useState(false);
  const initializedDocRef = useRef<string | null>(null);
  const writeable = canWrite(role);

  const { data: collection } = useQuery({
    queryKey: ["collections", dbId, collId],
    queryFn: () => getCollection(dbId!, collId!),
    enabled: !!dbId && !!collId,
  });

  const { data: document, isLoading } = useQuery({
    queryKey: ["documents", dbId, collId, docId],
    queryFn: () => getDocument(dbId!, collId!, docId!),
    enabled: !!dbId && !!collId && !!docId,
  });

  // 仅在切换到新文档（id 变化）时重建表单，避免后台 refetch 覆盖编辑中的草稿；
  // collection 未就绪时不初始化，防止以空 attributes 建表单。
  useEffect(() => {
    if (!document || !collection || initializedDocRef.current === document.id) return;
    initializedDocRef.current = document.id;
    setValues(documentToValues(collection.attributes, document));
    setIncrements({});
    setEditing(false);
  }, [collection, document]);

  // 只读视图直接由服务端文档与元数据派生，不依赖表单同步时序。
  const displayValues = useMemo(
    () => (document ? documentToValues(collection?.attributes ?? [], document) : {}),
    [collection, document]
  );

  const startEdit = () => {
    if (!document || !collection) return;
    setValues(documentToValues(collection.attributes, document));
    setIncrements({});
    setEditing(true);
  };

  const cancelEdit = () => {
    if (!document || !collection) return;
    setValues(documentToValues(collection.attributes, document));
    setIncrements({});
    setEditing(false);
  };

  // OCC 主面（§12.5）：保存命令化——version 随 data/increment 同一次 PATCH；
  // rejected.fieldErrors 回流字段红字；conflict（版本不匹配）→ 结果盒 +
  // 「刷新重载」（fetchQuery fresh-read 重建表单，草稿按服务端态重置）。
  // 失效 = descriptor 声明（kit documents 键 + 存量 ["documents"] 前缀——
  // 详情键经前缀失效，§12.10-②）。
  const save = useCommand(
    updateDocumentCommand(dbId!, collId!, collection?.attributes ?? []),
    {
      scope: { projectId: projectId ?? undefined, dbId: dbId!, collId: collId! },
      onSettled: (result, failure) => {
        if (result?.status === "ok") {
          toast.success("Document 已更新");
          setIncrements({});
          // 用响应文档重建表单，避免与（可能被自增/服务端归一化修改的）服务端状态失同步。
          setValues(documentToValues(collection?.attributes ?? [], result.payload));
          setEditing(false);
        } else if (failure) {
          toast.error(describeApiError(failure));
        }
        // rejected/conflict 不 toast——结果盒呈现（批 B 裁决 3）
      },
    }
  );

  // OCC 冲突的「刷新重载」：刻意的 fetchQuery fresh-read（staleTime 0 绕过
  // 缓存重建草稿），非失效路径——保留（§12.5-3）。
  const reloadAfterConflict = async () => {
    save.reset();
    const fresh = await queryClient.fetchQuery({
      queryKey: ["documents", dbId, collId, docId],
      queryFn: () => getDocument(dbId!, collId!, docId!),
      staleTime: 0,
    });
    setValues(documentToValues(collection?.attributes ?? [], fresh));
    setIncrements({});
  };

  const remove = useCommand(removeDocumentCommand(dbId!, collId!), {
    scope: { projectId: projectId ?? undefined, dbId: dbId!, collId: collId! },
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Document 已删除");
        navigate({ to: `/console/databases/${dbId}/collections/${collId}/documents` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const documentsPath = `/console/databases/${dbId}/collections/${collId}/documents`;

  if (isLoading) return <DetailSkeleton />;
  if (!document || !collection) {
    return <NotFound backTo={documentsPath} />;
  }

  return (
    <DetailPageWrapper
      title="Document"
      description={`ID: ${document.id}`}
      backTo={documentsPath}
      backLabel="返回文档列表"
      actions={
        collection.is_system || !writeable || editing ? null : (
          <>
            <Button size="sm" variant="outline" onClick={startEdit}>
              <Pencil className="mr-2 h-4 w-4" />
              编辑
            </Button>
            <DeleteButton
              onConfirm={() =>
                remove.mutate({ docId: docId!, version: document.version })
              }
              loading={remove.pending}
            />
          </>
        )
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: document.id, mono: true },
          { label: "创建时间", value: formatDateTime(document.created_at, tz) },
          { label: "更新时间", value: formatDateTime(document.updated_at, tz) },
        ]}
      />
      <Card className="mt-6">
        <CardContent className="pt-6">
          {editing ? (
            <CommandForm
              form={{
                handleSubmit: () => {
                  save.mutate({
                    docId: docId!,
                    values,
                    increments,
                    // OCC：data 与 increment 同一次 PATCH 使用同一个版本。
                    version: document.version,
                  });
                },
              }}
              submitting={save.pending}
              result={save.result}
              resultAction={
                save.result?.status === "conflict" ? (
                  <Button
                    type="button"
                    size="sm"
                    variant="outline"
                    onClick={() => void reloadAfterConflict()}
                  >
                    刷新重载
                  </Button>
                ) : undefined
              }
              submitLabel="保存"
              submittingLabel="保存中…"
              cancelLabel="取消"
              onCancel={cancelEdit}
            >
              <DocumentFormFields
                attributes={collection.attributes}
                values={values}
                onChange={(key, value) => setValues((prev) => ({ ...prev, [key]: value }))}
                errors={
                  save.result?.status === "rejected" ? save.result.fieldErrors : undefined
                }
              />
              {collection.attributes.some((a) => a.type === "integer" || a.type === "float") && (
                <div className="rounded-lg border p-4 space-y-3 max-w-lg">
                  <p className="text-sm font-medium">字段自增</p>
                  <p className="text-xs text-muted-foreground">
                    对数值字段做原子增减，不覆盖当前值；保存后立即生效（增量必须为整数）。
                  </p>
                  <div className="grid gap-3 sm:grid-cols-2">
                    {collection.attributes
                      .filter((a) => a.type === "integer" || a.type === "float")
                      .map((attr) => (
                        <div key={attr.key} className="space-y-2">
                          <Label htmlFor={`inc-${attr.key}`}>{attr.key} Δ</Label>
                          <Input
                            id={`inc-${attr.key}`}
                            value={increments[attr.key] ?? ""}
                            onChange={(e) =>
                              setIncrements((prev) => ({ ...prev, [attr.key]: e.target.value }))
                            }
                            placeholder="如 1、-1"
                            type="number"
                            step="any"
                          />
                          <FieldError
                            message={
                              save.result?.status === "rejected"
                                ? save.result.fieldErrors?.[`${attr.key}Δ`]
                                : undefined
                            }
                          />
                        </div>
                      ))}
                  </div>
                </div>
              )}
            </CommandForm>
          ) : (
            <div className="space-y-4">
              <DocumentReadonlyFields
                attributes={collection.attributes}
                values={displayValues}
              />
            </div>
          )}
        </CardContent>
      </Card>
    </DetailPageWrapper>
  );
}
