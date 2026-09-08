import type { HttpTransport } from "../http.js";
import type { APIKey } from "../types.js";

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
}
