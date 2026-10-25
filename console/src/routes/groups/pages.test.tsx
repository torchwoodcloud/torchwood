import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter, useRouterState } from "@/router/compat";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { GroupsListPage } from "./pages";
import type { Group } from "@/api/groups";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
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
vi.mock("@/api/groups", () => ({
  listGroups: vi.fn(),
  getGroup: vi.fn(),
  createGroup: vi.fn(),
  deleteGroup: vi.fn(),
  getGroupPrefs: vi.fn(),
  updateGroupPrefs: vi.fn(),
  listMemberships: vi.fn(),
  createMembership: vi.fn(),
  updateMembership: vi.fn(),
  updateMembershipStatus: vi.fn(),
  deleteMembership: vi.fn(),
}));

import { listGroups } from "@/api/groups";

const mockList = vi.mocked(listGroups);

const group: Group = {
  id: "g-1",
  name: "Engineering",
  total: 3,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

// TanStack 已解析的 search 对象即真相（数组值在 URL 是 JSON 编码串，直接
// 读 location.search 会拿到 %5B…%5D 不可断言）。
function SearchProbe() {
  const search = (useRouterState as unknown as (o: unknown) => Record<string, unknown>)({
    select: (s: { location: { search: object } }) => s.location.search,
  });
  return <div data-testid="loc">{JSON.stringify(search)}</div>;
}

async function renderPage(entry = "/console/groups") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[
          {
            path: "/console/groups",
            element: (
              <>
                <SearchProbe />
                <GroupsListPage />
              </>
            ),
          },
        ]}
        initialEntries={[entry]}
      />
    </QueryClientProvider>,
  );
  // RouterProvider 首提交是异步的：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

const search = () => JSON.parse(screen.getByTestId("loc").textContent!) as Record<string, unknown>;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("GroupsListPage（列表 fleet 化：URL 即真相）", () => {
  it("默认页大小/翻页游标透传 listGroups（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [group], nextPageToken: "tok-2" });
    await renderPage();
    await screen.findByText("Engineering");
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
    expect(search().cursor).toBe("tok-2");
  });
});
