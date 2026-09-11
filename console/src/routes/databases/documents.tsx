import {
  useCallback,
  useRef,
  useState,
  useEffect,
} from "react";
import {
  Link,
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
  Plus,
  Settings2,
} from "lucide-react";
import { type CollectionOutletContext } from "@/routes/databases/CollectionLayout";
import {
  getCollection,
  listDocuments,
  getDocument,
  createDocument,
  updateDocument,
  deleteDocument,
  bulkUpdateDocuments,
  bulkDeleteDocuments,
  type Attribute,
  type Document,
} from "@/api/databases";
import {
  useAdminRole,
  canWrite,
} from "@/hooks/useAdminRole";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
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
import { PermissionEditor } from "@/components/resource/PermissionEditor";

import { documentToValues, MAX_BULK_OPERATIONS } from "./collectionMeta";

const documentColumns: ColumnDef<Document>[] = [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[160px] truncate",
    cell: (d) => d.id,
  },
  {
    key: "updated",
    header: "更新时间",
    cell: (d) => new Date(d.updated_at).toLocaleString(),
  },
];

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
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [bulkUpdating, setBulkUpdating] = useState(false);
  const [bulkUpdateOpen, setBulkUpdateOpen] = useState(false);
  const [bulkUpdateIds, setBulkUpdateIds] = useState<string[]>([]);
  const clearRef = useRef<(() => void) | null>(null);
  const { data: documents = [], isLoading } = useQuery({
    queryKey: ["documents", dbId, collId],
    queryFn: () => listDocuments(dbId, collId),
  });

  const remove = useMutation({
    mutationFn: (doc: Document) => {
      if (!doc.version || doc.version <= 0) {
        throw new Error("文档缺少 OCC 版本信息，请刷新列表后重试");
      }
      return deleteDocument(dbId, collId, doc.id, doc.version);
    },
    onSuccess: () => {
      toast.success("Document 已删除");
      queryClient.invalidateQueries({ queryKey: ["documents", dbId, collId] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "删除失败"),
  });

  const columns: ColumnDef<Document>[] = [
    ...documentColumns,
    ...attributes.slice(0, 4).map((attr) => ({
      key: attr.key,
      header: attr.key,
      cell: (d: Document) => {
        const val = d.data?.[attr.key];
        if (val == null) return "—";
        const text = typeof val === "object" ? JSON.stringify(val) : String(val);
        return text.length > 48 ? `${text.slice(0, 48)}…` : text;
      },
    })),
  ];

  const getSearchText = useCallback(
    (d: Document) => `${d.id} ${JSON.stringify(d.data ?? {})}`,
    []
  );

  const handleBulkDelete = async (selected: Document[], clear: () => void) => {
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
      queryClient.invalidateQueries({ queryKey: ["documents", dbId, collId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  const handleBulkUpdate = async (
    ids: string[],
    data: Record<string, unknown>,
    clear: () => void
  ) => {
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
      queryClient.invalidateQueries({ queryKey: ["documents", dbId, collId] });
      clear();
      setBulkUpdateOpen(false);
    } finally {
      setBulkUpdating(false);
    }
  };

  return (
    <>
      <ResourceListPage
        cardTitle="文档"
        searchPlaceholder="搜索 Document ID 或字段内容..."
        isLoading={isLoading}
        items={documents}
        columns={columns}
        getSearchText={getSearchText}
        toolbarActions={
        readonly ? undefined : (
          <Button asChild size="sm">
            <Link to={`/console/databases/${dbId}/collections/${collId}/documents/new`}>
              <Plus className="mr-2 h-4 w-4" />
              新建 Document
            </Link>
          </Button>
        )
      }
      selectionActions={
        readonly
          ? undefined
          : (selected, clear) => (
              <div className="flex items-center gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  onClick={() => {
                    clearRef.current = clear;
                    setBulkUpdateIds(selected.map((d) => d.id));
                    setBulkUpdateOpen(true);
                  }}
                >
                  <Settings2 className="mr-2 h-4 w-4" />
                  批量更新 ({selected.length})
                </Button>
                <BulkDeleteButton
                  count={selected.length}
                  loading={bulkDeleting}
                  onConfirm={() => handleBulkDelete(selected, clear)}
                />
              </div>
            )
      }
      detailPath={(d) =>
        `/console/databases/${dbId}/collections/${collId}/documents/${d.id}`
      }
      rowActions={(d) =>
        readonly ? null : (
          <RowDeleteButton
            onConfirm={() => remove.mutate(d)}
            loading={remove.isPending}
          />
        )
      }
      emptyTitle="暂无 Document"
      emptyDescription={readonly ? undefined : "在此 Collection 中创建第一条文档记录"}
      emptyAction={
        readonly ? undefined : (
          <Button asChild size="sm">
            <Link to={`/console/databases/${dbId}/collections/${collId}/documents/new`}>
              新建 Document
            </Link>
          </Button>
        )
      }
    />
    <BulkUpdateDialog
      open={bulkUpdateOpen}
      onOpenChange={setBulkUpdateOpen}
      loading={bulkUpdating}
      count={bulkUpdateIds.length}
      onSubmit={(data) => handleBulkUpdate(bulkUpdateIds, data, clearRef.current ?? (() => {}))}
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
          <FormField
            id="bulk-json"
            label="字段 (JSON)"
            value={json}
            onChange={setJson}
            placeholder='{"status":"published"}'
          />
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

function parseFieldValue(type: string, raw: string): unknown {
  if (raw === "") return null;
  switch (type) {
    case "integer":
      return Number.parseInt(raw, 10);
    case "float":
      return Number.parseFloat(raw);
    case "boolean":
      return raw === "true";
    case "json":
      return JSON.parse(raw);
    default:
      return raw;
  }
}

function DocumentFormFields({
  attributes,
  values,
  onChange,
}: {
  attributes: Attribute[];
  values: Record<string, string>;
  onChange: (key: string, value: string) => void;
}) {
  if (attributes.length === 0) {
    return (
      <FormField
        id="payload"
        label="Data (JSON)"
        value={values.__json ?? "{}"}
        onChange={(v) => onChange("__json", v)}
        placeholder='{"title":"Hello"}'
      />
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
            <Textarea
              id={attr.key}
              value={values[attr.key] ?? ""}
              onChange={(e) => onChange(attr.key, e.target.value)}
              required={attr.required}
              className="font-mono min-h-[140px]"
            />
          </div>
        ) : (
          <FormField
            key={attr.key}
            id={attr.key}
            label={`${attr.key} (${attr.type})`}
            value={values[attr.key] ?? ""}
            onChange={(v) => onChange(attr.key, v)}
            required={attr.required}
            type={attr.type === "integer" || attr.type === "float" ? "number" : "text"}
          />
        )
      )}
    </>
  );
}

function buildDocumentData(
  attributes: Attribute[],
  values: Record<string, string>
): Record<string, unknown> {
  if (attributes.length === 0) {
    return JSON.parse(values.__json || "{}") as Record<string, unknown>;
  }
  const data: Record<string, unknown> = {};
  for (const attr of attributes) {
    if (values[attr.key] === undefined || values[attr.key] === "") {
      if (attr.required) {
        throw new Error(`${attr.key} is required`);
      }
      continue;
    }
    data[attr.key] = parseFieldValue(attr.type, values[attr.key]);
  }
  return data;
}

export function DocumentNewPage() {
  const { dbId, collId } = useParams();
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [values, setValues] = useState<Record<string, string>>({ __json: "{}" });
  const [permissions, setPermissions] = useState<string[]>([]);

  const { data: collection, isLoading } = useQuery({
    queryKey: ["collections", dbId, collId],
    queryFn: () => getCollection(dbId!, collId!),
    enabled: !!dbId && !!collId,
  });

  const create = useMutation({
    mutationFn: () =>
      createDocument(dbId!, collId!, {
        data: buildDocumentData(collection!.attributes, values),
        permissions: permissions.length > 0 ? permissions : undefined,
      }),
    onSuccess: (doc) => {
      toast.success("Document 已创建");
      navigate(`/console/databases/${dbId}/collections/${collId}/documents/${doc.id}`);
    },
  });

  if (isLoading) return <DetailSkeleton />;
  if (!collection) {
    return <NotFound backTo={`/console/databases/${dbId}/collections/${collId}/documents`} />;
  }

  const documentsPath = `/console/databases/${dbId}/collections/${collId}/documents`;

  return (
    <FormPageWrapper
      title="新建 Document"
      description={`Collection: ${collection.name}`}
      backTo={documentsPath}
      backLabel="返回文档列表"
      loading={create.isPending}
      submitLabel="创建"
      submitDisabled={!canWrite(role)}
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate();
      }}
    >
      <DocumentFormFields
        attributes={collection.attributes}
        values={values}
        onChange={(key, value) => setValues((prev) => ({ ...prev, [key]: value }))}
      />
      <div className="pt-2 border-t">
        <PermissionEditor permissions={permissions} onChange={setPermissions} />
      </div>
    </FormPageWrapper>
  );
}

