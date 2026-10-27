import { runCommand } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  createDocument,
  deleteDocument,
  updateDocument,
  type Attribute,
} from "@/api/databases";
import {
  createDocumentCommand,
  domainCodeOf,
  removeDocumentCommand,
  updateDocumentCommand,
} from "@/kit/documents";

// OCC 切片（§12.5）映射矩阵：通道方言 = OCC 冲突走 HTTP 400 + message 前缀
// 域码（非 409）；409 是重复键/幂等；404 → toApiError not-found，幂等归
// descriptor.treatAs。写函数全 mock，只测 fleet 层收敛。

vi.mock("@/api/databases", () => ({
  createDocument: vi.fn(),
  updateDocument: vi.fn(),
  deleteDocument: vi.fn(),
  // 列表 fleet 化（§12.9-2）后 fetchDocuments 引用；OCC 用例不触达
  listDocuments: vi.fn(),
}));

const attrs: Attribute[] = [
  { id: "a1", key: "title", type: "string", required: true, array: false },
  { id: "a2", key: "count", type: "integer", required: false, array: false },
];

function httpError(status: number, message: string, code = "FailedPrecondition"): AxiosError {
  return new AxiosError("Request failed", "ERR_BAD_REQUEST", undefined, undefined, {
    status,
    statusText: "",
    headers: {},
    config: {} as never,
    data: { error: { code, message } },
  } as never);
}

beforeEach(() => {
  vi.mocked(createDocument).mockReset();
  vi.mocked(updateDocument).mockReset();
  vi.mocked(deleteDocument).mockReset();
});

describe("domainCodeOf", () => {
  it("解析 message 前缀域码（HTTP 面域码判别的唯一可靠面）", () => {
    expect(domainCodeOf("DOCUMENT.VERSION_CONFLICT: version_mismatch")).toBe(
      "DOCUMENT.VERSION_CONFLICT"
    );
    expect(domainCodeOf("IDEMPOTENCY.IN_PROGRESS: ...")).toBe("IDEMPOTENCY.IN_PROGRESS");
    expect(domainCodeOf("plain message")).toBeUndefined();
    expect(domainCodeOf(undefined)).toBeUndefined();
  });
});

describe("updateDocumentCommand（OCC 主面）", () => {
  const values = { title: "t", count: "1" };

  it("ok：data/increment/version 同一次 PATCH（version 透传）", async () => {
    vi.mocked(updateDocument).mockResolvedValue({ id: "d1" } as never);
    const result = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values, increments: { count: "2" }, version: 3 },
      {}
    );
    expect(result).toEqual({ status: "ok", payload: { id: "d1" } });
    expect(vi.mocked(updateDocument)).toHaveBeenCalledWith(
      "app",
      "posts",
      "d1",
      { data: { title: "t", count: 1 }, increment: { count: 2 }, version: 3 },
      { __skipToast: true }
    );
  });

  it("OCC 冲突：400 + VERSION_CONFLICT 前缀 → conflict（结果盒文案，非 409 通道）", async () => {
    vi.mocked(updateDocument).mockRejectedValue(
      httpError(400, "DOCUMENT.VERSION_CONFLICT: version_mismatch")
    );
    const result = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values, version: 3 },
      {}
    );
    expect(result).toEqual({
      status: "conflict",
      message: "The document was modified by someone else (version mismatch). Please refresh and retry",
    });
  });

  it("400 其他域码 → rejected{code=域码前缀, message=中文}", async () => {
    vi.mocked(updateDocument).mockRejectedValue(
      httpError(400, "DOCUMENT.INVALID_ARGUMENT: invalid argument", "InvalidArgument")
    );
    const result = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values, version: 3 },
      {}
    );
    expect(result).toMatchObject({
      status: "rejected",
      code: "DOCUMENT.INVALID_ARGUMENT",
      message: "Invalid request parameters",
    });
  });

  it("未知域码 400 → rejected（网关原文，code 兜底 body.code）", async () => {
    vi.mocked(updateDocument).mockRejectedValue(httpError(400, "plain failure", "InvalidArgument"));
    const result = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values, version: 3 },
      {}
    );
    expect(result).toMatchObject({
      status: "rejected",
      code: "InvalidArgument",
      message: "plain failure",
    });
  });

  it("409（重复键/幂等进行中，非 OCC）→ conflict", async () => {
    vi.mocked(updateDocument).mockRejectedValue(
      httpError(409, "IDEMPOTENCY.IN_PROGRESS: ...", "Aborted")
    );
    const result = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values, version: 3 },
      {}
    );
    expect(result).toMatchObject({ status: "conflict" });
  });

  it("404 → 失败通道 not-found ApiError（update 404 = 文档被删，不幂等）", async () => {
    vi.mocked(updateDocument).mockRejectedValue(httpError(404, "DOCUMENT.NOT_FOUND: ...", "NotFound"));
    await expect(
      runCommand(
        updateDocumentCommand("app", "posts", attrs),
        { docId: "d1", values, version: 3 },
        {}
      )
    ).rejects.toEqual({ kind: "not-found" });
  });

  it("version 前置守卫：缺失/≤0 → 客户端 rejected（不发请求）", async () => {
    for (const version of [undefined, 0]) {
      const result = await runCommand(
        updateDocumentCommand("app", "posts", attrs),
        { docId: "d1", values, version },
        {}
      );
      expect(result).toMatchObject({
        status: "rejected",
        code: "DOCUMENT.VERSION_REQUIRED",
      });
    }
    expect(vi.mocked(updateDocument)).not.toHaveBeenCalled();
  });

  it("客户端 fieldErrors：required 缺失 → rejected{fieldErrors}，不发请求", async () => {
    const result = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values: { title: "", count: "1" }, version: 3 },
      {}
    );
    expect(result).toMatchObject({
      status: "rejected",
      fieldErrors: { title: "Required field" },
    });
    expect(vi.mocked(updateDocument)).not.toHaveBeenCalled();
  });

  it("客户端 fieldErrors：JSON 无效/增量非整数 → 字段级文案", async () => {
    const jsonAttrs: Attribute[] = [
      { id: "a1", key: "meta", type: "json", required: false, array: false },
    ];
    const result = await runCommand(
      updateDocumentCommand("app", "posts", jsonAttrs),
      { docId: "d1", values: { meta: "{oops" }, increments: {}, version: 3 },
      {}
    );
    expect(result).toMatchObject({
      status: "rejected",
      fieldErrors: { meta: expect.stringContaining("JSON parse failed") },
    });

    const incResult = await runCommand(
      updateDocumentCommand("app", "posts", attrs),
      { docId: "d1", values, increments: { count: "1.5" }, version: 3 },
      {}
    );
    expect(incResult).toMatchObject({
      status: "rejected",
      fieldErrors: { "countΔ": "Increment must be an integer" },
    });
  });
});

