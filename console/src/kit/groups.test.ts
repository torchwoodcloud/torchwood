import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getGroup, getGroupPrefs, listMemberships } from "@/api/groups";
import {
  addGroupMemberCommand,
  createGroupCommand,
  groupPrefsResource,
  groupsResource,
  membershipsResource,
  removeGroupCommand,
  removeGroupMemberCommand,
  setGroupMemberRoleCommand,
  setGroupMemberStatusCommand,
  updateGroupPrefsCommand,
} from "@/kit/groups";

// 详情读 record 槽 / 成员列表 collection 槽（子视图 kit 化批）与跨资源连带
// 失效声明锚：group 详情 404 → null 空态直出（recordOrNull 手写方言）；
// prefs 读无 404→null 空态分支（卡片对 !prefs 无消费面）——失败如实上抛。

vi.mock("@/api/groups", () => ({
  createGroup: vi.fn(),
  createMembership: vi.fn(),
  deleteGroup: vi.fn(),
  deleteMembership: vi.fn(),
  getGroup: vi.fn(),
  getGroupPrefs: vi.fn(),
  listGroups: vi.fn(),
  listMemberships: vi.fn(),
  updateGroupPrefs: vi.fn(),
  updateMembership: vi.fn(),
  updateMembershipStatus: vi.fn(),
}));

function httpError(status: number, message: string, code = "NotFound"): AxiosError {
  return new AxiosError("Request failed", "ERR_BAD_REQUEST", undefined, undefined, {
    status,
    statusText: "",
    headers: {},
    config: {} as never,
    data: { error: { code, message } },
  } as never);
}

beforeEach(() => {
  vi.mocked(getGroup).mockReset();
  vi.mocked(getGroupPrefs).mockReset();
  vi.mocked(listMemberships).mockReset();
});

describe("groupsResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getGroup).mockResolvedValue({ id: "g-1", name: "Eng" } as never);
    expect(await groupsResource.record!("g-1", { projectId: "p1" })).toEqual({
      id: "g-1",
      name: "Eng",
    });

    vi.mocked(getGroup).mockRejectedValue(httpError(404, "gone"));
    expect(await groupsResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getGroup).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(groupsResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("groupPrefsResource.record", () => {
  it("ctx.groupId 进适配器；失败如实上抛（无 404→null 方言）", async () => {
    vi.mocked(getGroupPrefs).mockResolvedValue({ theme: "dark" });
    const prefs = await groupPrefsResource.record!("g-1", { projectId: "p1", groupId: "g-1" });
    expect(prefs).toEqual({ theme: "dark" });
    expect(vi.mocked(getGroupPrefs)).toHaveBeenCalledWith("g-1");

    vi.mocked(getGroupPrefs).mockRejectedValue(httpError(404, "gone"));
    await expect(
      groupPrefsResource.record!("gone", { projectId: "p1", groupId: "gone" })
    ).rejects.toEqual({ kind: "not-found" });
  });
});

describe("membershipsResource.collection", () => {
  it("窗口映射 pageSize/pageToken，Page→Segment；ctx 缺参静默空段", async () => {
    vi.mocked(listMemberships).mockResolvedValue({
      rows: [{ id: "m-1" } as never],
      nextPageToken: "tok-2",
    });
    const seg = await membershipsResource.collection!(
      { window: { size: 20, cursor: "tok-1" } },
      { projectId: "p1", groupId: "g-1" }
    );
    expect(seg).toEqual({ rows: [{ id: "m-1" }], nextCursor: "tok-2" });
    expect(vi.mocked(listMemberships)).toHaveBeenCalledWith("g-1", {
      pageSize: 20,
      pageToken: "tok-1",
    });

    const empty = await membershipsResource.collection!(
      { window: { size: 20 } },
      { projectId: "p1" }
    );
    expect(empty).toEqual({ rows: [] });
    expect(vi.mocked(listMemberships)).toHaveBeenCalledTimes(1);
  });

  it("失败经 toApiError 上抛", async () => {
    vi.mocked(listMemberships).mockRejectedValue(httpError(403, "no", "PermissionDenied"));
    await expect(
      membershipsResource.collection!(
        { window: { size: 20 } },
        { projectId: "p1", groupId: "g-1" }
      )
    ).rejects.toEqual({ kind: "forbidden" });
  });
});

describe("跨资源连带失效声明（旧 legacyKeys 宽前缀桥的 kit 键承接）", () => {
  it("成员命令族 = groups + memberships", () => {
    for (const verb of [
      addGroupMemberCommand,
      setGroupMemberRoleCommand,
      setGroupMemberStatusCommand,
      removeGroupMemberCommand,
    ]) {
      expect(verb.invalidates).toEqual([
        { resource: "groups" },
        { resource: "memberships" },
      ]);
    }
  });

  it("updatePrefs = groups（同资源显式带上）+ group-prefs", () => {
    expect(updateGroupPrefsCommand.invalidates).toEqual([
      { resource: "groups" },
      { resource: "group-prefs" },
    ]);
  });

  it("建删连带 memberships 缓存失活（同 id 重建防死缓存，allScopes 与旧宽桥同宽）", () => {
    for (const verb of [createGroupCommand, removeGroupCommand]) {
      expect(verb.invalidates).toEqual([
        { resource: "groups" },
        { resource: "memberships", allScopes: true },
      ]);
    }
  });

  it("legacy 前缀桥退役：资源级声明已清零", () => {
    expect(groupsResource.legacyKeys).toBeUndefined();
  });
});
