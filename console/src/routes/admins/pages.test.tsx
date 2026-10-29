import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { AdminsListPage } from "./pages";
import type { Admin } from "@/api/admins";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

// admins 是平台级域：不 mock useAuth（页面本身不消费项目作用域）。
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/admins", () => ({
  ADMIN_ROLES: ["owner", "admin", "member", "viewer"],
  listAdmins: vi.fn(),
  getCurrentAdmin: vi.fn(),
  createAdmin: vi.fn(),
  updateAdmin: vi.fn(),
  deleteAdmin: vi.fn(),
}));

import { deleteAdmin, getCurrentAdmin, listAdmins } from "@/api/admins";

const mockList = vi.mocked(listAdmins);
const mockMe = vi.mocked(getCurrentAdmin);
const mockDelete = vi.mocked(deleteAdmin);

const me: Admin = { id: "a-1", email: "me@example.com", role: "owner" };
const other: Admin = {
  id: "a-2",
  email: "ops@example.com",
  role: "member",
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

async function renderPage(entry = "/console/admins") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[
          {
            path: "/console/admins",
            element: (
              <>
                <SearchProbe />
                <AdminsListPage />
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

describe("AdminsListPage（列表 fleet 化：URL 即真相）", () => {
  it("默认页大小/翻页游标透传 listAdmins（page_token）", async () => {
    mockMe.mockResolvedValue(me);
    mockList.mockResolvedValue({ rows: [me, other], nextPageToken: "tok-2" });
    await renderPage();
    await screen.findByText("ops@example.com");
    // defaultSize 20 = 旧页默认；首次请求无 token
    expect(mockList).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined }),
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
    expect(search().cursor).toBe("tok-2");
  });

  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockMe.mockResolvedValue(me);
    mockList.mockResolvedValue({ rows: [me, other], nextPageToken: undefined });
    mockDelete.mockResolvedValue(undefined);
    await renderPage();
    await screen.findByText("ops@example.com");
    // 当前行（me）无删除钮；仅 other 行可删
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() =>
      expect(mockDelete).toHaveBeenCalledWith("a-2", { __skipToast: true }),
    );
  });

  it("行展示 ID；Current account 标记在 email 后；详情弹层全角色可看", async () => {
    mockMe.mockResolvedValue(me);
    mockList.mockResolvedValue({ rows: [me, other], nextPageToken: undefined });
    await renderPage();
    await screen.findByText("ops@example.com");

    // ID 列（mono 截断样式与 users 列表一致）
    screen.getByText("a-1");
    screen.getByText("a-2");

    // Current account 标记挂在 email 后（不在 Actions 列）；self 行只有详情钮
    const meRow = screen.getByText("me@example.com").closest("tr")!;
    within(meRow).getByText("Current account");
    expect(within(meRow).queryByRole("button", { name: "Edit" })).toBeNull();
    expect(within(meRow).queryByRole("button", { name: "Delete" })).toBeNull();

    // other 行（owner）有编辑 + 删除；详情按钮每行都有
    const otherRow = screen.getByText("ops@example.com").closest("tr")!;
    within(otherRow).getByRole("button", { name: "Edit" });
    within(otherRow).getByRole("button", { name: "Delete" });
    expect(screen.getAllByRole("button", { name: "Details" })).toHaveLength(2);

    // 详情弹层：只读字段全量（me 无 created_at → "—"；无时区偏好 → 跟随浏览器）
    fireEvent.click(within(meRow).getByRole("button", { name: "Details" }));
    const dialog = await screen.findByRole("dialog");
    within(dialog).getByText("a-1");
    within(dialog).getByText("Follows browser");
    // me 无 created_at/updated_at → Created/Updated 均显示 "—"
    expect(within(dialog).getAllByText("—")).toHaveLength(2);
  });
});
