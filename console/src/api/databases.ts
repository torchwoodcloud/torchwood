import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface Database {
  id: string;
  name: string;
  created_at: string;
  updated_at: string;
}

export interface Attribute {
  id: string;
  key: string;
  type: string;
  size?: number;
  required: boolean;
  array: boolean;
}

export interface Index {
  id: string;
  type: string;
  attributes: string[];
  orders: string[];
}

export interface Collection {
  id: string;
  database_id: string;
  name: string;
  permissions: string[];
  document_security?: boolean;
  disabled?: boolean;
  is_system: boolean;
  attributes: Attribute[];
  indexes: Index[];
  created_at: string;
  updated_at: string;
}

export interface Document {
  id: string;
  data: Record<string, unknown>;
  permissions?: string[];
  created_at: string;
  updated_at: string;
  // 用户集合 OCC 版本；int64 网关可能给 string，消费时 Number()。
  version?: number;
}

function normalizeIndex(index: Index): Index {
  return {
    ...index,
    attributes: index.attributes ?? [],
    orders: index.orders ?? [],
  };
}

// normalizeVersion 把网关返回的 version（int64 常为 string）归一化为正整数；
// 非法 / 缺失 → 0（页面侧应拦截，禁止静默把 0 当 OCC 版本发出去）。
function normalizeVersion(value: unknown): number {
  const v = Number(value);
  return Number.isFinite(v) && v > 0 ? v : 0;
}

function normalizeDocument(doc: Document): Document {
  return { ...doc, version: normalizeVersion(doc.version) };
}

function normalizeCollection(collection: Collection): Collection {
  return {
    ...collection,
    permissions: collection.permissions ?? [],
    attributes: collection.attributes ?? [],
    indexes: (collection.indexes ?? []).map(normalizeIndex),
    is_system: collection.is_system ?? false,
  };
}

