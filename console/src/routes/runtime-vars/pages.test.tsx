import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/router/compat";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { VarSetsListPage } from "./pages";
import type { VarSet } from "@/api/runtimeVars";

// radix Select/Dialog 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "p1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/runtimeVars", () => ({
  createVar: vi.fn(),
  createVarSet: vi.fn(),
  deleteVar: vi.fn(),
  deleteVarSet: vi.fn(),
  getVarSet: vi.fn(),
  getVersion: vi.fn(),
  listVarSets: vi.fn(),
  listVars: vi.fn(),
  listVersions: vi.fn(),
  rollback: vi.fn(),
  updateVar: vi.fn(),
  updateVarSet: vi.fn(),
}));

import { listVarSets } from "@/api/runtimeVars";

const mockList = vi.mocked(listVarSets);

const varSet: VarSet = {
  id: "feature_flags",
  var_set_id: "feature_flags",
  visibility: "VAR_SET_VISIBILITY_PUBLIC",
  description: "客户端功能开关与阈值",
  etag: "3:1",
  revision: 3,
  var_count: 12,
  total_bytes: 2048,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

async function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: "/console/runtime-vars", element: <VarSetsListPage /> }]}
        initialEntries={["/console/runtime-vars"]}
      />
    </QueryClientProvider>,
  );
  // RouterProvider 首提交异步：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("VarSetsListPage（列表 fleet 化）", () => {
  it("URL 即真相：默认页大小/翻页游标透传 listVarSets（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [varSet], nextPageToken: "tok-2" });
    await renderPage();
    await screen.findByText("feature_flags");
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
