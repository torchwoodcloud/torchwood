import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { FunctionsListPage } from "./pages";

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/api/functions", () => ({
  listFunctions: vi.fn(),
  getFunction: vi.fn(),
  createFunction: vi.fn(),
  updateFunction: vi.fn(),
  deleteFunction: vi.fn(),
  rotateFunctionSecret: vi.fn(),
  invokeFunction: vi.fn(),
  listInvocations: vi.fn(),
  setFunctionScopes: vi.fn(),
}));

import { listFunctions, type FunctionItem } from "@/api/functions";

function makeFn(overrides: Partial<FunctionItem>): FunctionItem {
  return {
    id: "fn-1",
    project_id: "proj-1",
    name: "greet",
    timeout_seconds: 15,
    enabled: true,
    declared_scopes: [],
    client_callable: false,
    client_anonymous_allowed: false,
    client_per_user_limit: 0,
    client_limit_window: "day",
    target: {
      url: "https://fn.example.com/greet",
      secret_fingerprint: "twfnsec_ab12cd34",
    },
    created_at: "2026-10-01T00:00:00Z",
    updated_at: "2026-10-01T00:00:00Z",
    ...overrides,
  };
}

function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <FunctionsListPage />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

describe("FunctionsListPage", () => {
  beforeEach(() => {
    vi.mocked(listFunctions).mockReset();
  });
  afterEach(() => cleanup());

  // v4：列表列对齐新投影——target 进、runtime/规格/池字段退。
  it("渲染 target 列且不再有 Runtime/规格列", async () => {
    vi.mocked(listFunctions).mockResolvedValue({
      rows: [makeFn({})],
    });
    renderList();
    expect(await screen.findByText("https://fn.example.com/greet")).toBeTruthy();
    expect(screen.getByText("Target")).toBeTruthy();
    expect(screen.queryByText("Runtime")).toBeNull();
    expect(screen.queryByText("规格")).toBeNull();
  });
});
