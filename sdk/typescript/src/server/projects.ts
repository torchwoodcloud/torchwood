import { listQuery, type HttpTransport } from "../http.js";
import type { InviteCode, ListParams, Project } from "../types.js";

export class ProjectsService {
  constructor(private readonly http: HttpTransport) {}

  async list(params?: ListParams): Promise<Project[]> {
    const res = await this.http.request<{ projects: Project[] }>("GET", "/v1/server/projects", {
      auth: "apiKey",
      query: listQuery(params),
    });
    return res.projects ?? [];
  }

  async get(id: string): Promise<Project> {
    return this.http.request<Project>("GET", `/v1/server/projects/${id}`, { auth: "apiKey" });
  }

  async create(input: { id: string; name: string; description?: string }): Promise<Project> {
    return this.http.request<Project>("POST", "/v1/server/projects", {
      auth: "apiKey",
      body: input,
    });
  }

  async update(
    id: string,
    input: { name?: string; description?: string }
  ): Promise<Project> {
    return this.http.request<Project>("PATCH", `/v1/server/projects/${id}`, {
      auth: "apiKey",
      body: { id, ...input },
    });
  }

  async delete(id: string): Promise<void> {
    await this.http.request<void>("DELETE", `/v1/server/projects/${id}`, { auth: "apiKey" });
  }

  async createInviteCode(
    projectId: string,
    input: { max_uses?: number; expire_at?: string }
  ): Promise<InviteCode> {
    return this.http.request<InviteCode>(
      "POST",
      `/v1/server/projects/${projectId}/invite-codes`,
      { auth: "apiKey", body: input }
    );
  }

  async listInviteCodes(projectId: string): Promise<InviteCode[]> {
    const res = await this.http.request<{ invite_codes: InviteCode[] }>(
      "GET",
      `/v1/server/projects/${projectId}/invite-codes`,
      { auth: "apiKey" }
    );
    return res.invite_codes ?? [];
  }

  async deleteInviteCode(projectId: string, id: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/projects/${projectId}/invite-codes/${id}`,
      { auth: "apiKey" }
    );
  }

  async updateOAuthRedirectAllowlist(
    projectId: string,
    urls: string[]
  ): Promise<Project> {
    return this.http.request<Project>(
      "PUT",
      `/v1/server/projects/${projectId}/oauth-redirect-allowlist`,
      { auth: "apiKey", body: { urls } }
    );
  }
}
