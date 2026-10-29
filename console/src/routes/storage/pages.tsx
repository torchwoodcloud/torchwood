import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { invalidateAfterCommand, useCollection, useCommand } from "@lynx-go/uikit-core";
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
import { PAGE_SIZE_OPTIONS } from "@/api/pagination";
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
    header: "Name",
    cell: ({ row }) => (
      <span className="inline-flex items-center gap-2">
        {row.original.name}
        {row.original.public && <Badge variant="secondary">Public</Badge>}
      </span>
    ),
  },
  {
    id: "created",
    header: "Created",
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
        toast.success("Bucket deleted");
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
        toast.error(`Deletion complete: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${selectedBuckets.length} buckets`);
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
            header: "Actions",
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
      <PageHeader title="Storage" description="Manage storage buckets" />

      <KitListPage
        list={shownList}
        toolbar={
          <div className="flex flex-wrap items-center gap-2 px-4 pb-2 pt-2">
            <Input
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              placeholder="Search buckets by name or ID on this page..."
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
              <Button size="sm" asChild className="ml-auto">
                <Link to="/console/storage/new">
                  <Plus className="h-4 w-4 mr-2" />
                  New bucket
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
        emptyTitle={search !== "" ? "No matching buckets" : "No buckets yet"}
        emptyDescription={
          search !== "" ? "Adjust your search, or clear it to restore all rows on this page" : "Create a bucket to upload files"
        }
        emptyAction={
          writeable ? (
            <Button size="sm" asChild>
              <Link to="/console/storage/new">New bucket</Link>
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
        toast.success("Bucket created");
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
      title="New bucket"
      backTo="/console/storage"
      submitLabel="Create"
      onSubmit={(e) => {
        e.preventDefault();
        create.mutate({ name, public: publicBucket });
      }}
      loading={create.pending}
      submitDisabled={!canWrite(role)}
    >
      <FormField id="name" label="Bucket name" value={name} onChange={setName} required placeholder="uploads" />
      <label className="flex items-center gap-2 text-sm">
        <Checkbox checked={publicBucket} onChange={(e) => setPublicBucket(e.target.checked)} />
        Public bucket (read:any files readable anonymously without credentials)
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
        toast.success("Bucket settings updated");
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
        toast.success("Bucket deleted");
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
        toast.success("File uploaded");
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
        toast.success("File deleted");
      } else if (result) {
        toast.error(result.message);
      } else if (failure) {
        toast.error(describeApiError(failure));
      }
    },
  });

  const fileColumns: ColumnDef<FileItem>[] = [
    { key: "name", header: "Name", cell: (f) => f.name },
    { key: "size", header: "Size", cell: (f) => formatBytes(f.size) },
    { key: "type", header: "Type", cell: (f) => f.mime_type },
    {
      key: "created",
      header: "Uploaded At",
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
        toast.error(`Deletion complete: ${succeeded} succeeded, ${failed} failed`);
      } else {
        toast.success(`Deleted ${selected.length} files`);
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
        description="Bucket details and file management"
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
            { label: "Name", value: bucket.name },
            { label: "Created", value: formatDateTime(bucket.created_at, tz) },
          ]}
        />
      </DetailPageWrapper>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <UsageStatCard label="Buckets" value={String(usage?.buckets ?? "—")} />
        <UsageStatCard label="Files" value={String(usage?.files ?? "—")} />
        <UsageStatCard label="Total size" value={usage ? formatBytes(usage.total_size) : "—"} />
        <Card>
          <CardHeader className="space-y-0 pb-3">
            <CardTitle className="text-sm">Public access</CardTitle>
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
              Allow anonymous reads without credentials (read:any files)
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
              toast.success("File uploaded");
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
        cardTitle="Files"
        searchPlaceholder="Search file names on this page..."
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
                    toast.error("File exceeds the 156.25GB chunked upload limit");
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
              title="Download"
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
        emptyTitle="No files yet"
        emptyDescription="Upload files to this bucket"
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
        toast.success("File deleted");
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
        toast.success("File name updated");
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
      toast.error(err instanceof Error ? err.message : "Failed to generate share link");
    } finally {
      setShareLoading(false);
    }
  };

  const copyShare = async () => {
    try {
      await navigator.clipboard.writeText(shareUrl);
      toast.success("Link copied");
    } catch {
      toast.error("Copy failed, please copy manually");
    }
  };

  if (isLoading) return <DetailSkeleton />;
  if (!file) return <NotFound backTo={`/console/storage/${bucketId}`} />;

  const isImage = file.mime_type.startsWith("image/");

  return (
    <DetailPageWrapper
      title={file.name}
      description="File details"
      backTo={`/console/storage/${bucketId}`}
      backLabel="Back to bucket"
      actions={
        <div className="flex gap-2">
          {writeable && (
            <Button variant="outline" size="sm" onClick={generateShare} disabled={shareLoading}>
              <Link2 className="h-4 w-4 mr-2" />
              Share link
            </Button>
          )}
          <Button
            variant="outline"
            size="sm"
            onClick={() => void downloadFile(bucketId!, file.id, file.name)}
          >
            <Download className="h-4 w-4 mr-2" />
            Download
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
          { label: "Name", value: file.name },
          { label: "Size", value: formatBytes(file.size) },
          { label: "MIME type", value: file.mime_type },
          { label: "Bucket ID", value: file.bucket_id, mono: true },
          { label: "Created", value: formatDateTime(file.created_at, tz) },
          { label: "Updated", value: formatDateTime(file.updated_at, tz) },
        ]}
      />

      {isImage && (
        <Card className="mt-6">
          <CardHeader>
            <CardTitle className="text-base">Preview</CardTitle>
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
                {previewFailed ? "Failed to load preview" : "Loading preview..."}
              </div>
            )}
          </CardContent>
        </Card>
      )}

      <Card className="mt-6">
        <CardHeader>
          <CardTitle className="text-base">Rename</CardTitle>
        </CardHeader>
        <CardContent className="flex items-end gap-2">
          <div className="flex-1 space-y-2">
            <FormField
              id="rename"
              label="Name"
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
            {rename.pending ? "Saving..." : "Save"}
          </Button>
        </CardContent>
      </Card>

      <Dialog open={shareOpen} onOpenChange={setShareOpen}>
        <DialogContent className="max-w-lg">
          <DialogHeader>
            <DialogTitle>Share link</DialogTitle>
            <DialogDescription>
              Short-term anonymous access link, expires at: {shareExpires}
            </DialogDescription>
          </DialogHeader>
          <div className="flex items-center gap-2">
            <Input readOnly value={shareUrl} className="font-mono text-xs" />
            <Button size="icon" variant="outline" onClick={copyShare} title="Copy">
              <Copy className="h-4 w-4" />
            </Button>
          </div>
          <DialogFooter>
            <Button variant="outline" onClick={() => setShareOpen(false)}>
              Close
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </DetailPageWrapper>
  );
}
