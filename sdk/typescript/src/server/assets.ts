import { listQuery, type HttpTransport } from "../http.js";
import type {
  AssetDef,
  AssetHolding,
  AssetLedgerEntry,
  AssetOpResponse,
  ListParams,
  ReconcileResponse,
} from "../types.js";

export class ServerAssetsService {
  constructor(private readonly http: HttpTransport) {}

  async createAssetDef(input: {
    code: string;
    name: string;
    class: string;
    decimals?: number;
    max_quantity?: string;
    expires_in?: string;
    tradable?: boolean;
    unique_per_owner?: boolean;
    upgradeable?: boolean;
    metadata?: Record<string, unknown>;
  }): Promise<AssetDef> {
    return this.http.request<AssetDef>("POST", "/v1/server/assets/defs", {
      auth: "apiKey",
      body: input,
    });
  }

  async listAssetDefs(params?: ListParams): Promise<AssetDef[]> {
    const res = await this.http.request<{ defs: AssetDef[] }>("GET", "/v1/server/assets/defs", {
      auth: "apiKey",
      query: listQuery(params),
    });
    return res.defs ?? [];
  }

  async getAssetDef(defId: string): Promise<AssetDef> {
    return this.http.request<AssetDef>("GET", `/v1/server/assets/defs/${defId}`, { auth: "apiKey" });
  }

  async updateAssetDef(
    defId: string,
    input: {
      name?: string;
      decimals?: number;
      max_quantity?: string;
      expires_in?: string;
      tradable?: boolean;
      unique_per_owner?: boolean;
      upgradeable?: boolean;
      metadata?: Record<string, unknown>;
      status?: string;
    }
  ): Promise<AssetDef> {
    return this.http.request<AssetDef>("PATCH", `/v1/server/assets/defs/${defId}`, {
      auth: "apiKey",
      body: { def_id: defId, ...input },
    });
  }

  async deleteAssetDef(defId: string): Promise<void> {
    await this.http.request<void>("DELETE", `/v1/server/assets/defs/${defId}`, { auth: "apiKey" });
  }

  async grant(input: {
    owner_id: string;
    def_code: string;
    quantity: string;
    idempotency_key: string;
    expires_at?: string;
    level?: number;
    metadata?: Record<string, unknown>;
    ref_type?: string;
    ref_id?: string;
  }): Promise<AssetOpResponse> {
    return this.http.request<AssetOpResponse>("POST", "/v1/server/assets:grant", {
      auth: "apiKey",
      body: input,
    });
  }

  async consume(input: {
    owner_id: string;
    def_code: string;
    quantity: string;
    idempotency_key: string;
    ref_type?: string;
    ref_id?: string;
  }): Promise<AssetOpResponse> {
    return this.http.request<AssetOpResponse>("POST", "/v1/server/assets:consume", {
      auth: "apiKey",
      body: input,
    });
  }

  async transfer(input: {
    from_owner_id: string;
    to_owner_id: string;
    def_code: string;
    quantity: string;
    idempotency_key: string;
    ref_type?: string;
    ref_id?: string;
  }): Promise<AssetOpResponse> {
    return this.http.request<AssetOpResponse>("POST", "/v1/server/assets:transfer", {
      auth: "apiKey",
      body: input,
    });
  }

  async mutate(input: {
    holding_id: string;
    idempotency_key: string;
    level?: number;
    expires_at?: string;
    metadata?: Record<string, unknown>;
    ref_type?: string;
    ref_id?: string;
  }): Promise<AssetOpResponse> {
    return this.http.request<AssetOpResponse>("POST", "/v1/server/assets:mutate", {
      auth: "apiKey",
      body: input,
    });
  }

  async expire(input: { holding_id: string; idempotency_key: string }): Promise<AssetOpResponse> {
    return this.http.request<AssetOpResponse>("POST", "/v1/server/assets:expire", {
      auth: "apiKey",
      body: input,
    });
  }

  async reconcile(): Promise<ReconcileResponse> {
    return this.http.request<ReconcileResponse>("POST", "/v1/server/assets:reconcile", {
      auth: "apiKey",
      body: {},
    });
  }

  async listUserAssets(ownerId: string, params?: ListParams): Promise<AssetHolding[]> {
    const res = await this.http.request<{ holdings: AssetHolding[] }>(
      "GET",
      `/v1/server/assets/users/${ownerId}`,
      { auth: "apiKey", query: listQuery(params) }
    );
    return res.holdings ?? [];
  }

  async listDefAssets(
    defId: string,
    params?: ListParams & { owner_id?: string }
  ): Promise<AssetHolding[]> {
    const res = await this.http.request<{ holdings: AssetHolding[] }>(
      "GET",
      `/v1/server/assets/defs/${defId}/holders`,
      {
        auth: "apiKey",
        query: { ...listQuery(params), owner_id: params?.owner_id },
      }
    );
    return res.holdings ?? [];
  }

  async listUserLedger(
    ownerId: string,
    params?: ListParams & { def_code?: string }
  ): Promise<AssetLedgerEntry[]> {
    const res = await this.http.request<{ entries: AssetLedgerEntry[] }>(
      "GET",
      `/v1/server/assets/users/${ownerId}/ledger`,
      {
        auth: "apiKey",
        query: { ...listQuery(params), def_code: params?.def_code },
      }
    );
    return res.entries ?? [];
  }
}
