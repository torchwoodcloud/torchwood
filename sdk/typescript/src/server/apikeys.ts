import type { HttpTransport } from "../http.js";
import type { APIKey, WhoAmIResponse } from "../types.js";

export class APIKeysService {
  constructor(private readonly http: HttpTransport) {}

  async list(): Promise<APIKey[]> {
    const res = await this.http.request<{ api_keys: APIKey[] }>("GET", "/v1/server/api-keys", {
      auth: "apiKey",
    });
    return res.api_keys ?? [];
  }

  async get(id: string): Promise<APIKey> {
    return this.http.request<APIKey>("GET", `/v1/server/api-keys/${id}`, { auth: "apiKey" });
  }

  async create(input: {
    name: string;
    scopes?: string[];
  }): Promise<{ api_key: APIKey; secret: string }> {
    return this.http.request<{ api_key: APIKey; secret: string }>("POST", "/v1/server/api-keys", {
      auth: "apiKey",
      body: input,
    });
  }

  async delete(id: string): Promise<void> {
    await this.http.request<void>("DELETE", `/v1/server/api-keys/${id}`, { auth: "apiKey" });
  }

  async update(
    id: string,
    input: {
      name?: string;
      scopes?: string[];
      enabled?: boolean;
      expire_at?: string;
    }
  ): Promise<APIKey> {
    return this.http.request<APIKey>("PATCH", `/v1/server/api-keys/${id}`, {
      auth: "apiKey",
      body: input,
    });
  }

  // whoAmI 自述调用凭证本身（ACCESS_PUBLIC 自证凭证型：出示 key 明文即
  // 查询授权，无效/禁用/过期/删除 → 401）。
  async whoAmI(): Promise<WhoAmIResponse> {
    return this.http.request<WhoAmIResponse>("GET", "/v1/server/api-keys/whoami", {
      auth: "apiKey",
    });
  }
}
