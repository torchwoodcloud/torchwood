import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WebhooksListPage, WebhookDetailPage } from "./pages";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉
//（先例：assets/pages.test.tsx）。
Element.prototype.scrollIntoView = vi.fn();

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/webhooks", () => ({
  listWebhooks: vi.fn(),
  getWebhook: vi.fn(),
  createWebhook: vi.fn(),
  updateWebhook: vi.fn(),
  deleteWebhook: vi.fn(),
  rotateWebhookSecret: vi.fn(),
  listDeliveries: vi.fn(),
  getDelivery: vi.fn(),
  redeliverDelivery: vi.fn(),
}));

import { listWebhooks, getWebhook, listDeliveries, deleteWebhook, redeliverDelivery } from "@/api/webhooks";

async function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: "/console/webhooks", element: <WebhooksListPage /> }]}
        initialEntries={["/console/webhooks"]}
      />
    </QueryClientProvider>
  );
  // RouterProvider 首提交异步：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

function renderDetail() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <TestRouter
        initialEntries={["/console/webhooks/wh-1"]}
        routes={[{ path: "/console/webhooks/:webhookId", element: <WebhookDetailPage /> }]}
      />
    </QueryClientProvider>
  );
}

describe("WebhooksListPage", () => {
  beforeEach(() => {
    vi.mocked(listWebhooks).mockReset();
  });
  afterEach(() => cleanup());

  it("渲染订阅行（URL / 状态 / 订阅数）", async () => {
    vi.mocked(listWebhooks).mockResolvedValue({
      rows: [
        {
          id: "wh-1",
          project_id: "proj-1",
          url: "https://api.example.com/hooks",
          events: ["auth.users.created", "payments.orders.paid"],
          status: "enabled",
          secret_fingerprint: "whsec_ab12cd34",
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:00:00Z",
        },
      ],
    });
    await renderList();
    expect(await screen.findByText("https://api.example.com/hooks")).toBeTruthy();
    expect(screen.getByText("wh-1")).toBeTruthy();
    expect(screen.getByText("2 events")).toBeTruthy();
    expect(screen.getByText("enabled")).toBeTruthy();
  });

  it("URL 即真相：默认页大小/翻页游标透传 listWebhooks（page_token）", async () => {
    vi.mocked(listWebhooks).mockResolvedValue({
      rows: [
        {
          id: "wh-1",
          project_id: "proj-1",
          url: "https://api.example.com/hooks",
          events: ["auth.users.created"],
          status: "enabled",
          secret_fingerprint: "whsec_ab12cd34",
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:00:00Z",
        },
      ],
      nextPageToken: "tok-2",
    });
    await renderList();
    await screen.findByText("wh-1");
    // defaultSize 20 = 旧页默认；首次请求无 token
    expect(listWebhooks).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined }),
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(listWebhooks).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
  });

  // 写路径锚（§12.3）：行删确认走 useCommand——api 收到 SKIP 且成功路径不抛。
  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    vi.mocked(listWebhooks).mockResolvedValue({
      rows: [
        {
          id: "wh-1",
          project_id: "proj-1",
          url: "https://api.example.com/hooks",
          events: ["auth.users.created"],
          status: "enabled",
          secret_fingerprint: "whsec_ab12cd34",
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:00:00Z",
        },
      ],
      nextPageToken: undefined,
    });
    vi.mocked(deleteWebhook).mockResolvedValue(undefined);
    await renderList();
    await screen.findByText("wh-1");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() =>
      expect(deleteWebhook).toHaveBeenCalledWith("wh-1", { __skipToast: true }),
    );
  });
});

