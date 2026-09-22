import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface Bucket {
  id: string;
  name: string;
  permissions: string[];
  public?: boolean;
  created_at: string;
  updated_at: string;
}

export interface FileItem {
  id: string;
  bucket_id: string;
  name: string;
  mime_type: string;
  size: number;
  metadata?: Record<string, string>;
  created_at: string;
  updated_at: string;
}

export interface StorageUsage {
  buckets: number;
  files: number;
  total_size: number;
}

function normalizeBucket(bucket: Bucket): Bucket {
  return {
    ...bucket,
    public: bucket.public ?? false,
    permissions: bucket.permissions ?? [],
  };
}

// 桶列表（SQL 分页，服务端 clamp 默认 25 / max 100）；对接服务端分页
//（契约说明见 pagination.ts），pageSize 必传。
export async function listBuckets(params: ListParams): Promise<Page<Bucket>> {
  const res = await api.get<{ buckets: Bucket[] } & ListMeta>("/server/storage/buckets", {
    params: pageQuery(params),
  });
  return {
    rows: (res.data.buckets ?? []).map(normalizeBucket),
    nextPageToken: res.data.meta?.next_page_token,
  };
}

export async function getBucket(id: string): Promise<Bucket> {
  const res = await api.get<Bucket>(`/server/storage/buckets/${id}`);
  return normalizeBucket(res.data);
}

export async function createBucket(input: {
  name: string;
  public?: boolean;
}): Promise<Bucket> {
  const res = await api.post<Bucket>("/server/storage/buckets", input);
  return normalizeBucket(res.data);
}

