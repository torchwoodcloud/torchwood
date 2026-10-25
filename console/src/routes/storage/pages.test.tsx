import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/router/compat";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { StorageListPage } from "./pages";
import type { Bucket } from "@/api/storage";

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
vi.mock("@/api/storage", () => ({
  listBuckets: vi.fn(),
  getBucket: vi.fn(),
  createBucket: vi.fn(),
  updateBucket: vi.fn(),
  deleteBucket: vi.fn(),
  listFiles: vi.fn(),
  getFile: vi.fn(),
  uploadFile: vi.fn(),
  updateFile: vi.fn(),
  createFileToken: vi.fn(),
  getStorageUsage: vi.fn(),
  deleteFile: vi.fn(),
  downloadFile: vi.fn(),
  previewFile: vi.fn(),
  fileViewUrl: vi.fn(),
  shouldChunk: vi.fn(),
  isTooLarge: vi.fn(),
}));

import { listBuckets, deleteBucket } from "@/api/storage";

const mockList = vi.mocked(listBuckets);
const mockDelete = vi.mocked(deleteBucket);

const bucket: Bucket = {
  id: "bkt-1",
  name: "uploads",
  permissions: [],
  public: false,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

async function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: "/console/storage", element: <StorageListPage /> }]}
        initialEntries={["/console/storage"]}
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

describe("StorageListPage（列表 fleet 化）", () => {
  it("URL 即真相：默认页大小/翻页游标透传 listBuckets（page_token）", async () => {
    mockList.mockResolvedValue({ rows: [bucket], nextPageToken: "tok-2" });
    await renderPage();
    await screen.findByText("uploads");
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

  // 写路径锚（§12.3）：行删确认走 useCommand——api 收到 SKIP 且成功路径不抛。
  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockList.mockResolvedValue({ rows: [bucket], nextPageToken: undefined });
    mockDelete.mockResolvedValue(undefined);
    await renderPage();
    await screen.findByText("uploads");
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(mockDelete).toHaveBeenCalledWith("bkt-1", { __skipToast: true }),
    );
  });
});