// 服务端列表默认 page_size=50、上限 100，空页才停发 next_page_token；
// 对接服务端分页（契约说明见 pagination.ts），pageSize 必传。
export async function listDatabases(params: ListParams): Promise<Page<Database>> {
  const res = await api.get<{ databases: Database[] } & ListMeta>("/server/databases", {
    params: pageQuery(params),
  });
  return { rows: res.data.databases ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getDatabase(id: string): Promise<Database> {
  const res = await api.get<Database>(`/server/databases/${id}`);
  return res.data;
}

export async function createDatabase(input: {
  id: string;
  name: string;
}): Promise<Database> {
  const res = await api.post<Database>("/server/databases", input);
  return res.data;
}

export async function deleteDatabase(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/databases/${id}`, config);
}

export async function listCollections(
  databaseId: string,
  params: ListParams
): Promise<Page<Collection>> {
  const res = await api.get<{ collections: Collection[] } & ListMeta>(
    `/server/databases/${databaseId}/collections`,
    { params: pageQuery(params) }
  );
  const rows = (res.data.collections ?? []).map(normalizeCollection);
  return { rows, nextPageToken: res.data.meta?.next_page_token };
}

export async function getCollection(
  databaseId: string,
  collectionId: string
): Promise<Collection> {
  const res = await api.get<Collection>(
    `/server/databases/${databaseId}/collections/${collectionId}`
  );
  return normalizeCollection(res.data);
}

export async function createCollection(
  databaseId: string,
  input: { id: string; name: string; permissions?: string[]; document_security?: boolean }
): Promise<Collection> {
  const res = await api.post<Collection>(
    `/server/databases/${databaseId}/collections`,
    input
  );
  return normalizeCollection(res.data);
}

export async function deleteCollection(
  databaseId: string,
  collectionId: string,
  config?: ApiRequestConfig
): Promise<void> {
  await api.delete(
    `/server/databases/${databaseId}/collections/${collectionId}`,
    config
  );
}

export async function updateCollection(
  databaseId: string,
  collectionId: string,
  input: {
    name?: string;
    permissions?: string[];
    document_security?: boolean;
    disabled?: boolean;
  }
): Promise<Collection> {
  const body: Record<string, unknown> = {};
  if (input.name !== undefined) body.name = input.name;
  if (input.document_security !== undefined) body.document_security = input.document_security;
  if (input.disabled !== undefined) body.disabled = input.disabled;
  if (input.permissions !== undefined) {
    body.permissions = { values: input.permissions };
  }
  const res = await api.patch<Collection>(
    `/server/databases/${databaseId}/collections/${collectionId}`,
    body
  );
  return normalizeCollection(res.data);
}

export async function createAttribute(
  databaseId: string,
  collectionId: string,
  input: {
    key: string;
    type: string;
    size?: number;
    required?: boolean;
    array?: boolean;
  }
): Promise<Attribute> {
  const res = await api.post<Attribute>(
    `/server/databases/${databaseId}/collections/${collectionId}/attributes`,
    input
  );
  return res.data;
}

export async function createIndex(
  databaseId: string,
  collectionId: string,
  input: {
    id: string;
    type: string;
    attributes: string[];
    orders?: string[];
  }
): Promise<Index> {
  const res = await api.post<Index>(
    `/server/databases/${databaseId}/collections/${collectionId}/indexes`,
    input
  );
  return res.data;
}

export async function deleteAttribute(
  databaseId: string,
  collectionId: string,
  key: string
): Promise<void> {
  await api.delete(
    `/server/databases/${databaseId}/collections/${collectionId}/attributes/${key}`
  );
}

export async function deleteIndex(
  databaseId: string,
  collectionId: string,
  indexId: string
): Promise<void> {
  await api.delete(
    `/server/databases/${databaseId}/collections/${collectionId}/indexes/${indexId}`
  );
}

export async function listDocuments(
  databaseId: string,
  collectionId: string,
  params: ListParams
): Promise<Page<Document>> {
  // 服务端 documents 默认 page_size=50（2026-09-21 assets 同类事故），
  // 对接服务端分页，pageSize 必传。
  const res = await api.get<{ documents: Document[] } & ListMeta>(
    `/server/databases/${databaseId}/collections/${collectionId}/documents`,
    { params: pageQuery(params) }
  );
  const rows = (res.data.documents ?? []).map(normalizeDocument);
  return { rows, nextPageToken: res.data.meta?.next_page_token };
}

export async function getDocument(
  databaseId: string,
  collectionId: string,
  documentId: string
): Promise<Document> {
  const res = await api.get<Document>(
    `/server/databases/${databaseId}/collections/${collectionId}/documents/${documentId}`
  );
  return normalizeDocument(res.data);
}

export async function createDocument(
  databaseId: string,
  collectionId: string,
  input: { data: Record<string, unknown>; document_id?: string; permissions?: string[] }
): Promise<Document> {
  const res = await api.post<Document>(
    `/server/databases/${databaseId}/collections/${collectionId}/documents`,
    input
  );
  return res.data;
}

export async function updateDocument(
  databaseId: string,
  collectionId: string,
  documentId: string,
  input: {
    data?: Record<string, unknown>;
    permissions?: string[];
    increment?: Record<string, number>;
    // 用户集合 OCC 必填：来自 GetDocument/List 的 version。
    version: number;
  }
): Promise<Document> {
  const res = await api.patch<Document>(
    `/server/databases/${databaseId}/collections/${collectionId}/documents/${documentId}`,
    input
  );
  return res.data;
}

export async function deleteDocument(
  databaseId: string,
  collectionId: string,
  documentId: string,
  // 用户集合 OCC 必填：来自 GetDocument/List 的 version。
  version: number
): Promise<void> {
  await api.delete(
    `/server/databases/${databaseId}/collections/${collectionId}/documents/${documentId}`,
    { params: { version } }
  );
}

export async function bulkUpdateDocuments(
  databaseId: string,
  collectionId: string,
  input: {
    document_ids: string[];
    data: Record<string, unknown>;
    permissions?: string[];
  }
): Promise<number> {
  const res = await api.patch<{ affected: number }>(
    `/server/databases/${databaseId}/collections/${collectionId}/documents:bulkUpdate`,
    input
  );
  return res.data.affected ?? 0;
}

export async function bulkDeleteDocuments(
  databaseId: string,
  collectionId: string,
  documentIds: string[]
): Promise<number> {
  const res = await api.post<{ affected: number }>(
    `/server/databases/${databaseId}/collections/${collectionId}/documents:bulkDelete`,
    { document_ids: documentIds }
  );
  return res.data.affected ?? 0;
}
