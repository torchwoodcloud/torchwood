import { api } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";
import type { ApiRequestConfig } from "./client";

export interface Group {
  id: string;
  name: string;
  total: number;
  permissions?: string[];
  created_at: string;
  updated_at: string;
}

export interface Membership {
  id: string;
  group_id: string;
  user_id: string;
  email: string;
  name: string;
  roles: string[];
  status: string;
  invited_at?: string;
  joined_at?: string;
  created_at: string;
  updated_at: string;
}

// 服务端组列表（in-memory paginateDocuments，服务端 clamp 默认 25 / max 100，
// created_at DESC）；对接服务端分页，pageSize 必传。
export async function listGroups(params: ListParams): Promise<Page<Group>> {
  const res = await api.get<{ groups: Group[] } & ListMeta>("/server/groups", {
    params: pageQuery(params),
  });
  return { rows: res.data.groups ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getGroup(id: string): Promise<Group> {
  const res = await api.get<Group>(`/server/groups/${id}`);
  return res.data;
}

export async function createGroup(input: { name: string }): Promise<Group> {
  const res = await api.post<Group>("/server/groups", input);
  return res.data;
}

export async function deleteGroup(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/groups/${id}`, config);
}

export async function getGroupPrefs(id: string): Promise<Record<string, unknown>> {
  const res = await api.get<{ prefs: Record<string, unknown> }>(`/server/groups/${id}/prefs`);
  return res.data.prefs ?? {};
}

export async function updateGroupPrefs(
  id: string,
  prefs: Record<string, unknown>
): Promise<Record<string, unknown>> {
  const res = await api.put<{ prefs: Record<string, unknown> }>(`/server/groups/${id}/prefs`, {
    prefs,
  });
  return res.data.prefs ?? {};
}

// 成员列表（同 ListGroups 的服务端分页口径）。
export async function listMemberships(
  groupId: string,
  params: ListParams
): Promise<Page<Membership>> {
  const res = await api.get<{ memberships: Membership[] } & ListMeta>(
    `/server/groups/${groupId}/memberships`,
    { params: pageQuery(params) }
  );
  return { rows: res.data.memberships ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function createMembership(
  groupId: string,
  input: {
    email?: string;
    user_id?: string;
    name?: string;
    roles?: string[];
    status?: string;
  }
): Promise<Membership> {
  const res = await api.post<Membership>(`/server/groups/${groupId}/memberships`, {
    group_id: groupId,
    ...input,
  });
  return res.data;
}

export async function updateMembership(
  groupId: string,
  membershipId: string,
  input: { roles: string[] }
): Promise<Membership> {
  const res = await api.patch<Membership>(
    `/server/groups/${groupId}/memberships/${membershipId}`,
    {
      group_id: groupId,
      membership_id: membershipId,
      roles: input.roles,
    }
  );
  return res.data;
}

export async function updateMembershipStatus(
  groupId: string,
  membershipId: string,
  status: string
): Promise<Membership> {
  const res = await api.patch<Membership>(
    `/server/groups/${groupId}/memberships/${membershipId}/status`,
    {
      group_id: groupId,
      membership_id: membershipId,
      status,
    }
  );
  return res.data;
}

export async function deleteMembership(
  groupId: string,
  membershipId: string,
  config?: ApiRequestConfig
): Promise<void> {
  await api.delete(`/server/groups/${groupId}/memberships/${membershipId}`, config);
}
