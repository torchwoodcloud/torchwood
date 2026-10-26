import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { invalidateAfterCommand, useCollection, useCommand } from "@lynx-go/ui-kit";
import { toast } from "sonner";
import type { LegacyColumnDef } from "@tanstack/react-table/legacy";
import { Plus, Download, UploadCloud, Link2, Copy } from "lucide-react";
import {
  getBucket,
  deleteBucket,
  listFiles,
  getFile,
  createFileToken,
  getStorageUsage,
  deleteFile,
  downloadFile,
  previewFile,
  fileViewUrl,
  shouldChunk,
  isTooLarge,
  type Bucket,
  type FileItem,
} from "@/api/storage";
import {
  bucketsResource,
  bucketsSearch,
  createBucketCommand,
  updateBucketCommand,
  removeBucketCommand,
  uploadFileDescriptor,
  removeFileDescriptor,
  renameFileDescriptor,
} from "@/kit/storage";
import { describeApiError } from "@/kit/errors";
import { ChunkedUploader } from "@/routes/storage/chunked-uploader";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
import { PageHeader } from "@/components/PageHeader";
import { ResourceListPage } from "@/components/list/ResourceListPage";
import { useServerPaging } from "@/hooks/useServerPaging";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Checkbox } from "@/components/ui/checkbox";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
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
import { DataTable } from "@/components/kit/query/data-table";
import { KeysetPagination } from "@/components/kit/query/keyset-pagination";
import { PAGE_SIZE_OPTIONS, PAGINATION_ZH } from "@/components/kit/query/pagination-zh";
import { ListPage as KitListPage } from "@/components/kit/query/list-page";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

// 工厂：时区来自管理员偏好（hook 无法在模块级使用），由列表页传入。
const bucketColumns = (tz: string): LegacyColumnDef<Bucket, unknown>[] => [
  {
    id: "id",
    header: "ID",
    cell: ({ row }) => (
      <span className="inline-block max-w-[140px] truncate font-mono text-xs">
        {row.original.id}
      </span>
    ),
  },
  {
    id: "name",
    header: "名称",
    cell: ({ row }) => (
      <span className="inline-flex items-center gap-2">
        {row.original.name}
        {row.original.public && <Badge variant="secondary">公开</Badge>}
      </span>
    ),
  },
  {
    id: "created",
    header: "创建时间",
    cell: ({ row }) => formatDateTime(row.original.created_at, tz),
  },
];

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + " " + sizes[i];
}

function UsageStatCard({
  label,
  value,
}: {
  label: string;
  value: string;
}) {
  return (
    <div className="rounded-lg border bg-card p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <p className="mt-2 text-lg font-semibold">{value}</p>
    </div>
  );
}

