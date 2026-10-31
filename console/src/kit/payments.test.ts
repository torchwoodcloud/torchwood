import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { CommandDescriptor } from "@lynx-go/uikit-core/kernel";
import { getOrder } from "@/api/payments";
import { fulfillOrderCommand, ordersResource, refundOrderCommand } from "@/kit/payments";

// AttachedCommands 的推断类型只保留字面量里出现过的声明键——缺省缺席断言走
// CommandDescriptor 声明面视图。
function declarationOf(d: Pick<CommandDescriptor<never, never>, "invalidates" | "legacyKeys">) {
  return { invalidates: d.invalidates, legacyKeys: d.legacyKeys };
}

// 详情读 record 槽（子视图 kit 化批）与失效声明锚：404 → null 空态直出
//（recordOrNull 手写方言）；其余失败 toApiError 上抛；refund/fulfill 缺省
// 同资源失效（声明缺席 = 命中资源缺省全 kind）——legacy 前缀桥退役。

vi.mock("@/api/payments", () => ({
  getOrder: vi.fn(),
  listOrders: vi.fn(),
  manualFulfillOrder: vi.fn(),
  refundOrder: vi.fn(),
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
  vi.mocked(getOrder).mockReset();
});

describe("ordersResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getOrder).mockResolvedValue({ id: "ord-1", status: "paid" } as never);
    expect(await ordersResource.record!("ord-1", { projectId: "p1" })).toEqual({
      id: "ord-1",
      status: "paid",
    });

    vi.mocked(getOrder).mockRejectedValue(httpError(404, "gone"));
    expect(await ordersResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getOrder).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(ordersResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("失效声明锚（legacyKeys 退役，缺省同资源承接详情 record）", () => {
  it("legacy 前缀桥退役；refund/fulfill 挂订单资源（缺省失效的命中源）", () => {
    expect(ordersResource.legacyKeys).toBeUndefined();
    expect(declarationOf(refundOrderCommand).invalidates).toBeUndefined();
    expect(declarationOf(refundOrderCommand).legacyKeys).toBeUndefined();
    expect(refundOrderCommand.__resource).toBe("orders");
    expect(declarationOf(fulfillOrderCommand).invalidates).toBeUndefined();
    expect(declarationOf(fulfillOrderCommand).legacyKeys).toBeUndefined();
    expect(fulfillOrderCommand.__resource).toBe("orders");
  });
});