export async function deleteBucket(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/storage/buckets/${id}`, config);
}

export async function updateBucket(
  id: string,
  input: { name?: string; public?: boolean }
): Promise<Bucket> {
  const res = await api.patch<Bucket>(`/server/storage/buckets/${id}`, input);
  return normalizeBucket(res.data);
}

// 文件列表（SQL 分页，服务端 clamp 默认 25 / max 100）。
export async function listFiles(bucketId: string, params: ListParams): Promise<Page<FileItem>> {
  const res = await api.get<{ files: FileItem[] } & ListMeta>(
    `/server/storage/buckets/${bucketId}/files`,
    { params: pageQuery(params) }
  );
  return { rows: res.data.files ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getFile(bucketId: string, fileId: string): Promise<FileItem> {
  const res = await api.get<FileItem>(
    `/server/storage/buckets/${bucketId}/files/${fileId}`
  );
  return res.data;
}

export async function uploadFile(
  bucketId: string,
  file: File
): Promise<FileItem> {
  const form = new FormData();
  form.append("file", file);
  const res = await api.post<FileItem>(
    `/storage/buckets/${bucketId}/files`,
    form,
    {
      headers: { "Content-Type": "multipart/form-data" },
    }
  );
  return res.data;
}

// 分片常量：与后端 internal/domain/storage 的 DefaultChunkSize/MaxComposePartCount
// 一致，仅用于前端预检（shouldChunk/isTooLarge）；实际切片大小一律以服务端
// 会话返回的 chunk_size 为准。
export const CHUNK_SIZE = 16 * 1024 * 1024;
export const MAX_UPLOAD_SIZE = 10000 * CHUNK_SIZE;

// shouldChunk 判断文件是否走分片上传（>16MiB）。
export function shouldChunk(size: number): boolean {
  return size > CHUNK_SIZE;
}

// isTooLarge 判断文件是否超过分片上传上限（156.25GB）。
export function isTooLarge(size: number): boolean {
  return size > MAX_UPLOAD_SIZE;
}

export interface UploadSession {
  upload_id: string;
  file_id: string;
  chunk_size: number;
  part_count: number;
  expires_at: string;
}

export interface UploadProgress {
  upload_id: string;
  part_count: number;
  received: number[];
  chunk_size: number;
}

export async function createUploadSession(
  bucketId: string,
  input: {
    name: string;
    mime_type: string;
    size: number;
    metadata?: Record<string, string>;
  }
): Promise<UploadSession> {
  const res = await api.post<UploadSession>(
    `/storage/buckets/${bucketId}/uploads`,
    input
  );
  return res.data;
}

export async function getUploadSession(
  bucketId: string,
  uploadId: string
): Promise<UploadProgress> {
  const res = await api.get<UploadProgress>(
    `/storage/buckets/${bucketId}/uploads/${uploadId}`
  );
  return res.data;
}

export async function uploadChunk(
  bucketId: string,
  uploadId: string,
  partNumber: number,
  blob: Blob,
  signal?: AbortSignal
): Promise<{ part_number: number; received_count: number }> {
  const form = new FormData();
  form.append("chunk", blob);
  const res = await api.post(
    `/storage/buckets/${bucketId}/uploads/${uploadId}/chunks/${partNumber}`,
    form,
    { headers: { "Content-Type": "multipart/form-data" }, signal }
  );
  return res.data;
}

export async function completeUpload(
  bucketId: string,
  uploadId: string,
  signal?: AbortSignal
): Promise<FileItem> {
  const res = await api.post<FileItem>(
    `/storage/buckets/${bucketId}/uploads/${uploadId}/complete`,
    {},
    { signal }
  );
  return res.data;
}

export async function abortUpload(
  bucketId: string,
  uploadId: string
): Promise<void> {
  await api.delete(`/storage/buckets/${bucketId}/uploads/${uploadId}`);
}

function filenameFromDisposition(header: string | undefined): string | undefined {
  if (!header) return undefined;
  const encoded = header.match(/filename\*=UTF-8''([^;\n]+)/i);
  if (encoded?.[1]) {
    try {
      return decodeURIComponent(encoded[1]);
    } catch {
      // fall through to ASCII filename
    }
  }
  const ascii = header.match(/filename="([^"]+)"/);
  return ascii?.[1];
}

export async function downloadFile(
  bucketId: string,
  fileId: string,
  fallbackName?: string
): Promise<void> {
  const res = await api.get(
    `/storage/buckets/${bucketId}/files/${fileId}/download`,
    { responseType: "blob" }
  );
  const blob = res.data as Blob;
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download =
    fallbackName ??
    filenameFromDisposition(res.headers["content-disposition"]) ??
    "download";
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}

export async function deleteFile(
  bucketId: string,
  fileId: string,
  config?: ApiRequestConfig
): Promise<void> {
  await api.delete(`/server/storage/buckets/${bucketId}/files/${fileId}`, config);
}

export async function updateFile(
  bucketId: string,
  fileId: string,
  input: {
    name?: string;
    mime_type?: string;
    metadata?: Record<string, string>;
  }
): Promise<FileItem> {
  const res = await api.patch<FileItem>(
    `/server/storage/buckets/${bucketId}/files/${fileId}`,
    input
  );
  return res.data;
}

export async function createFileToken(
  bucketId: string,
  fileId: string,
  expiresIn?: number,
  config?: ApiRequestConfig
): Promise<{ token: string; expires_at: string }> {
  const res = await api.post<{ token: string; expires_at: string }>(
    `/server/storage/buckets/${bucketId}/files/${fileId}/tokens`,
    { expires_in: expiresIn },
    config
  );
  return res.data;
}

export async function getStorageUsage(): Promise<StorageUsage> {
  const res = await api.get<StorageUsage>("/server/storage/usage");
  return res.data;
}

// fileViewUrl 构造内联查看 URL（浏览器可直接打开）。
export function fileViewUrl(bucketId: string, fileId: string): string {
  return `/v1/storage/buckets/${bucketId}/files/${fileId}/view`;
}

// previewFile 以 blob 拉取缩略图（与 downloadFile 同路：axios 自动带
// X-Torchwood-Project 头与会话 cookie，避免 <img src> 直连带不上项目头导致
// 401/403；Round3 H5-1）。
export async function previewFile(bucketId: string, fileId: string): Promise<Blob> {
  const res = await api.get(
    `/storage/buckets/${bucketId}/files/${fileId}/preview?width=200`,
    { responseType: "blob" }
  );
  return res.data as Blob;
}