export function StorageListPage() {
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const navigate = useNavigate();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const [search, setSearch] = useState("");
  const [selectedIds, setSelectedIds] = useState<ReadonlySet<string>>(new Set());
  const writeable = canWrite(role);

  // 列表 fleet 化（§12.9-2）：游标/页大小进 URL（分页栈 = 浏览器历史）；
  // 页内当前页搜索保持本地态（旧页语义）。
  // scope/enabled 不再逐处传（§12.10-③）：资源声明了 projectId，Provider 单源 + 未就绪自动停用。
  const list = useCollection(bucketsResource, {
    from: "/console/storage",
    search: bucketsSearch,
  });

  // 行删除命令接线（§12.3）：remove 挂桶资源——kit 集合键由 useCommand 自动
  // 失效；存量 ["buckets"]/["storage-usage"] 键由 resource.legacyKeys 桥。
  // toast 单源 onSettled。
  const remove = useCommand(removeBucketCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Bucket 已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const getSearchText = useCallback(
    (b: Bucket) => `${b.id} ${b.name}`.toLowerCase(),
    []
  );
  const shown =
    search === ""
      ? list.rows
      : list.rows.filter((b) => getSearchText(b).includes(search.toLowerCase()));
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
  const selectedBuckets = list.rows.filter((b) => selectedIds.has(b.id));

  const handleBulkDelete = async () => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selectedBuckets.map((b) => deleteBucket(b.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selectedBuckets.length} 个 Bucket`);
      }
      list.refetch();
      setSelectedIds(new Set());
    } finally {
      setBulkDeleting(false);
    }
  };

  const tableColumns: LegacyColumnDef<Bucket, unknown>[] = [
    ...bucketColumns(tz),
    ...(writeable
      ? [
          {
            id: "actions",
            header: "操作",
            cell: ({ row }: { row: { original: Bucket } }) => (
              <RowDeleteButton
                onConfirm={() => remove.mutate({ id: row.original.id })}
                loading={remove.pending}
              />
            ),
          } satisfies LegacyColumnDef<Bucket, unknown>,
        ]
      : []),
  ];

  return (
    <div className="space-y-6">
      <PageHeader title="Storage" description="管理存储 Bucket" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="当前页内搜索 Bucket 名称或 ID..."
              className="h-8 w-56"
            />
            {selectedBuckets.length > 0 && writeable ? (
              <BulkDeleteButton
                count={selectedBuckets.length}
                loading={bulkDeleting}
                onConfirm={() => void handleBulkDelete()}
              />
            ) : null}
            {writeable ? (
              <Button asChild className="ml-auto">
                <Link to="/console/storage/new">
                  <Plus className="h-4 w-4 mr-2" />
                  新建 Bucket
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
        emptyTitle={search !== "" ? "没有匹配的 Bucket" : "暂无 Bucket"}
        emptyDescription={
          search !== "" ? "调整搜索词，或清空恢复当前页全部行" : "创建 Bucket 以上传文件"
        }
        emptyAction={
          writeable ? (
            <Button asChild>
              <Link to="/console/storage/new">新建 Bucket</Link>
            </Button>
          ) : undefined
        }
      >
        <div className="px-6 pb-2 pt-2">
          <DataTable
            list={shownList}
            columns={tableColumns}
            getRowId={(b) => b.id}
            onRowClick={(b) => navigate({ to: `/console/storage/${b.id}` })}
            selection={
              writeable
                ? {
                    selectedIds,
                    onToggle: toggleRow,
                    onToggleAll: toggleAll,
                  }
                : undefined
            }
          />
        </div>
      </KitListPage>
    </div>
  );
}

export function BucketNewPage() {
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const [name, setName] = useState("");
  const [publicBucket, setPublicBucket] = useState(false);

  // 创建命令接线：kit 桶集合自动失效；存量 ["buckets"] 键由 resource.legacyKeys 桥。
  const create = useCommand(createBucketCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Bucket 创建成功");
        navigate({ to: `/console/storage/${result.payload.id}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  return (
    <FormPageWrapper
      title="新建 Bucket"
      backTo="/console/storage"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ name, public: publicBucket });
      }}
      loading={create.pending}
      submitDisabled={!canWrite(role)}
    >
      <FormField id="name" label="Bucket 名称" value={name} onChange={setName} required placeholder="uploads" />
      <label className="flex items-center gap-2 text-sm">
        <Checkbox checked={publicBucket} onChange={(e) => setPublicBucket(e.target.checked)} />
        公开 Bucket（无凭证可匿名读取 read:any 文件）
      </label>
    </FormPageWrapper>
  );
}

