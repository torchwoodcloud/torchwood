import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { CommandDescriptor } from "@lynx-go/uikit-core/kernel";
import { getPlan, getSubscription } from "@/api/subscriptions";

// AttachedCommands 的推断类型只保留字面量里出现过的声明键——缺省缺席断言走
// CommandDescriptor 声明面视图。
function declarationOf(d: Pick<CommandDescriptor<never, never>, "invalidates" | "legacyKeys">) {
  return { invalidates: d.invalidates, legacyKeys: d.legacyKeys };
}
import {
  cancelSubscriptionCommand,
  expireSubscriptionCommand,
  plansResource,
  removePlanCommand,
  subscriptionsResource,
} from "@/kit/subscriptions";

// 详情读 record 槽（子视图 kit 化批）与失效声明锚：404 → null 空态直出
//（recordOrNull 手写方言）；其余失败 toApiError 上抛；cancel/expire/remove
// 缺省同资源失效（声明缺席 = 命中资源缺省全 kind）——legacy 前缀桥退役。

vi.mock("@/api/subscriptions", () => ({
  cancelSubscription: vi.fn(),
  createPlan: vi.fn(),
  deletePlan: vi.fn(),
  expireSubscription: vi.fn(),
  getPlan: vi.fn(),
  getSubscription: vi.fn(),
  listPlans: vi.fn(),
  listSubscriptions: vi.fn(),
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
  vi.mocked(getPlan).mockReset();
  vi.mocked(getSubscription).mockReset();
});

describe("plansResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getPlan).mockResolvedValue({ id: "p1", code: "pro" } as never);
    expect(await plansResource.record!("p1", { projectId: "p1" })).toEqual({
      id: "p1",
      code: "pro",
    });

    vi.mocked(getPlan).mockRejectedValue(httpError(404, "gone"));
    expect(await plansResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getPlan).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(plansResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("subscriptionsResource.record", () => {
  it("读取透传；404 → null", async () => {
    vi.mocked(getSubscription).mockResolvedValue({ id: "s1", status: "active" } as never);
    expect(await subscriptionsResource.record!("s1", { projectId: "p1" })).toEqual({
      id: "s1",
      status: "active",
    });

    vi.mocked(getSubscription).mockRejectedValue(httpError(404, "gone"));
    expect(await subscriptionsResource.record!("gone", { projectId: "p1" })).toBeNull();
  });
});

describe("失效声明锚（legacyKeys 退役，缺省同资源承接详情 record）", () => {
  it("两资源 legacy 前缀桥退役；cancel/expire/remove 挂各自资源", () => {
    expect(plansResource.legacyKeys).toBeUndefined();
    expect(subscriptionsResource.legacyKeys).toBeUndefined();
    expect(declarationOf(removePlanCommand).invalidates).toBeUndefined();
    expect(declarationOf(removePlanCommand).legacyKeys).toBeUndefined();
    expect(removePlanCommand.__resource).toBe("subscription-plans");
    expect(declarationOf(cancelSubscriptionCommand).invalidates).toBeUndefined();
    expect(declarationOf(cancelSubscriptionCommand).legacyKeys).toBeUndefined();
    expect(cancelSubscriptionCommand.__resource).toBe("subscriptions");
    expect(declarationOf(expireSubscriptionCommand).invalidates).toBeUndefined();
    expect(declarationOf(expireSubscriptionCommand).legacyKeys).toBeUndefined();
    expect(expireSubscriptionCommand.__resource).toBe("subscriptions");
  });
});
