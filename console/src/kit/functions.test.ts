import { runCommand } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  deleteFunctionTrigger,
  getFunction,
  listFunctionTriggers,
  listInvocations,
} from "@/api/functions";
import {
  createTriggerDescriptor,
  functionsResource,
  functionTriggersResource,
  invocationsResource,
  invokeFunctionCommand,
  removeTriggerDescriptor,
  rotateTriggerTokenDescriptor,
} from "@/kit/functions";

// 详情/子视图读槽（子视图 kit 化批）与失效声明锚：record 404 → null 空态
// 直出（recordOrNull 手写方言）；collection 守卫静默空段 + toApiError 上抛。

// mock 工厂必须列出 kit/functions.ts import 的该模块全部具名导出。
vi.mock("@/api/functions", () => ({
  createFunction: vi.fn(),
  createFunctionTrigger: vi.fn(),
  deleteFunction: vi.fn(),
  deleteFunctionTrigger: vi.fn(),
  getFunction: vi.fn(),
  invokeFunction: vi.fn(),
  listFunctionTriggers: vi.fn(),
  listFunctions: vi.fn(),
  listInvocations: vi.fn(),
  rotateFunctionSecret: vi.fn(),
  rotateFunctionTriggerToken: vi.fn(),
  setFunctionScopes: vi.fn(),
  updateFunction: vi.fn(),
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
  vi.mocked(getFunction).mockReset();
  vi.mocked(listInvocations).mockReset();
  vi.mocked(listFunctionTriggers).mockReset();
  vi.mocked(deleteFunctionTrigger).mockReset();
});

describe("functionsResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getFunction).mockResolvedValue({ id: "fn-1" } as never);
    expect(await functionsResource.record!("fn-1", { projectId: "p1" })).toEqual({ id: "fn-1" });
    expect(vi.mocked(getFunction)).toHaveBeenCalledWith("fn-1");

    vi.mocked(getFunction).mockRejectedValue(httpError(404, "gone"));
    expect(await functionsResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getFunction).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(functionsResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("invocationsResource.collection", () => {
  const q = { window: { size: 20, cursor: "tok-2" } };

  it("ctx 缺参静默空段（不触达 api）", async () => {
    expect(await invocationsResource.collection!(q, { projectId: "p1" })).toEqual({ rows: [] });
    expect(await invocationsResource.collection!(q, { functionId: "fn-1" })).toEqual({ rows: [] });
    expect(vi.mocked(listInvocations)).not.toHaveBeenCalled();
  });

  it("window/ctx 透传：cursor→pageToken、functionId、status 旁路键；nextCursor 投影", async () => {
    vi.mocked(listInvocations).mockResolvedValue({
      rows: [{ id: "inv-1" } as never],
      nextPageToken: "tok-3",
    });
    const seg = await invocationsResource.collection!(q, {
      projectId: "p1",
      functionId: "fn-1",
      status: "failed",
    });
    expect(seg).toEqual({ rows: [{ id: "inv-1" }], nextCursor: "tok-3" });
    expect(vi.mocked(listInvocations)).toHaveBeenCalledWith("fn-1", {
      pageSize: 20,
      pageToken: "tok-2",
      status: "failed",
    });
  });

  it("失败经 toApiError 如实上抛", async () => {
    vi.mocked(listInvocations).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(
      invocationsResource.collection!(q, { projectId: "p1", functionId: "fn-1" })
    ).rejects.toEqual({ kind: "server", status: 500, message: "boom" });
  });
});

describe("functionTriggersResource.collection", () => {
  const q = { window: { size: 20 } };

  it("ctx 缺参静默空段；整取透传（端点无分页契约，window 不透传）", async () => {
    expect(await functionTriggersResource.collection!(q, { projectId: "p1" })).toEqual({
      rows: [],
    });
    vi.mocked(listFunctionTriggers).mockResolvedValue([{ id: "trg-1" } as never]);
    const seg = await functionTriggersResource.collection!(q, {
      projectId: "p1",
      functionId: "fn-1",
    });
    expect(seg).toEqual({ rows: [{ id: "trg-1" }] });
    expect(vi.mocked(listFunctionTriggers)).toHaveBeenCalledWith("fn-1");
  });
});

describe("失效声明锚（存量前缀桥退役，子视图 kit 化批）", () => {
  it("invoke 只打 invocations（allScopes——命令侧 functionId 投影必 miss）；不失效 functions", () => {
    expect(invokeFunctionCommand.invalidates).toEqual([
      { resource: "invocations", allScopes: true },
    ]);
  });

  it("触发器三命令显式声明 function-triggers；资源级 legacyKeys 全量退役", () => {
    for (const verb of [
      createTriggerDescriptor,
      removeTriggerDescriptor,
      rotateTriggerTokenDescriptor,
    ]) {
      expect(verb.invalidates).toEqual([{ resource: "function-triggers" }]);
    }
    expect(functionsResource.legacyKeys).toBeUndefined();
    expect(invokeFunctionCommand.legacyKeys).toBeUndefined();
  });

  it("removeTrigger 幂等 ok（已删 → ok payload null）", async () => {
    vi.mocked(deleteFunctionTrigger).mockRejectedValue(httpError(404, "gone"));
    const result = await runCommand(
      removeTriggerDescriptor,
      { functionId: "fn-1", triggerId: "trg-1" },
      { projectId: "p1", functionId: "fn-1" }
    );
    expect(result).toEqual({ status: "ok", payload: null });
  });
});
