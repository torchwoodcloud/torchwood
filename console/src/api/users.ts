import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import {
  pageQuery,
  serializeGatewayParams,
  type ListMeta,
  type ListParams,
  type Page,
} from "./pagination";

export interface User {
  id: string;
  email: string;
  name: string;
  status: string;
  email_verified: boolean;
  labels?: string[];
  prefs?: Record<string, unknown>;
  phone?: string;
  created_at: string;
  updated_at: string;
}

export interface UserSession {
  id: string;
  user_id: string;
  provider: string;
  user_agent: string;
  ip: string;
  expire_at?: string;
  created_at: string;
}

export interface TokenBundle {
  access_token: string;
  refresh_token: string;
  // RFC3339（Timestamp 经 gateway 序列化为字符串）
  expires_at: string;
}

export interface ListUsersResponse {
  users: User[];
  meta?: ListMeta["meta"] & { total_count?: number };
}

// 服务端列表默认 page_size=50、max 100（created_at DESC）：请求不带 page_size
// 时只返回第一页（2026-09-22 users 56 条已实际踩坑）。对接服务端分页（契约
// 说明见 pagination.ts），pageSize 必传。queries 为服务端白名单 DSL
// （ParseUserList：id/email/name/status/phone/created_at/updated_at，算子
// equal/greaterThan/lessThan），常用过滤：
//   equal("id","<uuid>") / equal("status","active")
//   greaterThan("created_at","<RFC3339>") / lessThan("created_at","<RFC3339>")
export interface ListUsersParams extends ListParams {
  queries?: string[];
}

export async function listUsers(params: ListUsersParams): Promise<Page<User>> {
  const res = await api.get<ListUsersResponse>("/server/users", {
    params: { ...pageQuery(params), queries: params.queries },
    // queries 是 repeated 字段：axios 默认序列化成 queries[]=，gateway 不识别，
    // 必须用网关兼容序列化（见 serializeGatewayParams）。
    paramsSerializer: { serialize: serializeGatewayParams },
  });
  return { rows: res.data.users ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getUser(id: string): Promise<User> {
  const res = await api.get<User>(`/server/users/${id}`);
  return res.data;
}

export async function createUser(input: {
  email: string;
  password: string;
  name?: string;
  status?: string;
  labels?: string[];
  prefs?: Record<string, unknown>;
}): Promise<User> {
  const res = await api.post<User>("/server/users", {
    ...input,
    labels: input.labels ? { values: input.labels } : undefined,
  });
  return res.data;
}

export async function updateUser(
  id: string,
  input: {
    name?: string;
    email?: string;
    status?: string;
    email_verified?: boolean;
    labels?: string[];
    prefs?: Record<string, unknown>;
  }
): Promise<User> {
  const res = await api.patch<User>(`/server/users/${id}`, {
    ...input,
    labels: input.labels ? { values: input.labels } : undefined,
  });
  return res.data;
}

export async function updateUserPassword(id: string, password: string): Promise<User> {
  const res = await api.patch<User>(`/server/users/${id}/password`, { password });
  return res.data;
}

export async function deleteUser(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/users/${id}`, config);
}

export async function listUserSessions(id: string): Promise<UserSession[]> {
  const res = await api.get<{ sessions: UserSession[] }>(`/server/users/${id}/sessions`);
  return res.data.sessions ?? [];
}

export async function deleteUserSession(id: string, sessionId: string): Promise<void> {
  await api.delete(`/server/users/${id}/sessions/${sessionId}`);
}

export async function createUserToken(id: string): Promise<TokenBundle> {
  const res = await api.post<{ tokens: TokenBundle }>(`/server/users/${id}/tokens`);
  return res.data.tokens;
}