export function BucketDetailPage() {
  const { bucketId } = useParams({ strict: false });
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const tz = useUserTimezone();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  // 分片上传中的文件（>16MiB 走 ChunkedUploader）。
  const [chunkUpload, setChunkUpload] = useState<{ file: File; key: string } | null>(null);
  const writeable = canWrite(role);

  const { data: bucket, isLoading: bucketLoading } = useQuery({
    queryKey: ["buckets", bucketId],
    queryFn: () => getBucket(bucketId!),
    enabled: !!bucketId,
  });

  const { data: usage } = useQuery({
    queryKey: ["storage-usage", projectId],
    queryFn: getStorageUsage,
    enabled: !!projectId,
  });

  const filesPaging = useServerPaging();
  const { data: filesData, isLoading: filesLoading } = useQuery({
    queryKey: ["files", bucketId, filesPaging.pageSize, filesPaging.pageToken],
    queryFn: () =>
      listFiles(bucketId!, { pageSize: filesPaging.pageSize, pageToken: filesPaging.pageToken }),
    enabled: !!bucketId,
    placeholderData: (prev) => prev,
  });
  const files = filesData?.rows ?? [];

  // 命令接线（§12.3）：桶 update/remove 挂资源（kit 集合自动失效），
  // 存量 ["buckets"]/["storage-usage"] 键由 resource.legacyKeys 桥；文件命令
  // 独立 descriptor——存量 ["files"] 键由 descriptor.legacyKeys 桥。
  // toast 单源 onSettled。
  const updateBucketHandle = useCommand(updateBucketCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Bucket 设置已更新");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const removeBucket = useCommand(removeBucketCommand, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("Bucket 已删除");
        navigate({ to: "/console/storage" });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const uploadMutation = useCommand(uploadFileDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("文件上传成功");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const deleteMutation = useCommand(removeFileDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("文件已删除");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const fileColumns: ColumnDef<FileItem>[] = [
    { key: "name", header: "文件名", cell: (f) => f.name },
    { key: "size", header: "大小", cell: (f) => formatBytes(f.size) },
    { key: "type", header: "类型", cell: (f) => f.mime_type },
    {
      key: "created",
      header: "上传时间",
      cell: (f) => formatDateTime(f.created_at, tz),
    },
  ];

  const getFileSearchText = useCallback(
    (f: FileItem) => `${f.id} ${f.name} ${f.mime_type}`,
    []
  );

  const handleBulkDeleteFiles = async (selected: FileItem[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((f) => deleteFile(bucketId!, f.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个文件`);
      }
      // 直调路径的失效（§12.10-② 命令式编排）：走 remove descriptor 的
      // legacyKeys 声明（["files"] 前缀），不手写存量键。
      invalidateAfterCommand(queryClient, removeFileDescriptor, {});
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  if (bucketLoading) return <DetailSkeleton />;
  if (!bucket) return <NotFound backTo="/console/storage" />;

  return (
    <div className="space-y-6">
      <DetailPageWrapper
        title={bucket.name}
        description="Bucket 详情与文件管理"
        backTo="/console/storage"
        actions={
          writeable ? (
            <DeleteButton
              onConfirm={() => removeBucket.mutate({ id: bucket.id })}
              loading={removeBucket.pending}
            />
          ) : undefined
        }
      >
        <DetailGrid
          items={[
            { label: "ID", value: bucket.id, mono: true },
            { label: "名称", value: bucket.name },
            { label: "创建时间", value: formatDateTime(bucket.created_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <UsageStatCard label="Bucket 数量" value={String(usage?.buckets ?? "—")} />
        <UsageStatCard label="文件数量" value={String(usage?.files ?? "—")} />
        <UsageStatCard label="总容量" value={usage ? formatBytes(usage.total_size) : "—"} />
        <Card>
          <CardHeader className="space-y-0 pb-3">
            <CardTitle className="text-sm">公开访问</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center gap-2">
            <Checkbox
              checked={bucket.public ?? false}
              disabled={!writeable || updateBucketHandle.pending}
              onChange={(e) =>
                updateBucketHandle.mutate({ id: bucketId!, public: e.target.checked })
              }
            />
            <span className="text-sm text-muted-foreground">
              允许无凭证匿名读取（read:any 文件）
            </span>
          </CardContent>
        </Card>
      </div>

      {chunkUpload && (
        <div className="rounded-lg border bg-card p-4">
          <ChunkedUploader
            key={chunkUpload.key}
            bucketId={bucketId!}
            file={chunkUpload.file}
            onSuccess={() => {
              toast.success("文件上传成功");
              // 直调路径的失效（§12.10-② 命令式编排）：upload descriptor 的
              // legacyKeys 声明（["files"] 前缀）。
              invalidateAfterCommand(queryClient, uploadFileDescriptor, {});
              setChunkUpload(null);
            }}
            onError={() => {
              // 错误提示由 api 拦截器统一 toast，这里只做状态重置。
              setChunkUpload(null);
            }}
          />
        </div>
      )}

      <ResourceListPage
        title=""
        cardTitle="文件列表"
        searchPlaceholder="当前页内搜索文件名..."
        isLoading={filesLoading}
        items={files}
        columns={fileColumns}
        getSearchText={getFileSearchText}
        serverPaging={{
          page: filesPaging.page,
          pageSize: filesPaging.pageSize,
          hasPrev: filesPaging.hasPrev,
          hasNext: !!filesData?.nextPageToken,
          onPrev: filesPaging.goPrev,
          onNext: () => filesPaging.goNext(filesData?.nextPageToken),
          onPageSizeChange: filesPaging.setPageSize,
        }}
        detailPath={(f) => `/console/storage/${bucketId}/files/${f.id}`}
        toolbarActions={
          writeable ? (
            <div className="flex items-center gap-2">
              <UploadCloud className="h-4 w-4 text-muted-foreground" />
              <Input
                type="file"
                className="max-w-xs"
                onChange={(e) => {
                  const file = e.target.files?.[0];
                  e.target.value = "";
                  if (!file) return;
                  if (isTooLarge(file.size)) {
                    toast.error("文件超过 156.25GB 分片上传上限");
                    return;
                  }
                  if (shouldChunk(file.size)) {
                    setChunkUpload({ file, key: `${file.name}:${file.size}:${Date.now()}` });
                    return;
                  }
                  uploadMutation.mutate({ bucketId: bucketId!, file });
                }}
              />
            </div>
          ) : undefined
        }
        selectionActions={
          writeable
            ? (selected, clear) => (
                <BulkDeleteButton
                  count={selected.length}
                  loading={bulkDeleting}
                  onConfirm={() => handleBulkDeleteFiles(selected, clear)}
                />
              )
            : undefined
        }
        rowActions={(f) => (
          <>
            <Button
              variant="ghost"
              size="icon"
              title="下载"
              onClick={() => void downloadFile(bucketId!, f.id, f.name)}
            >
              <Download className="h-4 w-4" />
            </Button>
            {writeable && (
              <RowDeleteButton
                onConfirm={() => deleteMutation.mutate({ bucketId: bucketId!, fileId: f.id })}
                loading={deleteMutation.pending}
              />
            )}
          </>
        )}
        emptyTitle="暂无文件"
        emptyDescription="上传文件到此 Bucket"
      />
    </div>
  );
}

export function FileDetailPage() {
  const { bucketId, fileId } = useParams({ strict: false });
  const navigate = useNavigate();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const [shareOpen, setShareOpen] = useState(false);
  const [shareUrl, setShareUrl] = useState("");
  const [shareExpires, setShareExpires] = useState("");
  const [shareLoading, setShareLoading] = useState(false);
  const writeable = canWrite(role);

  const { data: file, isLoading } = useQuery({
    queryKey: ["files", bucketId, fileId],
    queryFn: () => getFile(bucketId!, fileId!),
    enabled: !!bucketId && !!fileId,
  });

  // Round3 H5-1：预览经 axios blob 拉取（自动带 X-Torchwood-Project 头），
  // 再 createObjectURL 渲染；卸载时 revokeObjectURL 防泄漏。
  const [previewUrl, setPreviewUrl] = useState<string | null>(null);
  const [previewFailed, setPreviewFailed] = useState(false);
  useEffect(() => {
    let objectUrl: string | null = null;
    let cancelled = false;
    setPreviewUrl(null);
    setPreviewFailed(false);
    if (!bucketId || !fileId) return;
    previewFile(bucketId, fileId)
      .then((blob) => {
        if (cancelled) return;
        objectUrl = URL.createObjectURL(blob);
        setPreviewUrl(objectUrl);
      })
      .catch(() => {
        if (!cancelled) setPreviewFailed(true);
      });
    return () => {
      cancelled = true;
      if (objectUrl) URL.revokeObjectURL(objectUrl);
    };
  }, [bucketId, fileId]);

  // 文件命令接线（独立 descriptor）：存量 ["files"] 键由 descriptor.legacyKeys
  // 桥（前缀匹配覆盖文件列表/详情键）；toast 单源 onSettled（§12.3）。
  const remove = useCommand(removeFileDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("文件已删除");
        navigate({ to: `/console/storage/${bucketId}` });
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const rename = useCommand(renameFileDescriptor, {
    onSettled: (result, failure) => {
      if (result?.status === "ok") {
        toast.success("文件名已更新");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const [newName, setNewName] = useState("");
  const [nameDirty, setNameDirty] = useState(false);

  const generateShare = async () => {
    if (!bucketId || !fileId) return;
    setShareLoading(true);
    try {
      // 失败由页面 toast 统一展示，跳过全局 toast 避免双显（R11-P1-4）。
      const { token, expires_at } = await createFileToken(
        bucketId,
        fileId,
        undefined,
        { __skipToast: true }
      );
      setShareUrl(`${fileViewUrl(bucketId, fileId)}?token=${encodeURIComponent(token)}`);
      setShareExpires(formatDateTime(expires_at, tz));
      setShareOpen(true);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : "生成分享链接失败");
    } finally {
      setShareLoading(false);
    }
  };

  const copyShare = async () => {
    try {
      await navigator.clipboard.writeText(shareUrl);
      toast.success("链接已复制");
    } catch {
      toast.error("复制失败，请手动复制");
    }
  };

  if (isLoading) return <DetailSkeleton />;
  if (!file) return <NotFound backTo={`/console/storage/${bucketId}`} />;

  const isImage = file.mime_type.startsWith("image/");

  return (
    <DetailPageWrapper
      title={file.name}
      description="文件详情"
      backTo={`/console/storage/${bucketId}`}
      backLabel="返回 Bucket"
      actions={
        <div className="flex gap-2">
          {writeable && (
            <Button variant="outline" size="sm" onClick={generateShare} disabled={shareLoading}>
              <Link2 className="h-4 w-4 mr-2" />
              分享链接
            </Button>
          )}
          <Button
            variant="outline"
            size="sm"
            onClick={() => void downloadFile(bucketId!, file.id, file.name)}
          >
            <Download className="h-4 w-4 mr-2" />
            下载
          </Button>
          {writeable && (
            <DeleteButton
              onConfirm={() => remove.mutate({ bucketId: bucketId!, fileId: fileId! })}
              loading={remove.pending}
            />
          )}
        </div>
      }
    >
      <DetailGrid
        items={[
          { label: "ID", value: file.id, mono: true },
          { label: "文件名", value: file.name },
          { label: "大小", value: formatBytes(file.size) },
          { label: "MIME 类型", value: file.mime_type },
          { label: "Bucket ID", value: file.bucket_id, mono: true },
          { label: "创建时间", value: formatDateTime(file.created_at, tz) },
          { label: "更新时间", value: formatDateTime(file.updated_at, tz) },
        ]}
      />

      {isImage && (
        <Card className="mt-6">
          <CardHeader>
            <CardTitle className="text-base">预览</CardTitle>
          </CardHeader>
          <CardContent>
            {previewUrl ? (
              <img
                src={previewUrl}
                alt={file.name}
                className="max-h-96 rounded-lg border object-contain"
              />
            ) : (
              <div className="flex h-40 items-center justify-center rounded-lg border text-sm text-muted-foreground">
                {previewFailed ? "预览加载失败" : "预览加载中..."}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-base">重命名</CardTitle>
        </CardHeader>
        <CardContent className="flex items-end gap-2">
          <div className="flex-1 space-y-2">
            <FormField
              id="rename"
              label="文件名"
              value={newName}
              onChange={(v) => {
                setNewName(v);
                setNameDirty(true);
              }}
              placeholder={file.name}
            />
          </div>
          <Button
            disabled={!writeable || !nameDirty || !newName.trim() || rename.pending}
            onClick={() =>
              rename.mutate({ bucketId: bucketId!, fileId: fileId!, name: newName.trim() })
            }
          >
            {rename.pending ? "保存中..." : "保存"}
          </Button>
        </CardContent>
      </Card>

      <Dialog open={shareOpen} onOpenChange={setShareOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>分享链接</DialogTitle>
            <DialogDescription>
              短期匿名访问链接，过期时间：{shareExpires}
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-center gap-2">
            <Input readOnly value={shareUrl} className="font-mono text-xs" />
            <Button size="icon" variant="outline" onClick={copyShare} title="复制">
              <Copy className="h-4 w-4" />
            </Button>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShareOpen(false)}>
              关闭
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </DetailPageWrapper>
  );
}