export function DocumentDetailPage() {
  const { dbId, collId, docId } = useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { role } = useAdminRole();
  const [values, setValues] = useState<Record<string, string>>({});
  const [increments, setIncrements] = useState<Record<string, string>>({});
  const [initialized, setInitialized] = useState(false);
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

  useEffect(() => {
    if (!document || initialized) return;
    setValues(documentToValues(collection?.attributes ?? [], document));
    setInitialized(true);
  }, [collection, document, initialized]);

  const save = useMutation({
    mutationFn: () => {
      if (!document?.version || document.version <= 0) {
        throw new Error("文档缺少 OCC 版本信息，请刷新后重试");
      }
      const increment: Record<string, number> = {};
      for (const attr of collection?.attributes ?? []) {
        const delta = increments[attr.key]?.trim();
        if (!delta) continue;
        const n = Number(delta);
        if (!Number.isInteger(n)) {
          throw new Error(`增量必须是整数: ${attr.key}`);
        }
        if (n !== 0) increment[attr.key] = n;
      }
      return updateDocument(dbId!, collId!, docId!, {
        data: buildDocumentData(collection!.attributes, values),
        increment: Object.keys(increment).length > 0 ? increment : undefined,
        // OCC：data 与 increment 同一次 PATCH 使用同一个版本。
        version: document.version,
      });
    },
    onSuccess: (doc) => {
      toast.success("Document 已更新");
      setIncrements({});
      // 用响应文档重建表单，避免与（可能被自增/服务端归一化修改的）服务端状态失同步。
      setValues(documentToValues(collection?.attributes ?? [], doc));
      queryClient.invalidateQueries({ queryKey: ["documents", dbId, collId] });
      queryClient.invalidateQueries({ queryKey: ["documents", dbId, collId, docId] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "保存失败"),
  });

  const remove = useMutation({
    mutationFn: () => {
      if (!document?.version || document.version <= 0) {
        throw new Error("文档缺少 OCC 版本信息，请刷新后重试");
      }
      return deleteDocument(dbId!, collId!, docId!, document.version);
    },
    onSuccess: () => {
      toast.success("Document 已删除");
      navigate(`/console/databases/${dbId}/collections/${collId}/documents`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : "删除失败"),
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
        collection.is_system || !writeable ? null : (
          <DeleteButton onConfirm={() => remove.mutate()} loading={remove.isPending} />
        )
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: document.id, mono: true },
          { label: "创建时间", value: new Date(document.created_at).toLocaleString() },
          { label: "更新时间", value: new Date(document.updated_at).toLocaleString() },
        ]}
      />
      <Card className="mt-6">
        <CardContent className="pt-6">
          <form
            onSubmit={(e) => {
              e.preventDefault();
              save.mutate();
            }}
            className="space-y-4 max-w-lg"
          >
            <DocumentFormFields
              attributes={collection.attributes}
              values={values}
              onChange={(key, value) => setValues((prev) => ({ ...prev, [key]: value }))}
            />
            {collection.attributes.some((a) => a.type === "integer" || a.type === "float") && (
              <div className="rounded-lg border p-4 space-y-3">
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
                      </div>
                    ))}
                </div>
              </div>
            )}
            <Button type="submit" disabled={!writeable || save.isPending}>
              {save.isPending ? "保存中..." : "保存"}
            </Button>
          </form>
        </CardContent>
      </Card>
    </DetailPageWrapper>
  );
}
