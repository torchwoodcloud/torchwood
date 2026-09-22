import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";
import { Plus, Download, UploadCloud, Link2, Copy } from "lucide-react";
import {
  listBuckets,
  getBucket,
  createBucket,
  updateBucket,
  deleteBucket,
  listFiles,
  getFile,
  uploadFile,
  updateFile,
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
import { ChunkedUploader } from "@/routes/storage/chunked-uploader";
import { useAuth } from "@/hooks/useAuth";
import { useAdminRole, canWrite } from "@/hooks/useAdminRole";
import { useUserTimezone } from "@/hooks/useTimezone";
import { formatDateTime } from "@/lib/datetime";
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
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

// 工厂：时区来自管理员偏好（hook 无法在模块级使用），由列表页传入。
const bucketColumns = (tz: string): ColumnDef<Bucket>[] => [
  {
    key: "id",
    header: "ID",
    className: "font-mono text-xs max-w-[140px] truncate",
    cell: (b) => b.id,
  },
  {
    key: "name",
    header: "名称",
    cell: (b) => (
      <span className="inline-flex items-center gap-2">
        {b.name}
        {b.public && <Badge variant="secondary">公开</Badge>}
      </span>
    ),
  },
  {
    key: "created",
    header: "创建时间",
    cell: (b) => formatDateTime(b.created_at, tz),
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
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const tz = useUserTimezone();
  const queryClient = useQueryClient();
  const [bulkDeleting, setBulkDeleting] = useState(false);
  const writeable = canWrite(role);

  const paging = useServerPaging();
  const { data, isLoading } = useQuery({
    queryKey: ["buckets", projectId, paging.pageSize, paging.pageToken],
    queryFn: () => listBuckets({ pageSize: paging.pageSize, pageToken: paging.pageToken }),
    enabled: !!projectId,
    placeholderData: (prev) => prev,
  });
  const buckets = data?.rows ?? [];

  const remove = useMutation({
    mutationFn: (id: string) => deleteBucket(id),
    onSuccess: () => {
      toast.success("Bucket 已删除");
      queryClient.invalidateQueries({ queryKey: ["buckets", projectId] });
    },
  });

  const getSearchText = useCallback((b: Bucket) => `${b.id} ${b.name}`, []);

  const handleBulkDelete = async (selected: Bucket[], clear: () => void) => {
    setBulkDeleting(true);
    try {
      // 单条失败由页面汇总展示，跳过全局 toast 避免刷屏（R11-P2-8）。
      const results = await Promise.allSettled(
        selected.map((b) => deleteBucket(b.id, { __skipToast: true }))
      );
      const failed = results.filter((r) => r.status === "rejected").length;
      const succeeded = results.length - failed;
      if (failed > 0) {
        toast.error(`删除完成：成功 ${succeeded} 个，失败 ${failed} 个`);
      } else {
        toast.success(`已删除 ${selected.length} 个 Bucket`);
      }
      queryClient.invalidateQueries({ queryKey: ["buckets", projectId] });
      clear();
    } finally {
      setBulkDeleting(false);
    }
  };

  return (
    <ResourceListPage
      title="Storage"
      description="管理存储 Bucket"
      searchPlaceholder="当前页内搜索 Bucket 名称或 ID..."
      isLoading={isLoading}
      items={buckets}
      columns={bucketColumns(tz)}
      getSearchText={getSearchText}
      serverPaging={{
        page: paging.page,
        pageSize: paging.pageSize,
        hasPrev: paging.hasPrev,
        hasNext: !!data?.nextPageToken,
        onPrev: paging.goPrev,
        onNext: () => paging.goNext(data?.nextPageToken),
        onPageSizeChange: paging.setPageSize,
      }}
      detailPath={(b) => `/console/storage/${b.id}`}
      toolbarActions={
        writeable ? (
          <Button asChild>
            <Link to="/console/storage/new">
              <Plus className="h-4 w-4 mr-2" />
              新建 Bucket
            </Link>
          </Button>
        ) : undefined
      }
      selectionActions={
        writeable
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
        writeable
          ? (b) => (
              <RowDeleteButton onConfirm={() => remove.mutate(b.id)} loading={remove.isPending} />
            )
          : undefined
      }
      emptyTitle="暂无 Bucket"
      emptyDescription="创建 Bucket 以上传文件"
      emptyAction={
        writeable ? (
          <Button asChild>
            <Link to="/console/storage/new">新建 Bucket</Link>
          </Button>
        ) : undefined
      }
    />
  );
}

export function BucketNewPage() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { projectId } = useAuth();
  const { role } = useAdminRole();
  const [name, setName] = useState("");
  const [publicBucket, setPublicBucket] = useState(false);

  const mutation = useMutation({
    mutationFn: createBucket,
    onSuccess: (bucket) => {
      toast.success("Bucket 创建成功");
      queryClient.invalidateQueries({ queryKey: ["buckets", projectId] });
      navigate(`/console/storage/${bucket.id}`);
    },
  });

  return (
    <FormPageWrapper
      title="新建 Bucket"
      backTo="/console/storage"
      submitLabel="创建"
      onSubmit={(e) => {
        e.preventDefault();
        mutation.mutate({ name, public: publicBucket });
      }}
      loading={mutation.isPending}
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
  const { bucketId } = useParams<{ bucketId: string }>();
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

  const updateBucketMutation = useMutation({
    mutationFn: (input: { name?: string; public?: boolean }) =>
      updateBucket(bucketId!, input),
    onSuccess: () => {
      toast.success("Bucket 设置已更新");
      queryClient.invalidateQueries({ queryKey: ["buckets", bucketId] });
      queryClient.invalidateQueries({ queryKey: ["buckets", projectId] });
      queryClient.invalidateQueries({ queryKey: ["storage-usage", projectId] });
    },
  });

  const removeBucket = useMutation({
    mutationFn: (id: string) => deleteBucket(id),
    onSuccess: () => {
      toast.success("Bucket 已删除");
      queryClient.invalidateQueries({ queryKey: ["buckets", projectId] });
      navigate("/console/storage");
    },
  });

  const uploadMutation = useMutation({
    mutationFn: (file: File) => uploadFile(bucketId!, file),
    onSuccess: () => {
      toast.success("文件上传成功");
      queryClient.invalidateQueries({ queryKey: ["files", bucketId] });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: (fileId: string) => deleteFile(bucketId!, fileId),
    onSuccess: () => {
      toast.success("文件已删除");
      queryClient.invalidateQueries({ queryKey: ["files", bucketId] });
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
      queryClient.invalidateQueries({ queryKey: ["files", bucketId] });
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
              onConfirm={() => removeBucket.mutate(bucket.id)}
              loading={removeBucket.isPending}
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
              disabled={!writeable || updateBucketMutation.isPending}
              onChange={(e) =>
                updateBucketMutation.mutate({ public: e.target.checked })
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
              queryClient.invalidateQueries({ queryKey: ["files", bucketId] });
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
                  uploadMutation.mutate(file);
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
                onConfirm={() => deleteMutation.mutate(f.id)}
                loading={deleteMutation.isPending}
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
  const { bucketId, fileId } = useParams<{ bucketId: string; fileId: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
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

  const remove = useMutation({
    mutationFn: () => deleteFile(bucketId!, fileId!),
    onSuccess: () => {
      toast.success("文件已删除");
      queryClient.invalidateQueries({ queryKey: ["files", bucketId] });
      navigate(`/console/storage/${bucketId}`);
    },
  });

  const rename = useMutation({
    mutationFn: (name: string) => updateFile(bucketId!, fileId!, { name }),
    onSuccess: () => {
      toast.success("文件名已更新");
      queryClient.invalidateQueries({ queryKey: ["files", bucketId, fileId] });
      queryClient.invalidateQueries({ queryKey: ["files", bucketId] });
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
            <DeleteButton onConfirm={() => remove.mutate()} loading={remove.isPending} />
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
            disabled={!writeable || !nameDirty || !newName.trim() || rename.isPending}
            onClick={() => rename.mutate(newName.trim())}
          >
            {rename.isPending ? "保存中..." : "保存"}
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
