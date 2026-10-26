import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Outlet } from "@/router/outlet-context.tsx";
import { TestRouter } from "@/test-utils/TestRouter";
import { AxiosError } from "axios";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DocumentDetailPage, DocumentsListPage } from "./documents";
import {
  deleteDocument,
  getCollection,
  getDocument,
  listDocuments,
  updateDocument,
} from "@/api/databases";
import { canWrite } from "@/hooks/useAdminRole";

// OCC 通道方言（§12.5）：冲突 = HTTP 400 + message 前缀域码（非 409）。
function occConflict(): AxiosError {
  return new AxiosError("Request failed", "ERR_BAD_REQUEST", undefined, undefined, {
    status: 400,
    statusText: "",
    headers: {},
    config: {} as never,
    data: { error: { code: "FailedPrecondition", message: "DOCUMENT.VERSION_CONFLICT: version_mismatch" } },
  } as never);
}

vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: vi.fn(() => true),
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "p1" }) }));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/databases", () => ({
  getCollection: vi.fn(),
  listDocuments: vi.fn(),
  getDocument: vi.fn(),
  createDocument: vi.fn(),
  updateDocument: vi.fn(),
  deleteDocument: vi.fn(),
  bulkUpdateDocuments: vi.fn(),
  bulkDeleteDocuments: vi.fn(),
}));

const collection = {
  id: "posts",
  database_id: "app",
  name: "posts",
  permissions: [],
  is_system: false,
  attributes: [
    { id: "a0", key: "title", type: "string", required: true, array: false },
    { id: "a1", key: "count", type: "integer", required: false, array: false },
    { id: "a2", key: "meta", type: "json", required: false, array: false },
  ],
  indexes: [],
  created_at: "2026-09-14T00:00:00Z",
  updated_at: "2026-09-14T00:00:00Z",
};

const document = {
  id: "doc-1",
  data: { title: "hello", count: 42, meta: { a: 1 } },
  created_at: "2026-09-14T00:00:00Z",
  updated_at: "2026-09-14T00:13:22Z",
  version: 3,
};

function renderDetail() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <TestRouter
        initialEntries={["/console/databases/app/collections/posts/documents/doc-1"]}
        routes={[
          {
            path: "/console/databases/:dbId/collections/:collId/documents/:docId",
            element: <DocumentDetailPage />,
          },
        ]}
      />
    </QueryClientProvider>
  );
}

