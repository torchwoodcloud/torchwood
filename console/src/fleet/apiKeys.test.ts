import { afterEach, describe, expect, it, vi } from "vitest";
import { runCommand } from "@lynx-go/ui-kit/kernel";
import {
  apiKeysResource,
  createApiKeyCommand,
  editApiKeyDescriptor,
  removeApiKeyCommand,
} from "./apiKeys";

vi.mock("@/api/apiKeys", () => ({
  createAPIKey: vi.fn(),
  updateAPIKey: vi.fn(),
  deleteAPIKey: vi.fn(),
}));
import { createAPIKey, updateAPIKey, deleteAPIKey } from "@/api/apiKeys";

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
    mockCreate.mockRejectedValue(axiosErr(400, { code: "E_INVALID_ARGUMENT", message: "名称无效" }));
    await expect(runCommand(createApiKeyCommand, { name: "", wildcard: false, selected: [], manual: "" }, {})).resolves.toEqual({
      status: "rejected",
      code: "E_INVALID_ARGUMENT",
      message: "名称无效",
    });

    mockCreate.mockRejectedValue(axiosErr(409, { message: "同名 Key 已存在" }));
    await expect(runCommand(createApiKeyCommand, { name: "x", wildcard: false, selected: [], manual: "" }, {})).resolves.toEqual({
      status: "conflict",
      message: "同名 Key 已存在",
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

describe("apiKeysResource 形状", () => {
  it("资源名与存量 queryKey 首段对齐（桥接失效锚点）", () => {
    expect(apiKeysResource.name).toBe("api-keys");
    expect(apiKeysResource.getId?.(key)).toBe("key-1");
  });
});
