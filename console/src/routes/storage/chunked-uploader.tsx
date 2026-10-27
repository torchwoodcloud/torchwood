import { useCallback, useEffect, useRef, useState } from "react";
import {
  createUploadSession,
  getUploadSession,
  uploadChunk,
  completeUpload,
  abortUpload,
  type UploadProgress,
  type FileItem,
} from "@/api/storage";
import { Button } from "@/components/ui/button";
import { Progress } from "@/components/ui/progress";

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + " " + sizes[i];
}

// 单分片最大尝试次数：初次 + 2 次退避重试。
const MAX_CHUNK_ATTEMPTS = 3;
const RETRY_BASE_DELAY_MS = 500;

interface ChunkedUploaderProps {
  bucketId: string;
  file: File;
  onSuccess: (file: FileItem) => void;
  onError: (message: string) => void;
}

// 续传 key 加入 lastModified（内容特征），避免同名同大小但内容不同的文件
// 复用同一 upload session 导致分片错位、文件损坏。
function uploadKey(bucketId: string, file: File): string {
  return `torchwood:upload:${bucketId}:${file.name}:${file.size}:${file.lastModified}`;
}

interface StartedUpload {
  upload_id: string;
  part_count: number;
  chunk_size: number;
  received: Set<number>;
}

/**
 * ChunkedUploader 大文件（> 16MiB）分片上传：
 * uploadId 存 localStorage（键含 bucketId+fileName+size+lastModified），页面刷新后可续传；
 * 续传前校验会话与当前文件 size 一致（服务端不返回 size，用 part_count 与
 * ceil(size/chunk_size) 比对），不一致则重建会话并清理旧 key；
 * 单分片失败自动退避重试（2 次），可手动取消（abortUpload + 清 key），
 * 组件卸载时中止在途请求（保留会话以便续传）。
 */
export function ChunkedUploader({ bucketId, file, onSuccess, onError }: ChunkedUploaderProps) {
  const [uploaded, setUploaded] = useState(0);
  const [total, setTotal] = useState(0);
  const [failed, setFailed] = useState(false);
  const [cancelled, setCancelled] = useState(false);
  const [cancelling, setCancelling] = useState(false);
  const startedRef = useRef(false);
  const cancelledRef = useRef(false);
  const mountedRef = useRef(true);
  const abortRef = useRef<AbortController | null>(null);
  const keyRef = useRef("");
  // 进度与回调走 ref，避免 run 闭包捕获过期状态/过期 props（stale closure）。
  const uploadedRef = useRef(0);
  const totalRef = useRef(0);
  const onSuccessRef = useRef(onSuccess);
  const onErrorRef = useRef(onError);
  onSuccessRef.current = onSuccess;
  onErrorRef.current = onError;

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      // 卸载时中止在途请求；不调用 abortUpload，保留会话供续传。
      abortRef.current?.abort();
    };
  }, []);

  const start = useCallback(
    async (key: string): Promise<StartedUpload> => {
      const uploadId = localStorage.getItem(key);
      if (uploadId) {
        try {
          const progress: UploadProgress = await getUploadSession(bucketId, uploadId);
          const expectedParts = Math.ceil(file.size / progress.chunk_size);
          if (progress.part_count === expectedParts) {
            totalRef.current = progress.part_count;
            setTotal(progress.part_count);
            return {
              upload_id: uploadId,
              part_count: progress.part_count,
              chunk_size: progress.chunk_size,
              received: new Set(progress.received),
            };
          }
          // size 不一致：旧会话属于其它文件（碰撞或 stale key），重建并清旧 key。
          localStorage.removeItem(key);
        } catch {
          // 会话过期/不存在：重建。
          localStorage.removeItem(key);
        }
      }
      const session = await createUploadSession(bucketId, {
        name: file.name,
        mime_type: file.type || "",
        size: file.size,
      });
      localStorage.setItem(key, session.upload_id);
      totalRef.current = session.part_count;
      setTotal(session.part_count);
      return {
        upload_id: session.upload_id,
        part_count: session.part_count,
        chunk_size: session.chunk_size,
        received: new Set(),
      };
    },
    [bucketId, file]
  );

  const run = useCallback(async () => {
    const key = uploadKey(bucketId, file);
    keyRef.current = key;
    try {
      const { upload_id: uid, part_count, chunk_size, received } = await start(key);
      const controller = new AbortController();
      abortRef.current = controller;

      let done = 0;
      for (let part = 1; part <= part_count; part++) {
        if (controller.signal.aborted) return;
        if (received.has(part)) {
          done++;
          uploadedRef.current = done;
          setUploaded(done);
          continue;
        }
        const blob = file.slice((part - 1) * chunk_size, Math.min(part * chunk_size, file.size));
        let attempts = 0;
        for (;;) {
          if (controller.signal.aborted) return;
          try {
            await uploadChunk(bucketId, uid, part, blob, controller.signal);
            break;
          } catch (err) {
            if (controller.signal.aborted) return;
            attempts++;
            if (attempts >= MAX_CHUNK_ATTEMPTS) throw err;
            await new Promise((resolve) =>
              setTimeout(resolve, RETRY_BASE_DELAY_MS * 2 ** (attempts - 1))
            );
          }
        }
        done++;
        uploadedRef.current = done;
        setUploaded(done);
      }

      if (controller.signal.aborted) return;
      const completed = await completeUpload(bucketId, uid, controller.signal);
      localStorage.removeItem(key);
      onSuccessRef.current(completed);
    } catch (err) {
      if (mountedRef.current && !cancelledRef.current) {
        setFailed(true);
        onErrorRef.current(
          err instanceof Error && err.message
            ? err.message
            : `Upload interrupted: ${uploadedRef.current}/${totalRef.current || "?"} chunks uploaded, reselect the file to resume`
        );
      }
    }
  }, [bucketId, file, start]);

  useEffect(() => {
    if (startedRef.current) return;
    startedRef.current = true;
    void run();
  }, [run]);

  const handleCancel = async () => {
    if (cancelledRef.current) return;
    cancelledRef.current = true;
    setCancelling(true);
    abortRef.current?.abort();
    const key = keyRef.current;
    const uploadId = localStorage.getItem(key);
    if (uploadId) {
      try {
        await abortUpload(bucketId, uploadId);
      } catch {
        // 会话可能已过期/不存在：忽略，本地 key 仍清理。
      }
    }
    localStorage.removeItem(key);
    setCancelled(true);
    setCancelling(false);
    onError("Upload cancelled");
  };

  if (failed || cancelled) {
    return null;
  }

  const percent = total > 0 ? Math.round((uploaded / total) * 100) : 0;
  return (
    <div className="w-full max-w-sm space-y-1.5">
      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span className="truncate pr-2">{file.name}</span>
        <span>
          {formatBytes(file.size)} · {percent}%
        </span>
      </div>
      <Progress value={percent} />
      <p className="text-xs text-muted-foreground">
        Uploading chunks ({uploaded}/{total})…
      </p>
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={cancelling}
        onClick={() => void handleCancel()}
      >
        {cancelling ? "Cancelling..." : "Cancel upload"}
      </Button>
    </div>
  );
}
