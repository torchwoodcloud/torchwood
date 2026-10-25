import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Outlet, TestRouter } from "@/router/compat";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DatabasesListPage, DatabaseDetailPage } from "./databases";
import { CollectionNewPage, CollectionDetailPage } from "./collections";
import type { Collection, Database } from "@/api/databases";

// 列表 fleet 化（§12.9-2）URL 锚：默认页大小/翻页游标透传 fetch 函数
//（URL 即真相；嵌套 from = 路由 id）。写路径命令化锚：行删/建/集合 schema
// 编辑器写路径走 useCommand（提交形收敛 + __skipToast）。写函数全 mock。

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "p1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/databases", () => ({
  listDatabases: vi.fn(),
  getDatabase: vi.fn(),
  createDatabase: vi.fn(),
  deleteDatabase: vi.fn(),
  listCollections: vi.fn(),
  getCollection: vi.fn(),
  createCollection: vi.fn(),
  updateCollection: vi.fn(),
  deleteCollection: vi.fn(),
  createAttribute: vi.fn(),
  createIndex: vi.fn(),
  deleteAttribute: vi.fn(),
  deleteIndex: vi.fn(),
}));

import {
  createCollection,
  deleteAttribute,
  deleteDatabase,
  getCollection,
  getDatabase,
  listCollections,
  listDatabases,
} from "@/api/databases";

const mockListDatabases = vi.mocked(listDatabases);
const mockGetDatabase = vi.mocked(getDatabase);
const mockListCollections = vi.mocked(listCollections);
const mockDeleteDatabase = vi.mocked(deleteDatabase);
const mockCreateCollection = vi.mocked(createCollection);
const mockGetCollection = vi.mocked(getCollection);
const mockDeleteAttribute = vi.mocked(deleteAttribute);

const db: Database = {
  id: "app",
  name: "Production DB",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const coll: Collection = {
  id: "posts",
  database_id: "app",
  name: "文章",
  permissions: [],
  is_system: false,
  attributes: [],
  indexes: [],
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

async function renderPage(
  routePath: string,
  element: React.ReactElement,
  entry = routePath,
  // 额外路由：命令 onSettled 里 navigate 的目标必须真实注册（api-keys 测试
  // 同款——not-found rejection 落点随并行时序漂移）。
  extraRoutes: { path: string; element: React.ReactElement }[] = [],
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter routes={[{ path: routePath, element }, ...extraRoutes]} initialEntries={[entry]} />
    </QueryClientProvider>,
  );
  // RouterProvider 首提交异步：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("DatabasesListPage（列表 fleet 化 URL 锚）", () => {
  it("URL 即真相：默认页大小/翻页游标透传 listDatabases（page_token）", async () => {
    mockListDatabases.mockResolvedValue({ rows: [db], nextPageToken: "tok-2" });
    await renderPage("/console/databases", <DatabasesListPage />);
    await screen.findByText("Production DB");
    // defaultSize 20 = 旧页默认（useServerPaging 缺省）；首次请求无 token
    expect(mockListDatabases).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined }),
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() =>
      expect(mockListDatabases).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
  });

  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockListDatabases.mockResolvedValue({ rows: [db], nextPageToken: undefined });
    mockDeleteDatabase.mockResolvedValue(undefined);
    await renderPage("/console/databases", <DatabasesListPage />);
    await screen.findByText("Production DB");
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(mockDeleteDatabase).toHaveBeenCalledWith("app", { __skipToast: true }),
    );
  });
});

describe("CollectionNewPage（写路径命令化）", () => {
  it("创建：ID 留空从名称派生 + 权限空不发送 + __skipToast", async () => {
    mockGetDatabase.mockResolvedValue(db);
    mockCreateCollection.mockResolvedValue(coll);
    await renderPage(
      "/console/databases/$dbId/collections/new",
      <CollectionNewPage />,
      "/console/databases/app/collections/new",
      // ok 后 navigate 到集合详情——目标路由注册
      [{ path: "/console/databases/$dbId/collections/$collId", element: <div /> }],
    );
    fireEvent.change(await screen.findByLabelText("名称"), { target: { value: "My Posts" } });
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    await waitFor(() =>
      expect(mockCreateCollection).toHaveBeenCalledWith(
        "app",
        { id: "my_posts", name: "My Posts", permissions: undefined },
        { __skipToast: true },
      ),
    );
  });
});

describe("DatabaseDetailPage（collections 列表嵌套 scope URL 锚）", () => {
  it("嵌套 from：dbId 取自路径参数，默认页大小/游标透传 listCollections", async () => {
    mockGetDatabase.mockResolvedValue(db);
    mockListCollections.mockResolvedValue({ rows: [coll], nextPageToken: "tok-2" });
    await renderPage(
      "/console/databases/:dbId",
      <DatabaseDetailPage />,
      "/console/databases/app",
    );
    await screen.findByText("文章");
    expect(mockListCollections).toHaveBeenLastCalledWith(
      "app",
      expect.objectContaining({ pageSize: 20, pageToken: undefined }),
    );
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() =>
      expect(mockListCollections).toHaveBeenLastCalledWith(
        "app",
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
  });
});

describe("CollectionDetailPage（写路径命令化：schema 编辑器）", () => {
  // 写路径锚（§12.3）：属性删除走 useCommand（挂 collections 资源）——api
  // 收到 SKIP 且成功路径不抛。Outlet context 提供 dbId/collId（嵌套路由形态）。
  it("属性删除走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockGetCollection.mockResolvedValue({
      ...coll,
      attributes: [
        { id: "attr-1", key: "title", type: "string", required: false, array: false },
      ],
      indexes: [],
    });
    mockDeleteAttribute.mockResolvedValue(undefined);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          routes={[
            {
              path: "/console/databases/:dbId/collections/:collId",
              element: <Outlet context={{ dbId: "app", collId: "posts" }} />,
              children: [{ path: "/", element: <CollectionDetailPage /> }],
            },
          ]}
          initialEntries={["/console/databases/app/collections/posts"]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
    fireEvent.click(await screen.findByRole("button", { name: "删除" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(mockDeleteAttribute).toHaveBeenCalledWith("app", "posts", "title", {
        __skipToast: true,
      }),
    );
  });
});
