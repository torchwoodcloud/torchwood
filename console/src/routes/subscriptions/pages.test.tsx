import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { PlansListPage } from "./pages";

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/api/subscriptions", () => ({
  listPlans: vi.fn(),
  getPlan: vi.fn(),
  createPlan: vi.fn(),
  deletePlan: vi.fn(),
  listSubscriptions: vi.fn(),
  getSubscription: vi.fn(),
  cancelSubscription: vi.fn(),
  expireSubscription: vi.fn(),
}));

import { listPlans } from "@/api/subscriptions";

describe("PlansListPage", () => {
  beforeEach(() => {
    vi.mocked(listPlans).mockReset();
  });
  afterEach(() => cleanup());

  it("展示计划金额为最小单位", async () => {
    vi.mocked(listPlans).mockResolvedValue({
      rows: [
        {
          id: "p1",
          code: "pro",
          name: "Pro",
          amount: "999",
          currency: "USD",
          interval: "month",
          status: "active",
        },
      ],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter>
          <PlansListPage />
        </MemoryRouter>
      </QueryClientProvider>
    );
    expect(await screen.findByText("999 USD")).toBeTruthy();
    expect(screen.getByText("pro")).toBeTruthy();
  });
});
