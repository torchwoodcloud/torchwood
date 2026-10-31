import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { runCommand } from "@lynx-go/uikit-core/kernel";
import {
  apiKeysResource,
  createApiKeyCommand,
  editApiKeyDescriptor,
  removeApiKeyCommand,
} from "./apiKeys";

// mock 工厂列出 kit/apiKeys.ts import 的该模块全部具名导出（子视图 kit 化
// 批起 getAPIKey 进 record 槽）。
vi.mock("@/api/apiKeys", () => ({
  listAPIKeys: vi.fn(),
  getAPIKey: vi.fn(),
  createAPIKey: vi.fn(),
  updateAPIKey: vi.fn(),
  deleteAPIKey: vi.fn(),
}));
import {
  createAPIKey,
  getAPIKey,
  updateAPIKey,
  deleteAPIKey,
} from "@/api/apiKeys";

const mockGet = vi.mocked(getAPIKey);
const mockCreate = vi.mocked(createAPIKey);
const mockUpdate = vi.mocked(updateAPIKey);
const mockDelete = vi.mocked(deleteAPIKey);

function axiosErr(status: number, body?: { code?: string; message?: string }) {
  return Object.assign(new Error("req failed"), {
    isAxiosError: true,
    response: { status, data: body ? { error: body } : undefined },
  });
}

const key = {
  id: "key-1",
  name: "prod",
  scopes: ["databases:blog.read"],
  enabled: true,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

beforeEach(() => {
  mockGet.mockReset();
});

afterEach(() => {
  vi.clearAllMocks();
});

describe("createApiKeyCommand（表单值→提交形收敛在 run 内）", () => {
  it("wildcard 优先：scopes 收敛为 [*]", async () => {
    mockCreate.mockResolvedValue({ api_key: key, secret: "s1" });
    const result = await runCommand(
      createApiKeyCommand,
      { name: "n", wildcard: true, selected: ["a.read"], manual: "b.write" },
      {},
    );
    expect(mockCreate).toHaveBeenCalledWith({ name: "n", scopes: ["*"] }, { __skipToast: true });
    expect(result).toEqual({ status: "ok", payload: { api_key: key, secret: "s1" } });
  });

  it("勾选与手输合并去重排序", async () => {
    mockCreate.mockResolvedValue({ api_key: key, secret: "s1" });
    await runCommand(
      createApiKeyCommand,
      { name: "n", wildcard: false, selected: ["b.write", "a.read"], manual: "a.read, c.admin\nx.y" },
      {},
    );
    expect(mockCreate).toHaveBeenCalledWith(
      { name: "n", scopes: ["a.read", "b.write", "c.admin", "x.y"] },
      { __skipToast: true },
    );
  });

  it("结果两分：400→rejected（带 gateway code）、409→conflict、其余 throw ApiError", async () => {
    mockCreate.mockRejectedValue(axiosErr(400, { code: "E_INVALID_ARGUMENT", message: "Invalid name" }));
    await expect(runCommand(createApiKeyCommand, { name: "", wildcard: false, selected: [], manual: "" }, {})).resolves.toEqual({
      status: "rejected",
      code: "E_INVALID_ARGUMENT",
      message: "Invalid name",
    });

    mockCreate.mockRejectedValue(axiosErr(409, { message: "A key with the same name already exists" }));
    await expect(runCommand(createApiKeyCommand, { name: "x", wildcard: false, selected: [], manual: "" }, {})).resolves.toEqual({
      status: "conflict",
      message: "A key with the same name already exists",
    });

    mockCreate.mockRejectedValue(axiosErr(500, { message: "boom" }));
    await expect(runCommand(createApiKeyCommand, { name: "x", wildcard: false, selected: [], manual: "" }, {})).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("editApiKeyDescriptor（proto3 optional diff 归 descriptor）", () => {
  it("无变更 → 空 patch", async () => {
    mockUpdate.mockResolvedValue(key);
    await runCommand(
      editApiKeyDescriptor(key),
      { name: key.name, scopes: key.scopes.join(", "), enabled: key.enabled, expireAt: "" },
      {},
    );
    expect(mockUpdate).toHaveBeenCalledWith("key-1", {}, { __skipToast: true });
  });

  it("逐字段 diff：仅传变更字段；expireAt 按 ISO 比较", async () => {
    mockUpdate.mockResolvedValue(key);
    await runCommand(
      editApiKeyDescriptor(key),
      {
        name: "prod2",
        scopes: "databases:blog.read, storage:x",
        enabled: false,
        expireAt: "2027-01-01T00:00:00.000Z",
      },
      {},
    );
    expect(mockUpdate).toHaveBeenCalledWith(
      "key-1",
      {
        name: "prod2",
        scopes: ["databases:blog.read", "storage:x"],
        enabled: false,
        expire_at: "2027-01-01T00:00:00.000Z",
      },
      { __skipToast: true },
    );
  });

  it("expireAt 与原值相同 ISO → 不传", async () => {
    mockUpdate.mockResolvedValue(key);
    const sameIso = new Date(key.created_at).toISOString();
    await runCommand(
      editApiKeyDescriptor({ ...key, expire_at: sameIso }),
      { name: key.name, scopes: key.scopes.join(", "), enabled: key.enabled, expireAt: sameIso },
      {},
    );
    expect(mockUpdate).toHaveBeenCalledWith("key-1", {}, { __skipToast: true });
  });
});

describe("removeApiKeyCommand（treatAs：已删 → 幂等 ok）", () => {
  it("404 → ok null；成功 → ok null", async () => {
    mockDelete.mockRejectedValue(axiosErr(404));
    await expect(runCommand(removeApiKeyCommand, { id: "key-1" }, {})).resolves.toEqual({
      status: "ok",
      payload: null,
    });
    expect(mockDelete).toHaveBeenCalledWith("key-1", { __skipToast: true });

    mockDelete.mockResolvedValue(undefined);
    await expect(runCommand(removeApiKeyCommand, { id: "key-1" }, {})).resolves.toEqual({
      status: "ok",
      payload: null,
    });
  });
});

describe("apiKeysResource.record（详情读，子视图 kit 化批）", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    mockGet.mockResolvedValue(key);
    expect(await apiKeysResource.record!("key-1", { projectId: "p1" })).toEqual(key);
    expect(mockGet).toHaveBeenCalledWith("key-1");

    mockGet.mockRejectedValue(axiosErr(404));
    expect(await apiKeysResource.record!("gone", { projectId: "p1" })).toBeNull();

    mockGet.mockRejectedValue(axiosErr(500, { message: "boom" }));
    await expect(apiKeysResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe('失效声明（存量 ["api-keys"] 前缀桥退役后的 kit 单源）', () => {
  it("资源级 legacyKeys 退役；edit 裸 descriptor 保留同资源 invalidates 全 kind", () => {
    expect(apiKeysResource.legacyKeys).toBeUndefined();
    // 裸 descriptor 的 legacyKeys 条目退役：声明面不再有该键（invalidates
    // 保留——kit 键全 kind，编辑 ok 后列表与详情一并刷新）。
    expect(Object.hasOwn(editApiKeyDescriptor(key), "legacyKeys")).toBe(false);
    expect(editApiKeyDescriptor(key).invalidates).toEqual([{ resource: "api-keys" }]);
  });
});

describe("apiKeysResource 形状", () => {
  it("资源名 = 旧 legacy 前缀首段（kit 键首段自持，失效同源）", () => {
    expect(apiKeysResource.name).toBe("api-keys");
    expect(apiKeysResource.getId?.(key)).toBe("key-1");
  });
});
