import { afterEach, describe, expect, it, vi } from "vitest";
import type { AxiosError } from "axios";
import { auditLogsResource, filtersOf } from "./auditLogs";
import type { CollectionQuery, Predicate } from "@lynx-go/uikit-core/kernel";

vi.mock("@/api/auditLogs", () => ({ listAuditLogs: vi.fn() }));
import { listAuditLogs } from "@/api/auditLogs";

const mockList = vi.mocked(listAuditLogs);

afterEach(() => {
  vi.clearAllMocks();
});

function axiosErr(status: number): AxiosError {
  return Object.assign(new Error("req failed"), {
    isAxiosError: true,
    response: { status, data: { error: { code: "E", message: "boom" } } },
  }) as AxiosError;
}

async function collect(q: CollectionQuery, projectId: string | null = "p1") {
  return auditLogsResource.collection!(q, projectId == null ? {} : { projectId });
}

describe("auditLogsResource.collection 映射", () => {
  it("窗口/排序映射 page_size + sort_order，Page→Segment", async () => {
    mockList.mockResolvedValue({
      audit_logs: [{ id: "a1", action: "/svc/Do", status: "success" }],
      meta: { total_count: 45, next_page_token: "t2" },
    });
    const seg = await collect({
      window: { size: 50 },
      order: [{ field: "created_at", dir: "desc" }],
    });
    expect(mockList.mock.calls[0][0]).toMatchObject({ page_size: 50, sort_order: "DESC" });
    expect(seg).toEqual({
      rows: [{ id: "a1", action: "/svc/Do", status: "success" }],
      total: 45,
      nextCursor: "t2",
    });
  });

  it("游标映射 page_token；无 order = 不传 sort_order（服务端默认 DESC）", async () => {
    mockList.mockResolvedValue({ audit_logs: [] });
    await collect({ window: { size: 20, cursor: "tok-9" } });
    expect(mockList.mock.calls[0][0]).toMatchObject({ page_size: 20, page_token: "tok-9" });
    expect(mockList.mock.calls[0][0].sort_order).toBeUndefined();
  });

  it("谓词白名单：已知 eq 叶子映射、未知字段丢弃、scope 伪字段展开布尔参数", async () => {
    mockList.mockResolvedValue({ audit_logs: [] });
    const predicate: Predicate = {
      all: [
        { field: "action", op: "eq", value: "UpdateFunction" },
        { field: "evil", op: "eq", value: "x" },
        { field: "scope", op: "eq", value: "all_projects" },
      ],
    };
    await collect({ window: { size: 50 }, predicate });
    const params = mockList.mock.calls[0][0];
    expect(params).toMatchObject({ action: "UpdateFunction", all_projects: true });
    expect(params).not.toHaveProperty("evil");
    expect(params.include_platform).toBe(true);
  });

  it("projectId 缺失时静默空段（不发起请求）", async () => {
    const seg = await collect({ window: { size: 50 } }, null);
    expect(seg).toEqual({ rows: [] });
    expect(mockList).not.toHaveBeenCalled();
  });

  it("axios 失败收敛为 ApiError 封闭集", async () => {
    mockList.mockRejectedValue(axiosErr(403));
    await expect(collect({ window: { size: 50 } }, "p1")).rejects.toEqual({ kind: "forbidden" });
    mockList.mockRejectedValue(axiosErr(500));
    await expect(collect({ window: { size: 50 } }, "p1")).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
    const network = Object.assign(new Error("net"), { isAxiosError: true }) as AxiosError;
    mockList.mockRejectedValue(network);
    await expect(collect({ window: { size: 50 } }, "p1")).rejects.toMatchObject({ kind: "network" });
  });
});

describe("filtersOf", () => {
  it("从谓词抽取白名单字段，忽略非字符串值", () => {
    expect(filtersOf({ all: [{ field: "status", op: "eq", value: "denied" }, { field: "x", op: "eq", value: "1" }] })).toEqual({
      status: "denied",
    });
    expect(filtersOf(undefined)).toEqual({});
  });
});
