import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { DocumentDetailPage } from "./documents";
import { getCollection, getDocument } from "@/api/databases";
import { canWrite } from "@/hooks/useAdminRole";

vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: vi.fn(() => true),
  isPlatformAdmin: () => true,
}));
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
    { id: "a1", key: "count", type: "integer", required: false, array: false },
    { id: "a2", key: "meta", type: "json", required: false, array: false },
  ],
  indexes: [],
  created_at: "2026-09-14T00:00:00Z",
  updated_at: "2026-09-14T00:00:00Z",
};

const document = {
  id: "doc-1",
  data: { count: 42, meta: { a: 1 } },
  created_at: "2026-09-14T00:00:00Z",
  updated_at: "2026-09-14T00:13:22Z",
  version: 3,
};

function renderDetail() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/console/databases/app/collections/posts/documents/doc-1"]}>
        <Routes>
          <Route
            path="/console/databases/:dbId/collections/:collId/documents/:docId"
            element={<DocumentDetailPage />}
          />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

describe("DocumentDetailPage", () => {
  beforeEach(() => {
    vi.mocked(canWrite).mockReturnValue(true);
    vi.mocked(getCollection).mockResolvedValue(collection);
    vi.mocked(getDocument).mockResolvedValue(document);
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
});
