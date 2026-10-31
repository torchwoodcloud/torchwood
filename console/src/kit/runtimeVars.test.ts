import { runCommand, type CollectionQuery } from "@lynx-go/uikit-core/kernel";
import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  deleteVarSet,
  getVarSet,
  getVersion,
  listVars,
  listVersions,
} from "@/api/runtimeVars";
import {
  createVarCommand,
  editVarDescriptor,
  editVarSetDescriptor,
  removeVarCommand,
  removeVarSetCommand,
  rollbackVarSetCommand,
  runtimeVarVersionResource,
  runtimeVarVersionsResource,
  runtimeVarsResource,
  varSetsResource,
} from "@/kit/runtimeVars";

// 子视图 kit 化批：详情读 record 槽（404 → null 空态直出——recordOrNull 手写
// 方言）、vars/versions 整取 collection 槽（ctx 缺参静默空段）与 var 级命令的
// 三资源 invalidates 声明锚（legacy 前缀桥退役）。

vi.mock("@/api/runtimeVars", () => ({
  createVar: vi.fn(),
  createVarSet: vi.fn(),
  deleteVar: vi.fn(),
  deleteVarSet: vi.fn(),
  getVarSet: vi.fn(),
  getVersion: vi.fn(),
  listVarSets: vi.fn(),
  listVars: vi.fn(),
  listVersions: vi.fn(),
  rollback: vi.fn(),
  updateVar: vi.fn(),
  updateVarSet: vi.fn(),
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
  vi.mocked(getVarSet).mockReset();
  vi.mocked(getVersion).mockReset();
  vi.mocked(listVars).mockReset();
  vi.mocked(listVersions).mockReset();
  vi.mocked(deleteVarSet).mockReset();
});

describe("varSetsResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getVarSet).mockResolvedValue({ id: "flags" } as never);
    expect(await varSetsResource.record!("flags", { projectId: "p1" })).toEqual({ id: "flags" });

    vi.mocked(getVarSet).mockRejectedValue(httpError(404, "gone"));
    expect(await varSetsResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getVarSet).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(varSetsResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("runtimeVarsResource.collection（整取方言）", () => {
  it("ctx.varSetId 进适配器，行全量直出（无窗口参数）", async () => {
    vi.mocked(listVars).mockResolvedValue([{ key: "a", var_set_id: "flags" }] as never);
    const seg = await runtimeVarsResource.collection!(q, { projectId: "p1", varSetId: "flags" });
    expect(seg).toEqual({ rows: [{ key: "a", var_set_id: "flags" }] });
    expect(vi.mocked(listVars)).toHaveBeenCalledWith("flags");
  });

  it("ctx 缺参静默空段（不发起请求）；失败收敛 ApiError", async () => {
    expect(await runtimeVarsResource.collection!(q, { projectId: "p1" })).toEqual({ rows: [] });
    expect(vi.mocked(listVars)).not.toHaveBeenCalled();

    vi.mocked(listVars).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(
      runtimeVarsResource.collection!(q, { projectId: "p1", varSetId: "flags" })
    ).rejects.toEqual({ kind: "server", status: 500, message: "boom" });
  });
});

describe("runtimeVarVersionsResource.collection（整取方言）", () => {
  it("ctx.varSetId 进适配器；ctx 缺参静默空段", async () => {
    vi.mocked(listVersions).mockResolvedValue([{ revision: 1 }] as never);
    const seg = await runtimeVarVersionsResource.collection!(q, {
      projectId: "p1",
      varSetId: "flags",
    });
    expect(seg).toEqual({ rows: [{ revision: 1 }] });
    expect(vi.mocked(listVersions)).toHaveBeenCalledWith("flags");

    expect(
      await runtimeVarVersionsResource.collection!(q, { projectId: "p1" })
    ).toEqual({ rows: [] });
    expect(vi.mocked(listVersions)).toHaveBeenCalledTimes(1);
  });
});

describe("runtimeVarVersionResource.record（版本快照单读）", () => {
  it("revision 为 id；ctx.varSetId 进适配器；404 → null（窗口淘汰空态直出）", async () => {
    vi.mocked(getVersion).mockResolvedValue({ entries: [] });
    const rec = await runtimeVarVersionResource.record!(3, { projectId: "p1", varSetId: "flags" });
    expect(rec).toEqual({ entries: [] });
    expect(vi.mocked(getVersion)).toHaveBeenCalledWith("flags", 3);

    vi.mocked(getVersion).mockRejectedValue(httpError(404, "target revision pruned"));
    expect(
      await runtimeVarVersionResource.record!(9, { projectId: "p1", varSetId: "flags" })
    ).toBeNull();
  });
});

describe("var 级命令的三资源 invalidates 声明锚（legacy 前缀桥退役）", () => {
  it("varCreate/varRemove/rollback 与裸 editVar 工厂 = 三资源全失效", () => {
    const full = [
      { resource: "runtime-var-sets" },
      { resource: "runtime-vars" },
      { resource: "runtime-var-versions" },
    ];
    for (const verb of [createVarCommand, removeVarCommand, rollbackVarSetCommand]) {
      expect(verb.invalidates).toEqual(full);
    }
    // 裸 editVar 工厂（键集同族）；editVarSet 收窄为仅 var-sets（元数据变更
    // 不入版本链，D10——旧宽桥连带刷 versions 属过宽）。
    expect(editVarDescriptor("flags", "a").invalidates).toEqual(full);
    expect(editVarSetDescriptor({} as never).invalidates).toEqual([
      { resource: "runtime-var-sets" },
    ]);
  });

  it("legacy 前缀桥退役：资源级与 descriptor 级声明均已清零", () => {
    expect(varSetsResource.legacyKeys).toBeUndefined();
    expect(varSetsResource.commands.create.legacyKeys).toBeUndefined();
    expect(varSetsResource.commands.varCreate.legacyKeys).toBeUndefined();
    expect(editVarDescriptor("flags", "a").legacyKeys).toBeUndefined();
  });

  it("removeVarSet 幂等 ok（已删 → ok payload null）", async () => {
    vi.mocked(deleteVarSet).mockRejectedValue(httpError(404, "gone"));
    const result = await runCommand(removeVarSetCommand, { varSetId: "gone" }, { projectId: "p1" });
    expect(result).toEqual({ status: "ok", payload: null });
  });
});
