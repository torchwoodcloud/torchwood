import { api } from "./client";

export interface Project {
  id: string;
  name: string;
  description?: string;
  status: string;
  registration_policy?: string;
  oauth_allowed_redirect_urls?: string[];
  created_at: string;
  updated_at: string;
}

export interface ListProjectsResponse {
  projects: Project[];
  meta?: { total_count?: number; page_size?: number };
}

export async function listProjects(): Promise<Project[]> {
  const res = await api.get<ListProjectsResponse>("/server/projects");
  return res.data.projects ?? [];
}

export async function getProject(id: string): Promise<Project> {
  const res = await api.get<Project>(`/server/projects/${id}`);
  return res.data;
}

export async function createProject(input: {
  id: string;
  name: string;
  description?: string;
}): Promise<Project> {
  const res = await api.post<Project>("/server/projects", input);
  return res.data;
}

export async function updateProject(
  id: string,
  input: { name?: string; description?: string; registration_policy?: string }
): Promise<Project> {
  const res = await api.patch<Project>(`/server/projects/${id}`, input);
  return res.data;
}

export async function deleteProject(id: string): Promise<void> {
  await api.delete(`/server/projects/${id}`);
}

// ---- OAuth 重定向白名单（settings auth.oauth_allowed_redirect_urls）----

export async function updateOAuthRedirectAllowlist(
  projectId: string,
  urls: string[]
): Promise<Project> {
  const res = await api.put<Project>(
    `/server/projects/${projectId}/oauth-redirect-allowlist`,
    { urls }
  );
  return res.data;
}

// ---- 邀请码（T-03，invite_only 注册策略）----

export interface InviteCode {
  id: string;
  project_id: string;
  code: string;
  max_uses: number;
  used_count: number;
  expire_at?: string;
  revoked: boolean;
  created_by: string;
  created_at: string;
}

export interface ListInviteCodesResponse {
  invite_codes: InviteCode[];
  meta?: { total_count?: number; page_size?: number };
}

export async function listInviteCodes(projectId: string): Promise<InviteCode[]> {
  const res = await api.get<ListInviteCodesResponse>(
    `/server/projects/${projectId}/invite-codes`
  );
  return res.data.invite_codes ?? [];
}

export async function createInviteCode(
  projectId: string,
  input: { max_uses?: number; expire_at?: string }
): Promise<InviteCode> {
  const res = await api.post<InviteCode>(
    `/server/projects/${projectId}/invite-codes`,
    input
  );
  return res.data;
}

export async function deleteInviteCode(
  projectId: string,
  id: string
): Promise<void> {
  await api.delete(`/server/projects/${projectId}/invite-codes/${id}`);
}
