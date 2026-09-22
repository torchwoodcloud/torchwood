import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface APIKey {
  id: string;
  name: string;
  scopes: string[];
  enabled: boolean;
  expire_at?: string;
  created_at: string;
  updated_at: string;
}

export interface ListAPIKeysResponse {
  api_keys: APIKey[];
  meta?: ListMeta["meta"] & { total_count?: number };
}

// 服务端 key 列表（ListAPIKeysRequest）：SQL 分页（clamp 默认 50 / max 100）+
// enabled 精确过滤（proto3 optional：未传 = 全部）。对接服务端分页
//（契约说明见 pagination.ts），pageSize 必传。
export interface ListAPIKeysFilter {
  enabled?: boolean;
}

export async function listAPIKeys(
  params: ListParams & ListAPIKeysFilter
): Promise<Page<APIKey>> {
  const res = await api.get<ListAPIKeysResponse>("/server/api-keys", {
    params: {
      ...pageQuery(params),
      enabled: params.enabled,
    },
  });
  return { rows: res.data.api_keys ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getAPIKey(id: string): Promise<APIKey> {
  const res = await api.get<APIKey>(`/server/api-keys/${id}`);
  return res.data;
}

export async function createAPIKey(input: {
  name: string;
  scopes?: string[];
}): Promise<{ api_key: APIKey; secret: string }> {
  const res = await api.post<{ api_key: APIKey; secret: string }>(
    "/server/api-keys",
    input
  );
  return res.data;
}

// UpdateAPIKeyInput 与 proto3 optional 对齐：仅携带要修改的字段（未携带 =
// 不修改）；enabled: false 即禁用（禁用后立即 401）。
export interface UpdateAPIKeyInput {
  name?: string;
  scopes?: string[];
  enabled?: boolean;
  expire_at?: string;
}

export async function updateAPIKey(
  id: string,
  input: UpdateAPIKeyInput
): Promise<APIKey> {
  const res = await api.patch<APIKey>(`/server/api-keys/${id}`, input);
  return res.data;
}

export async function deleteAPIKey(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/api-keys/${id}`, config);
}