describe("removeDocumentCommand", () => {
  it("404 → treatAs 幂等 ok（删除已删 = 目标态达成）", async () => {
    vi.mocked(deleteDocument).mockRejectedValue(
      httpError(404, "DOCUMENT.NOT_FOUND: ...", "NotFound")
    );
    const result = await runCommand(
      removeDocumentCommand("app", "posts"),
      { docId: "d1", version: 3 },
      {}
    );
    expect(result).toEqual({ status: "ok", payload: null });
  });

  it("OCC 冲突/守卫面同 update", async () => {
    vi.mocked(deleteDocument).mockRejectedValue(
      httpError(400, "DOCUMENT.VERSION_CONFLICT: version_mismatch")
    );
    const result = await runCommand(
      removeDocumentCommand("app", "posts"),
      { docId: "d1", version: 3 },
      {}
    );
    expect(result).toMatchObject({ status: "conflict" });

    const guarded = await runCommand(
      removeDocumentCommand("app", "posts"),
      { docId: "d1", version: undefined },
      {}
    );
    expect(guarded).toMatchObject({ status: "rejected", code: "DOCUMENT.VERSION_REQUIRED" });
  });
});

describe("createDocumentCommand", () => {
  it("ok：permissions 空不传；409 重复键 → conflict", async () => {
    vi.mocked(createDocument).mockResolvedValue({ id: "d2" } as never);
    const result = await runCommand(
      createDocumentCommand("app", "posts", attrs),
      { values: { title: "t" }, permissions: [] },
      {}
    );
    expect(result).toEqual({ status: "ok", payload: { id: "d2" } });
    expect(vi.mocked(createDocument)).toHaveBeenCalledWith(
      "app",
      "posts",
      { data: { title: "t" } },
      { __skipToast: true }
    );

    vi.mocked(createDocument).mockRejectedValueOnce(
      httpError(409, "DOCUMENT.ALREADY_EXISTS: duplicate key", "AlreadyExists")
    );
    const conflict = await runCommand(
      createDocumentCommand("app", "posts", attrs),
      { values: { title: "t" } },
      {}
    );
    expect(conflict).toMatchObject({
      status: "conflict",
      message: "Document ID already exists. Please use a different ID and retry",
    });
  });
});
