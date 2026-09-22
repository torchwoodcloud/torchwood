import { api } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

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

// 服务端项目列表（in-memory crud，默认 page_size=50，created_at DESC）；
// 对接服务端分页，pageSize 必传。
export async function listProjects(params: ListParams): Promise<Page<Project>> {
  const res = await api.get<ListProjectsResponse & ListMeta>("/server/projects", {
    params: pageQuery(params),
  });
  return { rows: res.data.projects ?? [], nextPageToken: res.data.meta?.next_page_token };
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
  meta?: ListMeta["meta"] & { total_count?: number };
}

// 邀请码列表（offset 型分页；服务端 clamp 默认 50 / max 100，created_at DESC）。
// 2026-09-22 修复：此前服务端忽略 page_size/page_token 固定返回前 100 条。
export async function listInviteCodes(
  projectId: string,
  params: ListParams
): Promise<Page<InviteCode>> {
  const res = await api.get<ListInviteCodesResponse>(
    `/server/projects/${projectId}/invite-codes`,
    { params: pageQuery(params) }
  );
  return { rows: res.data.invite_codes ?? [], nextPageToken: res.data.meta?.next_page_token };
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
