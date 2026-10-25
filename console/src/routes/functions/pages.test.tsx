import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/router/compat";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { FunctionsListPage } from "./pages";
import type { FunctionItem } from "@/api/functions";

// radix Select/Dialog 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
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

import { listFunctions } from "@/api/functions";

const mockList = vi.mocked(listFunctions);

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

async function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: "/console/functions", element: <FunctionsListPage /> }]}
        initialEntries={["/console/functions"]}
      />
    </QueryClientProvider>
  );
  // RouterProvider 首提交异步：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("FunctionsListPage（列表 fleet 化）", () => {
  // v4：列表列对齐新投影——target 进、runtime/规格/池字段退。
  it("渲染 target 列且不再有 Runtime/规格列", async () => {
    mockList.mockResolvedValue({
      rows: [makeFn({})],
    });
    await renderList();
    expect(await screen.findByText("https://fn.example.com/greet")).toBeTruthy();
    expect(screen.getByText("Target")).toBeTruthy();
    expect(screen.queryByText("Runtime")).toBeNull();
    expect(screen.queryByText("规格")).toBeNull();
  });

  it("URL 即真相：默认页大小/翻页游标透传 listFunctions（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [makeFn({})], nextPageToken: "tok-2" });
    await renderList();
    await screen.findByText("greet");
    // defaultSize 20 = 旧页默认；首次请求无 token
    expect(mockList).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined }),
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
  });
});
