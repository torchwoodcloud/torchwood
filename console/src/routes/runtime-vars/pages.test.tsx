import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { VarSetDetailPage, VarSetsListPage } from "./pages";
import type { RuntimeVar, VarSet } from "@/api/runtimeVars";

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

import {
  createVarSet,
  deleteVar,
  getVarSet,
  listVarSets,
  listVars,
  listVersions,
} from "@/api/runtimeVars";

const mockList = vi.mocked(listVarSets);
const mockCreateVarSet = vi.mocked(createVarSet);
const mockDeleteVar = vi.mocked(deleteVar);
const mockGetVarSet = vi.mocked(getVarSet);
const mockListVars = vi.mocked(listVars);
const mockListVersions = vi.mocked(listVersions);

const varSet: VarSet = {
  id: "feature_flags",
  var_set_id: "feature_flags",
  visibility: "VAR_SET_VISIBILITY_PUBLIC",
  description: "Client feature flags and thresholds",
  etag: "3:1",
  revision: 3,
  var_count: 12,
  total_bytes: 2048,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

async function renderPage(
  routePath = "/console/runtime-vars",
  element: React.ReactElement = <VarSetsListPage />,
  entry = routePath,
  // 额外路由：命令 onSettled 里 navigate 的目标必须真实注册（api-keys 测试
  // 同款——not-found rejection 落点随并行时序漂移）。
  extraRoutes: { path: string; element: React.ReactElement }[] = [],
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: routePath, element }, ...extraRoutes]}
        initialEntries={[entry]}
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
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(mockList).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
  });

  it("新建变量集：对话框表单提交走 useCommand（提交形 + __skipToast）", async () => {
    mockList.mockResolvedValue({ rows: [] });
    mockCreateVarSet.mockResolvedValue(varSet);
    await renderPage();
    // 空列表时工具栏与空态各有一个「新建变量集」钮（开门竞赛与空态渲染
    // 时序相关）——取第一个（工具栏钮，两者都开同一对话框）。
    fireEvent.click(screen.getAllByRole("button", { name: "New variable set" })[0]);
    fireEvent.change(await screen.findByLabelText("Set ID"), {
      target: { value: "feature_flags" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(mockCreateVarSet).toHaveBeenCalledWith(
        {
          var_set_id: "feature_flags",
          visibility: "VAR_SET_VISIBILITY_PUBLIC",
          description: "",
        },
        { __skipToast: true },
      ),
    );
  });
});

describe("VarSetDetailPage（写路径命令化）", () => {
  const aVar: RuntimeVar = {
    var_set_id: "feature_flags",
    key: "flag_a",
    value: { bool_value: true },
    description: "",
  };

  it("删除变量：确认后 deleteVar 带 __skipToast", async () => {
    mockGetVarSet.mockResolvedValue(varSet);
    mockListVars.mockResolvedValue([aVar]);
    mockListVersions.mockResolvedValue([]);
    await renderPage(
      "/console/runtime-vars/$varSetId",
      <VarSetDetailPage />,
      "/console/runtime-vars/feature_flags",
    );
    // 行内删除钮（title 定位，icon-only）
    fireEvent.click(await screen.findByTitle("Delete"));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() =>
      expect(mockDeleteVar).toHaveBeenCalledWith("feature_flags", "flag_a", {
        __skipToast: true,
      }),
    );
  });
});
