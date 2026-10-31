import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { getWebhook, listDeliveries } from "@/api/webhooks";
import {
  redeliverDeliveryCommand,
  webhookDeliveriesResource,
  webhooksResource,
} from "@/kit/webhooks";

// 详情/子视图读槽（子视图 kit 化批）与失效声明锚：record 404 → null 空态
// 直出（recordOrNull 手写方言）；collection 守卫静默空段 + toApiError 上抛。

// mock 工厂必须列出 kit/webhooks.ts import 的该模块全部具名导出。
vi.mock("@/api/webhooks", () => ({
  createWebhook: vi.fn(),
  deleteWebhook: vi.fn(),
  getWebhook: vi.fn(),
  listDeliveries: vi.fn(),
  listWebhooks: vi.fn(),
  redeliverDelivery: vi.fn(),
  rotateWebhookSecret: vi.fn(),
  updateWebhook: vi.fn(),
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
  vi.mocked(getWebhook).mockReset();
  vi.mocked(listDeliveries).mockReset();
});

describe("webhooksResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getWebhook).mockResolvedValue({ id: "wh-1" } as never);
    expect(await webhooksResource.record!("wh-1", { projectId: "p1" })).toEqual({ id: "wh-1" });
    expect(vi.mocked(getWebhook)).toHaveBeenCalledWith("wh-1");

    vi.mocked(getWebhook).mockRejectedValue(httpError(404, "gone"));
    expect(await webhooksResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getWebhook).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(webhooksResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("webhookDeliveriesResource.collection", () => {
  const q = { window: { size: 20, cursor: "tok-2" } };

  it("ctx 缺参静默空段（不触达 api）", async () => {
    expect(await webhookDeliveriesResource.collection!(q, { projectId: "p1" })).toEqual({
      rows: [],
    });
    expect(await webhookDeliveriesResource.collection!(q, { webhookId: "wh-1" })).toEqual({
      rows: [],
    });
    expect(vi.mocked(listDeliveries)).not.toHaveBeenCalled();
  });

  it("window/ctx 透传：cursor→pageToken、webhookId、status 旁路键；nextCursor 投影", async () => {
    vi.mocked(listDeliveries).mockResolvedValue({
      rows: [{ id: "dlv-1" } as never],
      nextPageToken: "tok-3",
    });
    const seg = await webhookDeliveriesResource.collection!(q, {
      projectId: "p1",
      webhookId: "wh-1",
      status: "failed",
    });
    expect(seg).toEqual({ rows: [{ id: "dlv-1" }], nextCursor: "tok-3" });
    expect(vi.mocked(listDeliveries)).toHaveBeenCalledWith("wh-1", {
      pageSize: 20,
      pageToken: "tok-2",
      status: "failed",
    });
  });

  it("失败经 toApiError 如实上抛", async () => {
    vi.mocked(listDeliveries).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(
      webhookDeliveriesResource.collection!(q, { projectId: "p1", webhookId: "wh-1" })
    ).rejects.toEqual({ kind: "server", status: 500, message: "boom" });
  });
});

describe("失效声明锚（存量前缀桥退役，子视图 kit 化批）", () => {
  it("redeliver 只打 webhook-deliveries（allScopes——命令侧 webhookId 投影必 miss）；不失效 webhooks", () => {
    expect(redeliverDeliveryCommand.invalidates).toEqual([
      { resource: "webhook-deliveries", allScopes: true },
    ]);
    // update/rotate/remove 无显式 invalidates——缺省同资源失效自动命中详情
    // record 键（全 kind），不需要（也不得有）声明覆盖。
    for (const verb of ["update", "rotateSecret", "remove"] as const) {
      expect("invalidates" in webhooksResource.commands[verb]).toBe(false);
    }
    // 资源级 legacyKeys 全量退役
    expect(webhooksResource.legacyKeys).toBeUndefined();
    expect(redeliverDeliveryCommand.legacyKeys).toBeUndefined();
  });
});
