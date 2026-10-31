import { runCommand } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  createDatabase,
  deleteDatabase,
  getCollection,
  getDatabase,
} from "@/api/databases";
import {
  collectionsResource,
  createDatabaseCommand,
  databasesResource,
  removeDatabaseCommand,
} from "@/kit/databases";

// 详情读 record 槽（子视图 kit 化批）与建删命令的连带失效声明锚：404 → null
// 空态直出（recordOrNull 手写方言）；其余失败 toApiError 上抛。

vi.mock("@/api/databases", () => ({
  createAttribute: vi.fn(),
  createCollection: vi.fn(),
  createDatabase: vi.fn(),
  createIndex: vi.fn(),
  deleteAttribute: vi.fn(),
  deleteCollection: vi.fn(),
  deleteDatabase: vi.fn(),
  deleteIndex: vi.fn(),
  getCollection: vi.fn(),
  getDatabase: vi.fn(),
  listCollections: vi.fn(),
  listDatabases: vi.fn(),
  updateCollection: vi.fn(),
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
  vi.mocked(createDatabase).mockReset();
  vi.mocked(deleteDatabase).mockReset();
  vi.mocked(getDatabase).mockReset();
  vi.mocked(getCollection).mockReset();
});

describe("databasesResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getDatabase).mockResolvedValue({ id: "app" } as never);
    expect(await databasesResource.record!("app", { projectId: "p1" })).toEqual({ id: "app" });

    vi.mocked(getDatabase).mockRejectedValue(httpError(404, "gone"));
    expect(await databasesResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getDatabase).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(databasesResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("collectionsResource.record", () => {
  it("ctx.dbId 进适配器；404 → null", async () => {
    vi.mocked(getCollection).mockResolvedValue({ id: "posts" } as never);
    const rec = await collectionsResource.record!("posts", { projectId: "p1", dbId: "app" });
    expect(rec).toEqual({ id: "posts" });
    expect(vi.mocked(getCollection)).toHaveBeenCalledWith("app", "posts");

    vi.mocked(getCollection).mockRejectedValue(httpError(404, "gone"));
    expect(
      await collectionsResource.record!("gone", { projectId: "p1", dbId: "app" })
    ).toBeNull();
  });
});

describe("建库/删库的连带失效声明（旧 collections 前缀桥的 kit 键承接）", () => {
  it("create/remove 显式声明同资源 + collections allScopes（dbId 不可知）", () => {
    for (const verb of [createDatabaseCommand, removeDatabaseCommand]) {
      expect(verb.invalidates).toEqual([
        { resource: "databases" },
        { resource: "collections", allScopes: true },
      ]);
    }
    // legacy 前缀桥退役：资源级声明已清零
    expect(databasesResource.legacyKeys).toBeUndefined();
    expect(collectionsResource.legacyKeys).toBeUndefined();
  });

  it("remove 幂等 ok（已删 → ok payload null）", async () => {
    vi.mocked(deleteDatabase).mockRejectedValue(httpError(404, "gone"));
    const result = await runCommand(removeDatabaseCommand, { id: "gone" }, { projectId: "p1" });
    expect(result).toEqual({ status: "ok", payload: null });
  });
});
