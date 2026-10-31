import { runCommand, type CollectionQuery } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  deleteBucket,
  getBucket,
  getFile,
  getStorageUsage,
  listFiles,
} from "@/api/storage";
import {
  bucketsResource,
  filesResource,
  removeBucketCommand,
  removeFileDescriptor,
  renameFileDescriptor,
  storageUsageResource,
  uploadFileDescriptor,
} from "@/kit/storage";

// 子视图 kit 化批：桶/文件详情读 record 槽（404 → null 空态直出——recordOrNull
// 手写方言）、files 分页 collection 槽（ctx 缺参静默空段）、storage-usage 单例
// record 槽（"current" 哨兵）与失效声明锚（legacy 前缀桥退役；文件命令
// invalidates 指向 files 资源——调用点须传全量 scope）。

vi.mock("@/api/storage", () => ({
  listBuckets: vi.fn(),
  listFiles: vi.fn(),
  getBucket: vi.fn(),
  getFile: vi.fn(),
  getStorageUsage: vi.fn(),
  createBucket: vi.fn(),
  updateBucket: vi.fn(),
  deleteBucket: vi.fn(),
  uploadFile: vi.fn(),
  updateFile: vi.fn(),
  deleteFile: vi.fn(),
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

const q: CollectionQuery = { window: { size: 20 } };

beforeEach(() => {
  vi.mocked(getBucket).mockReset();
  vi.mocked(getFile).mockReset();
  vi.mocked(getStorageUsage).mockReset();
  vi.mocked(listFiles).mockReset();
  vi.mocked(deleteBucket).mockReset();
});

describe("bucketsResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getBucket).mockResolvedValue({ id: "uploads" } as never);
    expect(await bucketsResource.record!("uploads", { projectId: "p1" })).toEqual({
      id: "uploads",
    });

    vi.mocked(getBucket).mockRejectedValue(httpError(404, "gone"));
    expect(await bucketsResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getBucket).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(bucketsResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("filesResource", () => {
  it("record：ctx.bucketId 进适配器；404 → null", async () => {
    vi.mocked(getFile).mockResolvedValue({ id: "f1" } as never);
    const rec = await filesResource.record!("f1", { projectId: "p1", bucketId: "uploads" });
    expect(rec).toEqual({ id: "f1" });
    expect(vi.mocked(getFile)).toHaveBeenCalledWith("uploads", "f1");

    vi.mocked(getFile).mockRejectedValue(httpError(404, "gone"));
    expect(
      await filesResource.record!("gone", { projectId: "p1", bucketId: "uploads" })
    ).toBeNull();
  });

  it("collection：窗口映射分页参数、Page→Segment；ctx 缺参静默空段", async () => {
    vi.mocked(listFiles).mockResolvedValue({
      rows: [{ id: "f1" } as never],
      nextPageToken: "t2",
    });
    const seg = await filesResource.collection!(
      { window: { size: 20, cursor: "t1" } },
      { projectId: "p1", bucketId: "uploads" }
    );
    expect(seg).toEqual({ rows: [{ id: "f1" }], nextCursor: "t2" });
    expect(vi.mocked(listFiles)).toHaveBeenCalledWith("uploads", {
      pageSize: 20,
      pageToken: "t1",
    });

    expect(await filesResource.collection!(q, { projectId: "p1" })).toEqual({ rows: [] });
    expect(vi.mocked(listFiles)).toHaveBeenCalledTimes(1);
  });
});

describe("storageUsageResource.record（单例哨兵读）", () => {
  it("端点无参调用透传；404 → null", async () => {
    vi.mocked(getStorageUsage).mockResolvedValue({ buckets: 2, files: 10, total_size: 2048 });
    const rec = await storageUsageResource.record!("current", { projectId: "p1" });
    expect(rec).toEqual({ buckets: 2, files: 10, total_size: 2048 });
    expect(vi.mocked(getStorageUsage)).toHaveBeenCalledWith();

    vi.mocked(getStorageUsage).mockRejectedValue(httpError(404, "gone"));
    expect(await storageUsageResource.record!("current", { projectId: "p1" })).toBeNull();
  });
});

describe("失效声明锚（legacy 前缀桥退役）", () => {
  it("桶建删连带 storage-usage；文件命令 invalidates 指向 files 资源", () => {
    expect(bucketsResource.commands.create.invalidates).toEqual([
      { resource: "buckets" },
      { resource: "storage-usage" },
    ]);
    expect(bucketsResource.commands.remove.invalidates).toEqual([
      { resource: "buckets" },
      { resource: "storage-usage" },
    ]);
    // update 不动用量——维持缺省同资源失效（无显式声明；退役后 TS 类型面
    // 已无该属性，影子断言保退役锚）
    expect(
      (bucketsResource.commands.update as { invalidates?: unknown }).invalidates
    ).toBeUndefined();

    for (const verb of [uploadFileDescriptor, removeFileDescriptor, renameFileDescriptor]) {
      expect(verb.invalidates).toEqual([{ resource: "files" }]);
      expect((verb as { legacyKeys?: unknown }).legacyKeys).toBeUndefined();
    }
    expect(bucketsResource.legacyKeys).toBeUndefined();
  });

  it("removeBucket 幂等 ok（已删 → ok payload null）", async () => {
    vi.mocked(deleteBucket).mockRejectedValue(httpError(404, "gone"));
    const result = await runCommand(removeBucketCommand, { id: "gone" }, { projectId: "p1" });
    expect(result).toEqual({ status: "ok", payload: null });
  });
});
