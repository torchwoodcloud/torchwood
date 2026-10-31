import { runCommand } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  createInviteCode,
  deleteInviteCode,
  getProject,
  listInviteCodes,
} from "@/api/projects";
import {
  createInviteCodeCommand,
  inviteCodesResource,
  projectsResource,
  removeInviteCodeCommand,
  removeProjectCommand,
  updateOAuthAllowlistCommand,
  updateProjectCommand,
} from "@/kit/projects";

// 详情读 record 槽（子视图 kit 化批）与 invite-codes 分页列表资源/建删命令的
// 失效声明锚：record 404 → null 空态直出（recordOrNull 手写方言）；collection
// ctx 缺参静默空段；invite 建删失效 = kit invite-codes 键（存量前缀桥退役）。

vi.mock("@/api/projects", () => ({
  createInviteCode: vi.fn(),
  createProject: vi.fn(),
  deleteInviteCode: vi.fn(),
  deleteProject: vi.fn(),
  getProject: vi.fn(),
  listInviteCodes: vi.fn(),
  listProjects: vi.fn(),
  updateOAuthRedirectAllowlist: vi.fn(),
  updateProject: vi.fn(),
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
  vi.mocked(getProject).mockReset();
  vi.mocked(listInviteCodes).mockReset();
  vi.mocked(createInviteCode).mockReset();
  vi.mocked(deleteInviteCode).mockReset();
});

describe("projectsResource.record（详情读，子视图 kit 化批）", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getProject).mockResolvedValue({ id: "app" } as never);
    expect(await projectsResource.record!("app", {})).toEqual({ id: "app" });

    vi.mocked(getProject).mockRejectedValue(httpError(404, "gone"));
    expect(await projectsResource.record!("gone", {})).toBeNull();

    vi.mocked(getProject).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(projectsResource.record!("x", {})).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });

  it("legacy [\"projects\"] 前缀桥保留（壳层读者 ProjectBootstrap/ProjectSelector 仍在）", () => {
    expect(projectsResource.legacyKeys?.({})).toEqual([["projects"]]);
  });

  it("project 实体写命令双通道失效：invalidates 打 kit 键（详情 record 承接）+ legacyKeys 打壳层", () => {
    for (const verb of [updateProjectCommand, removeProjectCommand, updateOAuthAllowlistCommand]) {
      expect(verb.invalidates).toEqual([{ resource: "projects" }]);
      expect(verb.legacyKeys?.({})).toEqual([["projects"]]);
    }
  });
});

describe("inviteCodesResource.collection（settings 分页列表，子视图 kit 化批）", () => {
  it("窗口映射 pageSize/pageToken，Page→Segment", async () => {
    vi.mocked(listInviteCodes).mockResolvedValue({
      rows: [{ id: "c1" } as never],
      nextPageToken: "t2",
    });
    const seg = await inviteCodesResource.collection!(
      { window: { size: 20, cursor: "t1" } },
      { projectId: "p1" }
    );
    expect(vi.mocked(listInviteCodes)).toHaveBeenCalledWith("p1", {
      pageSize: 20,
      pageToken: "t1",
    });
    expect(seg).toEqual({ rows: [{ id: "c1" }], nextCursor: "t2" });

    vi.mocked(listInviteCodes).mockResolvedValue({ rows: [] });
    await inviteCodesResource.collection!({ window: { size: 20 } }, { projectId: "p1" });
    expect(vi.mocked(listInviteCodes)).toHaveBeenLastCalledWith("p1", {
      pageSize: 20,
      pageToken: undefined,
    });
  });

  it("projectId 缺失时静默空段（不发起请求）", async () => {
    const seg = await inviteCodesResource.collection!({ window: { size: 20 } }, {});
    expect(seg).toEqual({ rows: [] });
    expect(vi.mocked(listInviteCodes)).not.toHaveBeenCalled();
  });

  it("axios 失败收敛为 ApiError 封闭集", async () => {
    vi.mocked(listInviteCodes).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(
      inviteCodesResource.collection!({ window: { size: 20 } }, { projectId: "p1" })
    ).rejects.toEqual({ kind: "server", status: 500, message: "boom" });
  });
});

describe("invite 建删命令的失效声明锚（存量 invite-codes 前缀桥退役）", () => {
  it("invalidates = kit invite-codes 键；scopeOf 从 input 派生 projectId", () => {
    for (const verb of [createInviteCodeCommand, removeInviteCodeCommand]) {
      expect(verb.invalidates).toEqual([{ resource: "invite-codes" }]);
      expect(verb.legacyKeys).toBeUndefined();
    }
    expect(createInviteCodeCommand.scopeOf?.({ projectId: "p9" })).toEqual({ projectId: "p9" });
    expect(removeInviteCodeCommand.scopeOf?.({ projectId: "p9", id: "c1" })).toEqual({
      projectId: "p9",
    });
  });

  it("create ok：maxUses/expireAt 展示形收敛提交形（__skipToast 单源）", async () => {
    vi.mocked(createInviteCode).mockResolvedValue({ id: "c1" } as never);
    const result = await runCommand(
      createInviteCodeCommand,
      { projectId: "p1", maxUses: 5, expireAt: "2026-01-01T00:00:00.000Z" },
      { projectId: "p1" }
    );
    expect(result).toEqual({ status: "ok", payload: { id: "c1" } });
    expect(vi.mocked(createInviteCode)).toHaveBeenCalledWith(
      "p1",
      { max_uses: 5, expire_at: "2026-01-01T00:00:00.000Z" },
      { __skipToast: true }
    );
  });
});
