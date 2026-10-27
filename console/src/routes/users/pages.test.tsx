import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { UsersListPage } from "./pages";
import type { User } from "@/api/users";

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
vi.mock("@/api/users", () => ({
  listUsers: vi.fn(),
  getUser: vi.fn(),
  createUser: vi.fn(),
  updateUser: vi.fn(),
  updateUserPassword: vi.fn(),
  deleteUser: vi.fn(),
  listUserSessions: vi.fn(),
  deleteUserSession: vi.fn(),
  createUserToken: vi.fn(),
}));

import { deleteUser, listUsers } from "@/api/users";

const mockList = vi.mocked(listUsers);
const mockDelete = vi.mocked(deleteUser);

const user: User = {
  id: "u-1",
  email: "ada@example.com",
  name: "Ada",
  status: "active",
  email_verified: true,
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

async function renderPage(entry = "/console/users") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[
          {
            path: "/console/users",
            element: (
              <>
                <SearchProbe />
                <UsersListPage />
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

describe("UsersListPage（列表 fleet 化：URL 即真相）", () => {
  it("默认页大小/翻页游标透传 listUsers（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [user], nextPageToken: "tok-2" });
    await renderPage();
    await screen.findByText("ada@example.com");
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

  it("服务端过滤进 URL 谓词并映射 ParseUserList DSL（ID 消毒/状态/时间范围）", async () => {
    mockList.mockResolvedValue({ rows: [user] });
    await renderPage();
    await screen.findByText("ada@example.com");

    // ID：受控输入按「查询」提交；URL 存原值，消毒在适配器（引号剥离）
    fireEvent.change(screen.getByLabelText("User ID"), { target: { value: 'u "1"' } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ queries: ['equal("id","u1")'] }),
      ),
    );
    expect(search().filter).toEqual(['id:eq:u "1"']);

    // 状态：Select 即时应用（setPredicate = replace + 清游标回第一页）
    fireEvent.click(screen.getByRole("combobox", { name: "Status" }));
    fireEvent.click(await screen.findByRole("option", { name: "blocked" }));
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({
          queries: ['equal("id","u1")', 'equal("status","blocked")'],
        }),
      ),
    );

    // 创建时间范围：datetime-local 墙钟按管理员时区（UTC）换算绝对时刻 → gt
    fireEvent.change(screen.getByLabelText("Created from"), {
      target: { value: "2026-01-01T00:00" },
    });
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({
          queries: [
            'equal("id","u1")',
            'equal("status","blocked")',
            'greaterThan("created_at","2026-01-01T00:00:00.000Z")',
          ],
        }),
      ),
    );
    expect(search().filter).toContain("created_after:eq:2026-01-01T00:00:00.000Z");
  });

  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockList.mockResolvedValue({ rows: [user], nextPageToken: undefined });
    mockDelete.mockResolvedValue(undefined);
    await renderPage();
    await screen.findByText("ada@example.com");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() =>
      expect(mockDelete).toHaveBeenCalledWith("u-1", { __skipToast: true }),
    );
  });
});