describe("WebhookDetailPage", () => {
  beforeEach(() => {
    vi.mocked(getWebhook).mockReset();
    vi.mocked(listDeliveries).mockReset();
  });
  afterEach(() => cleanup());

  it("渲染详情与投递记录子视图（status/attempts/last_response）", async () => {
    vi.mocked(getWebhook).mockResolvedValue({
      id: "wh-1",
      project_id: "proj-1",
      url: "https://api.example.com/hooks",
      events: ["auth.users.created"],
      status: "enabled",
      secret_fingerprint: "whsec_ab12cd34",
      created_at: "2026-10-01T00:00:00Z",
      updated_at: "2026-10-01T00:00:00Z",
    });
    vi.mocked(listDeliveries).mockResolvedValue({
      rows: [
        {
          id: "dlv-1",
          webhook_id: "wh-1",
          event_id: "evt-1",
          event_type: "auth.users.created",
          seq: 42,
          attempts: 3,
          next_attempt_at: "2026-10-02T00:00:00Z",
          status: "failed",
          last_response_code: 500,
          last_error: "upstream 500",
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:05:00Z",
        },
      ],
    });
    renderDetail();
    expect(await screen.findByText("failed")).toBeTruthy();
    // 订阅事件卡片与投递行都会出现事件名，这里断言至少各一处。
    expect(screen.getAllByText("auth.users.created").length).toBeGreaterThanOrEqual(2);
    expect(screen.getByText(/attempts 3/)).toBeTruthy();
    expect(screen.getByText("Redeliver")).toBeTruthy();
  });

  // 「参数变即回第一页」走 useServerPaging resetKeys 机制：状态过滤变化后
  // 以空 pageToken 重新拉取（keyset 游标与过滤条件耦合）。
  it("投递状态过滤变化回第一页（resetKeys）", async () => {
    vi.mocked(getWebhook).mockResolvedValue({
      id: "wh-1",
      project_id: "proj-1",
      url: "https://api.example.com/hooks",
      events: ["auth.users.created"],
      status: "enabled",
      secret_fingerprint: "whsec_ab12cd34",
      created_at: "2026-10-01T00:00:00Z",
      updated_at: "2026-10-01T00:00:00Z",
    });
    vi.mocked(listDeliveries).mockResolvedValue({ rows: [] });
    renderDetail();
    await screen.findByText("No deliveries yet");
    // 投递列表换轨 kit collection 槽（子视图 kit 化批）：空游标归一 undefined
    //（适配器 cursor || undefined 方言，同 databases 域）。
    expect(listDeliveries).toHaveBeenLastCalledWith("wh-1", {
      pageSize: 20,
      pageToken: undefined,
      status: undefined,
    });

    // 页面上有两个 combobox（投递状态过滤 + 页大小选择）；状态过滤先渲染，
    // 取第一个。radix Select 需要 pointer 序列打开。
    const trigger = screen.getAllByRole("combobox")[0];
    fireEvent.mouseDown(trigger);
    fireEvent.click(trigger);
    await screen.findByRole("option", { name: "failed" });
    // 点击时重查活节点（打开到点击之间 react-query 状态刷写可能重建 portal 内容，
    // 旧引用会指向已脱离文档的死节点）。
    fireEvent.click(screen.getByRole("option", { name: "failed" }));
    await waitFor(() =>
      expect(listDeliveries).toHaveBeenLastCalledWith("wh-1", {
        pageSize: 20,
        pageToken: undefined,
        status: "failed",
      })
    );
  });

  // 写路径锚（§12.3）：Redeliver 走 useCommand（裸 descriptor）——api 收到
  // SKIP 且成功路径不抛。
  it("Redeliver 走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    vi.mocked(getWebhook).mockResolvedValue({
      id: "wh-1",
      project_id: "proj-1",
      url: "https://api.example.com/hooks",
      events: ["auth.users.created"],
      status: "enabled",
      secret_fingerprint: "whsec_ab12cd34",
      created_at: "2026-10-01T00:00:00Z",
      updated_at: "2026-10-01T00:00:00Z",
    });
    vi.mocked(listDeliveries).mockResolvedValue({
      rows: [
        {
          id: "dlv-1",
          webhook_id: "wh-1",
          event_id: "evt-1",
          event_type: "auth.users.created",
          seq: 42,
          attempts: 3,
          next_attempt_at: "2026-10-02T00:00:00Z",
          status: "failed",
          last_response_code: 500,
          last_error: "upstream 500",
          created_at: "2026-10-01T00:00:00Z",
          updated_at: "2026-10-01T00:05:00Z",
        },
      ],
    });
    vi.mocked(redeliverDelivery).mockResolvedValue({
      id: "dlv-1",
      webhook_id: "wh-1",
      event_id: "evt-1",
      event_type: "auth.users.created",
      seq: 42,
      attempts: 0,
      status: "pending",
      last_response_code: 0,
      last_error: "",
      created_at: "2026-10-01T00:00:00Z",
      updated_at: "2026-10-03T00:00:00Z",
    });
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: "Redeliver" }));
    await waitFor(() =>
      expect(redeliverDelivery).toHaveBeenCalledWith("wh-1", "dlv-1", { __skipToast: true }),
    );
  });
});