describe("DocumentDetailPage", () => {
  beforeEach(() => {
    vi.mocked(canWrite).mockReturnValue(true);
    vi.mocked(getCollection).mockReset().mockResolvedValue(collection);
    vi.mocked(getDocument).mockReset().mockResolvedValue(document);
    vi.mocked(updateDocument).mockReset();
  });
  afterEach(() => cleanup());

  it("默认只读：展示字段值，不渲染可编辑控件", async () => {
    renderDetail();
    expect(await screen.findByText("count (integer)")).toBeTruthy();
    expect(screen.getByText("42")).toBeTruthy();
    expect(screen.getByText("meta (json)")).toBeTruthy();
    expect(screen.queryByRole("spinbutton")).toBeNull();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.getByRole("button", { name: /编辑/ })).toBeTruthy();
    expect(screen.getByRole("button", { name: /删除/ })).toBeTruthy();
  });

  it("点「编辑」进入表单，「取消」丢弃草稿回到只读", async () => {
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /编辑/ }));
    const countField = screen.getByLabelText("count (integer)");
    expect(screen.getByLabelText("meta (json)")).toBeTruthy();
    expect(screen.getByLabelText("count Δ")).toBeTruthy();
    expect(screen.getByRole("button", { name: /保存/ })).toBeTruthy();

    fireEvent.change(countField, { target: { value: "7" } });
    fireEvent.click(screen.getByRole("button", { name: "取消" }));
    expect(screen.queryByRole("spinbutton")).toBeNull();
    expect(screen.getByText("42")).toBeTruthy();
    expect(screen.queryByText("7")).toBeNull();
  });

  it("无写权限时不展示编辑入口，仍为只读视图", async () => {
    vi.mocked(canWrite).mockReturnValue(false);
    renderDetail();
    expect(await screen.findByText("count (integer)")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /编辑/ })).toBeNull();
    expect(screen.queryByRole("spinbutton")).toBeNull();
  });

  it("保存 ok：data/increment/version 同一次 PATCH，成功后回只读并重建表单", async () => {
    vi.mocked(updateDocument).mockResolvedValue({
      ...document,
      data: { ...document.data, count: 7 },
      version: 4,
    });
    // invalidate 触发的 refetch 返回更新态（只读视图由查询数据派生）
    vi.mocked(getDocument)
      .mockResolvedValueOnce(document)
      .mockResolvedValue({ ...document, data: { ...document.data, count: 7 }, version: 4 });
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /编辑/ }));
    fireEvent.change(screen.getByLabelText("count (integer)"), { target: { value: "7" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() =>
      expect(vi.mocked(updateDocument)).toHaveBeenCalledWith(
        "app",
        "posts",
        "doc-1",
        { data: { title: "hello", count: 7, meta: { a: 1 } }, version: 3 },
        { __skipToast: true }
      )
    );
    // 回只读 + 用响应文档重建（count=7 呈现）
    await waitFor(() =>
      expect(screen.queryByRole("button", { name: "保存" })).toBeNull()
    );
    expect(screen.getByText("7")).toBeTruthy();
  });

  it("OCC 冲突：结果盒中文文案 + 「刷新重载」重建表单（fresh-read）", async () => {
    vi.mocked(updateDocument).mockRejectedValue(occConflict());
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /编辑/ }));
    fireEvent.change(screen.getByLabelText("title (string)"), { target: { value: "stale-edit" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    // 冲突走结果盒（非 toast）：域码中文 + 重载动作
    expect(
      await screen.findByText("文档已被其他修改抢先提交（版本不匹配），请刷新后重试")
    ).toBeTruthy();
    const reload = screen.getByRole("button", { name: "刷新重载" });

    // fresh-read 重载：getDocument 二次调用（fetchQuery），草稿按服务端态重建
    fireEvent.click(reload);
    await waitFor(() => expect(vi.mocked(getDocument)).toHaveBeenCalledTimes(2));
    await waitFor(() =>
      expect((screen.getByLabelText("title (string)") as HTMLInputElement).value).toBe("hello")
    );
    // 结果盒随 reset 清空，仍处编辑态
    expect(
      screen.queryByText("文档已被其他修改抢先提交（版本不匹配），请刷新后重试")
    ).toBeNull();
    expect(screen.getByRole("button", { name: "保存" })).toBeTruthy();
  });

  it("required 缺失：rejected.fieldErrors 字段级红字，不发请求", async () => {
    renderDetail();
    fireEvent.click(await screen.findByRole("button", { name: /编辑/ }));
    fireEvent.change(screen.getByLabelText("title (string)"), { target: { value: "" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    expect(await screen.findByText("必填字段")).toBeTruthy();
    expect(vi.mocked(updateDocument)).not.toHaveBeenCalled();
  });
});

// 列表 fleet 化（§12.9-2）URL 锚：DocumentsListPage 经 CollectionLayout 的
// Outlet context 取 dbId/collId（嵌套 scope），游标/页大小进 URL 透传
// listDocuments。
describe("DocumentsListPage（列表 fleet 化 URL 锚）", () => {
  afterEach(() => cleanup());

  function renderList() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <TestRouter
          initialEntries={["/console/databases/app/collections/posts/documents"]}
          routes={[
            {
              // CollectionLayout 替身：只提供 Outlet context（dbId/collId）
              path: "/console/databases/:dbId/collections/:collId",
              element: <Outlet context={{ dbId: "app", collId: "posts" }} />,
              children: [{ path: "/documents", element: <DocumentsListPage /> }],
            },
          ]}
        />
      </QueryClientProvider>
    );
  }

  it("URL 即真相：默认页大小/翻页游标透传 listDocuments（page_token）", async () => {
    vi.mocked(getCollection).mockReset().mockResolvedValue(collection);
    vi.mocked(listDocuments)
      .mockReset()
      .mockResolvedValue({ rows: [document], nextPageToken: "tok-2" });
    renderList();
    await act(async () => {});
    await screen.findByText("hello");
    // defaultSize 20 = 旧页默认（useServerPaging 缺省）；首次请求无 token
    expect(vi.mocked(listDocuments)).toHaveBeenLastCalledWith(
      "app",
      "posts",
      expect.objectContaining({ pageSize: 20, pageToken: undefined }),
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "下一页" }));
    await waitFor(() =>
      expect(vi.mocked(listDocuments)).toHaveBeenLastCalledWith(
        "app",
        "posts",
        expect.objectContaining({ pageToken: "tok-2" }),
      ),
    );
  });

  // §12.10-② 失效收口锚：行删除 ok 后 kit documents 集合键失效自动刷新
  // 活跃列表（list.refetch 手动桥退役——缓存失效行为断言，非 refetch 断言）。
  it("行删除 ok：kit 键失效自动刷新列表（descriptor invalidates 声明）", async () => {
    vi.mocked(getCollection).mockReset().mockResolvedValue(collection);
    vi.mocked(listDocuments)
      .mockReset()
      .mockResolvedValue({ rows: [document], nextPageToken: undefined });
    vi.mocked(deleteDocument).mockReset().mockResolvedValue(undefined);
    renderList();
    await screen.findByText("hello");
    expect(vi.mocked(listDocuments)).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(vi.mocked(deleteDocument)).toHaveBeenCalledWith(
        "app",
        "posts",
        "doc-1",
        3,
        { __skipToast: true },
      ),
    );
    // kit 集合键失效（命令 scope 与列表绑定 scope 同形相交）→ 活跃列表 refetch
    await waitFor(() => expect(vi.mocked(listDocuments)).toHaveBeenCalledTimes(2));
  });
});
