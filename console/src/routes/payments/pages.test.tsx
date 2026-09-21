import { cleanup, render, screen } from "@testing-library/react";
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
});
