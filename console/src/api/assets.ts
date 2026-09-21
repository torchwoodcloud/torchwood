import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface AssetDef {
  id: string;
  project_id?: string;
  code: string;
  name: string;
  class: string;
  decimals: number;
  max_quantity?: string;
  expires_in?: string;
  tradable?: boolean;
  unique_per_owner?: boolean;
  upgradeable?: boolean;
  metadata?: Record<string, unknown>;
  status?: string;
  created_at?: string;
  updated_at?: string;
}

export interface AssetHolding {
  id: string;
  owner_id?: string;
  def_id: string;
  def_code: string;
  class: string;
  quantity: string;
  expires_at?: string;
  level?: number;
}

export interface AssetLedgerEntry {
  id: string;
  owner_id?: string;
  def_id: string;
  def_code?: string;
  kind: string;
  delta: string;
  quantity_after: string;
  created_at?: string;
}

// 服务端 assets 列表默认 page_size=25、空页才停发 next_page_token；
// 对接服务端分页（契约说明见 pagination.ts），pageSize 必传。
export async function listAssetDefs(params: ListParams): Promise<Page<AssetDef>> {
  const res = await api.get<{ defs: AssetDef[] } & ListMeta>("/server/assets/defs", {
    params: pageQuery(params),
  });
  return { rows: res.data.defs ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getAssetDef(defId: string): Promise<AssetDef> {
  const res = await api.get<AssetDef>(`/server/assets/defs/${defId}`);
  return res.data;
}

export async function createAssetDef(input: {
  code: string;
  name: string;
  class: string;
  decimals?: number;
  max_quantity?: string;
  expires_in?: string;
  tradable?: boolean;
  unique_per_owner?: boolean;
  upgradeable?: boolean;
}): Promise<AssetDef> {
  const res = await api.post<AssetDef>("/server/assets/defs", input);
  return res.data;
}

export async function updateAssetDef(
  defId: string,
  input: { name?: string; status?: string; tradable?: boolean }
): Promise<AssetDef> {
  const res = await api.patch<AssetDef>(`/server/assets/defs/${defId}`, { def_id: defId, ...input });
  return res.data;
}

export async function deleteAssetDef(defId: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/assets/defs/${defId}`, config);
}

export async function listUserAssets(
  ownerId: string,
  params: ListParams
): Promise<Page<AssetHolding>> {
  const res = await api.get<{ holdings: AssetHolding[] } & ListMeta>(
    `/server/assets/users/${ownerId}`,
    { params: pageQuery(params) }
  );
  return { rows: res.data.holdings ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function listUserLedger(
  ownerId: string,
  params: ListParams
): Promise<Page<AssetLedgerEntry>> {
  const res = await api.get<{ entries: AssetLedgerEntry[] } & ListMeta>(
    `/server/assets/users/${ownerId}/ledger`,
    { params: pageQuery(params) }
  );
  return { rows: res.data.entries ?? [], nextPageToken: res.data.meta?.next_page_token };
}
