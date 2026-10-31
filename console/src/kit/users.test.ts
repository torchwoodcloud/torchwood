import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getUser, listUserSessions } from "@/api/users";
import {
  issueUserTokenDescriptor,
  resetUserPasswordCommand,
  revokeUserSessionDescriptor,
  userSessionsResource,
  usersResource,
} from "@/kit/users";

// 详情读 record 槽 / 会话列表 collection 槽（整取方言）与跨资源连带失效
// 声明锚：user 详情 404 → null 空态直出（recordOrNull 手写方言）；重置密码
// 连带 user-sessions（旧 legacyKeys 宽桥的 kit 键承接）；撤销/签发只打
// user-sessions（不改 user 实体）。

vi.mock("@/api/users", () => ({
  createUser: vi.fn(),
  createUserToken: vi.fn(),
  deleteUser: vi.fn(),
  deleteUserSession: vi.fn(),
  getUser: vi.fn(),
  listUserSessions: vi.fn(),
  listUsers: vi.fn(),
  updateUser: vi.fn(),
  updateUserPassword: vi.fn(),
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
  vi.mocked(getUser).mockReset();
  vi.mocked(listUserSessions).mockReset();
});

describe("usersResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getUser).mockResolvedValue({ id: "u-1", email: "a@b.c" } as never);
    expect(await usersResource.record!("u-1", { projectId: "p1" })).toEqual({
      id: "u-1",
      email: "a@b.c",
    });

    vi.mocked(getUser).mockRejectedValue(httpError(404, "gone"));
    expect(await usersResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getUser).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(usersResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("userSessionsResource.collection（整取方言）", () => {
  it("窗口忽略、全量行单段直出；ctx 缺参静默空段", async () => {
    vi.mocked(listUserSessions).mockResolvedValue([{ id: "s-1" } as never]);
    const seg = await userSessionsResource.collection!(
      { window: { size: 20 } },
      { projectId: "p1", userId: "u-1" }
    );
    expect(seg).toEqual({ rows: [{ id: "s-1" }] });
    expect(seg.nextCursor).toBeUndefined();
    expect(vi.mocked(listUserSessions)).toHaveBeenCalledWith("u-1");

    const empty = await userSessionsResource.collection!(
      { window: { size: 20 } },
      { projectId: "p1" }
    );
    expect(empty).toEqual({ rows: [] });
    expect(vi.mocked(listUserSessions)).toHaveBeenCalledTimes(1);
  });

  it("失败经 toApiError 上抛", async () => {
    vi.mocked(listUserSessions).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(
      userSessionsResource.collection!(
        { window: { size: 20 } },
        { projectId: "p1", userId: "u-1" }
      )
    ).rejects.toEqual({ kind: "server", status: 500, message: "boom" });
  });
});

describe("跨资源连带失效声明（旧 legacyKeys 宽前缀桥的 kit 键承接）", () => {
  it("resetPassword = users（同资源显式带上）+ user-sessions", () => {
    expect(resetUserPasswordCommand.invalidates).toEqual([
      { resource: "users" },
      { resource: "user-sessions" },
    ]);
  });

  it("撤销/签发只打 user-sessions（不改 user 实体）", () => {
    expect(revokeUserSessionDescriptor.invalidates).toEqual([
      { resource: "user-sessions" },
    ]);
    expect(issueUserTokenDescriptor.invalidates).toEqual([
      { resource: "user-sessions" },
    ]);
  });

  it("legacy 前缀桥退役：资源级声明已清零、裸 descriptor 不再携带", () => {
    expect(usersResource.legacyKeys).toBeUndefined();
    expect(Object.hasOwn(revokeUserSessionDescriptor, "legacyKeys")).toBe(false);
  });
});
