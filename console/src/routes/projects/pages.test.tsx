import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { ProjectsListPage, ProjectNewPage } from "./pages";
import type { Project } from "@/api/projects";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

// projects 是平台级域：不 mock useAuth（页面本身不消费项目作用域）。
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: vi.fn() }));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/projects", () => ({
  listProjects: vi.fn(),
  getProject: vi.fn(),
  createProject: vi.fn(),
}));

import { listProjects, createProject } from "@/api/projects";

const mockList = vi.mocked(listProjects);

const project: Project = {
  id: "shop",
  name: "My Shop",
  status: "active",
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

async function renderPage(entry = "/console/projects") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[
          {
            path: "/console/projects",
            element: (
              <>
                <SearchProbe />
                <ProjectsListPage />
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

describe("ProjectsListPage（列表 fleet 化：URL 即真相）", () => {
  it("默认页大小/翻页游标透传 listProjects（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [project], nextPageToken: "tok-2" });
    await renderPage();
    await screen.findByText("My Shop");
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
});

describe("ProjectNewPage（写路径命令化）", () => {
  // 写路径锚（§12.3）：创建走 useCommand（挂 projects 资源）——description 空
  // 不发送（run 内收敛）+ api 收到 SKIP。
  it("创建走 useCommand（description 空不发送 + skipToast）", async () => {
    vi.mocked(createProject).mockResolvedValue(project);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          routes={[
            { path: "/console/projects/new", element: <ProjectNewPage /> },
            // ok 后 navigate 到项目详情——目标路由注册
            { path: "/console/projects/:id", element: <div /> },
          ]}
          initialEntries={["/console/projects/new"]}
        />
      </QueryClientProvider>,
    );
    await act(async () => {});
    fireEvent.change(await screen.findByLabelText("ID"), { target: { value: "shop" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "My Shop" } });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(createProject).toHaveBeenCalledWith(
        { id: "shop", name: "My Shop", description: undefined },
        { __skipToast: true },
      ),
    );
  });
});
