import { api } from "./client";

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

// updateCurrentAdmin 自助更新个人偏好（PATCH /console/admins/me）。
// timezone：undefined = 不修改；"" = 清除偏好（回退浏览器时区）；非空 = IANA 名。
export async function updateCurrentAdmin(input: {
  timezone?: string;
}): Promise<Admin> {
  const res = await api.patch<Admin>("/console/admins/me", input);
  return res.data;
}

export async function listAdmins(): Promise<Admin[]> {
  const res = await api.get<ListAdminsResponse>("/console/admins");
  return res.data.admins ?? [];
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
