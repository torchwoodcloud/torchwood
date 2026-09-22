import { api, type ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface Admin {
  id: string;
  email: string;
  role: string;
  // 用户偏好时区（IANA 名）；空 = 未设置，显示回退浏览器时区。
  timezone?: string;
  created_at?: string;
  updated_at?: string;
}

export interface ListAdminsResponse {
  admins: Admin[];
}

export const ADMIN_ROLES = ["owner", "admin", "member", "viewer"] as const;

export async function getCurrentAdmin(): Promise<Admin> {
  const res = await api.get<Admin>("/console/admins/me");
  return res.data;
}

// updateCurrentAdmin 自助更新个人资料（PATCH /console/admins/me）。
// timezone：undefined = 不修改；"" = 清除偏好（回退浏览器时区）；非空 = IANA 名。
// current_password + new_password：非空 new_password = 改密（须带当前密码校验），
// 成功后服务端撤销全部凭证，需要重新登录。
export async function updateCurrentAdmin(
  input: {
    timezone?: string;
    current_password?: string;
    new_password?: string;
  },
  config?: ApiRequestConfig
): Promise<Admin> {
  const res = await api.patch<Admin>("/console/admins/me", input, config);
  return res.data;
}

// 服务端 admins 列表（in-memory crud，默认 page_size=50）；对接服务端分页
//（契约说明见 pagination.ts），pageSize 必传。
export async function listAdmins(params: ListParams): Promise<Page<Admin>> {
  const res = await api.get<ListAdminsResponse & ListMeta>("/console/admins", {
    params: pageQuery(params),
  });
  return { rows: res.data.admins ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function createAdmin(input: {
  email: string;
  password: string;
  role: string;
}): Promise<Admin> {
  const res = await api.post<Admin>("/console/admins", input);
  return res.data;
}

export async function updateAdmin(
  id: string,
  input: { role?: string; password?: string }
): Promise<Admin> {
  const res = await api.patch<Admin>(
    `/console/admins/${encodeURIComponent(id)}`,
    input
  );
  return res.data;
}

export async function deleteAdmin(id: string): Promise<void> {
  await api.delete(`/console/admins/${encodeURIComponent(id)}`);
}
