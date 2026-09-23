import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { OrdersListPage } from "./pages";

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/api/payments", () => ({
  listOrders: vi.fn(),
  getOrder: vi.fn(),
  refundOrder: vi.fn(),
  manualFulfillOrder: vi.fn(),
}));

import { listOrders } from "@/api/payments";

function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <OrdersListPage />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

describe("OrdersListPage", () => {
  beforeEach(() => {
    vi.mocked(listOrders).mockReset();
  });
  afterEach(() => cleanup());

  it("展示订单金额为最小单位整数", async () => {
    vi.mocked(listOrders).mockResolvedValue({
      rows: [
        {
          id: "ord-1",
          user_id: "u1",
          provider: "stripe",
          amount: "1999",
          currency: "USD",
          purpose_kind: "topup",
          status: "paid",
          created_at: "2026-08-20T00:00:00Z",
        },
      ],
    });
    renderList();
    expect(await screen.findByText("1999 USD")).toBeTruthy();
    expect(screen.getByText("ord-1")).toBeTruthy();
  });

  // 服务端时间排序契约：默认 DESC；点击表头换向必须回第一页
  //（keyset 游标与方向耦合，服务端拒异向游标）。
  it("点击创建时间表头切换排序方向并回到第一页", async () => {
    vi.mocked(listOrders).mockResolvedValue({
      rows: [
        {
          id: "ord-1",
          user_id: "u1",
          provider: "stripe",
          amount: "1999",
          currency: "USD",
          purpose_kind: "topup",
          status: "paid",
          created_at: "2026-08-20T00:00:00Z",
        },
      ],
    });
    renderList();
    await waitFor(() => expect(listOrders).toHaveBeenCalledWith({ pageSize: 20, pageToken: "", sortOrder: "DESC" }));
    // 等首屏行渲染完成（isLoading 结束、表头可交互）。
    await screen.findByText("1999 USD");

    fireEvent.click(screen.getByRole("button", { name: /创建时间/ }));
    await waitFor(() => expect(listOrders).toHaveBeenLastCalledWith({ pageSize: 20, pageToken: "", sortOrder: "ASC" }));

    fireEvent.click(screen.getByRole("button", { name: /创建时间/ }));
    await waitFor(() => expect(listOrders).toHaveBeenLastCalledWith({ pageSize: 20, pageToken: "", sortOrder: "DESC" }));
  });
});
