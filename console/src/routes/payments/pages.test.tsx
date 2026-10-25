import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/router/compat";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { OrdersListPage } from "./pages";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/payments", () => ({
  listOrders: vi.fn(),
  getOrder: vi.fn(),
  refundOrder: vi.fn(),
  manualFulfillOrder: vi.fn(),
}));

import { listOrders } from "@/api/payments";

const mockList = vi.mocked(listOrders);

const order = {
  id: "ord-1",
  user_id: "u1",
  provider: "stripe",
  amount: "1999",
  currency: "USD",
  purpose_kind: "topup",
  status: "paid",
  created_at: "2026-08-20T00:00:00Z",
};

// useCollection 的 from 锚定真实路由 id（/console/orders）——测试挂载真实路径。
async function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: "/console/orders", element: <OrdersListPage /> }]}
        initialEntries={["/console/orders"]}
      />
    </QueryClientProvider>
  );
  await act(async () => {});
}

describe("OrdersListPage", () => {
  beforeEach(() => {
    mockList.mockReset();
  });
  afterEach(() => cleanup());

  it("展示订单金额为最小单位整数", async () => {
    mockList.mockResolvedValue({ rows: [order] });
    await renderList();
    expect(await screen.findByText("1999 USD")).toBeTruthy();
    expect(screen.getByText("ord-1")).toBeTruthy();
  });

  it("URL 即真相：默认页大小/翻页游标透传 listOrders（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [order], nextPageToken: "tok-2" });
    await renderList();
    await screen.findByText("1999 USD");
    // defaultSize 20 = 旧页默认；首次请求无 token，sort_order 落服务端默认 DESC
    expect(mockList).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined, sortOrder: undefined })
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" })
      )
    );
  });

  // 服务端时间排序契约（URL sort 为真相）：无 → asc → desc → 无；
  // 换向清游标回第一页（keyset 游标与方向耦合，服务端拒异向游标）。
  it("点击创建时间表头循环排序方向（asc→desc→无）", async () => {
    mockList.mockResolvedValue({ rows: [order] });
    await renderList();
    await screen.findByText("1999 USD");

    const head = screen.getByRole("button", { name: /创建时间/ });
    fireEvent.click(head);
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ sortOrder: "ASC", pageToken: undefined })
      )
    );

    fireEvent.click(head);
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ sortOrder: "DESC" })
      )
    );

    fireEvent.click(head);
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ sortOrder: undefined })
      )
    );
  });
});
