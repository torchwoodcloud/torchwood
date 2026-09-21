import type { ReactNode } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AssetDefsListPage, AssetDefDetailPage, UserAssetsPage } from "./pages";

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/api/assets", () => ({
  listAssetDefs: vi.fn(),
  getAssetDef: vi.fn(),
  createAssetDef: vi.fn(),
  updateAssetDef: vi.fn(),
  deleteAssetDef: vi.fn(),
  listUserAssets: vi.fn(),
  listUserLedger: vi.fn(),
  listDefHolders: vi.fn(),
}));

import { getAssetDef, listAssetDefs, listDefHolders, listUserAssets, listUserLedger } from "@/api/assets";

function wrap(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>
  );
}

describe("AssetDefsListPage", () => {
  beforeEach(() => {
    vi.mocked(listAssetDefs).mockReset();
  });
  afterEach(() => cleanup());

  it("列出定义且无 Grant/Consume/Transfer 写入口", async () => {
    vi.mocked(listAssetDefs).mockResolvedValue({
      rows: [
        {
          id: "d1",
          code: "gold",
          name: "金币",
          class: "currency",
          decimals: 0,
          status: "active",
          created_at: "2026-08-20T00:00:00Z",
        },
      ],
    });
    wrap(<AssetDefsListPage />);
    expect(await screen.findByText("gold")).toBeTruthy();
    expect(screen.queryByText(/Grant/i)).toBeNull();
    expect(screen.queryByText(/Consume/i)).toBeNull();
    expect(screen.queryByText(/Transfer/i)).toBeNull();
    expect(screen.getByText("查询用户资产")).toBeTruthy();
  });
});

describe("UserAssetsPage", () => {
  beforeEach(() => {
    vi.mocked(listUserAssets).mockReset();
    vi.mocked(listUserLedger).mockReset();
  });
  afterEach(() => cleanup());

  it("只读查询表单，无资产写按钮", () => {
    wrap(<UserAssetsPage />);
    expect(screen.getByLabelText("用户 ID")).toBeTruthy();
    expect(screen.getByText("查询")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /grant/i })).toBeNull();
    expect(screen.queryByRole("button", { name: /consume/i })).toBeNull();
  });

  it("URL owner 参数直达查询（用户详情页跳入）", async () => {
    vi.mocked(listUserAssets).mockResolvedValue({
      rows: [
        { id: "h1", def_id: "d1", def_code: "gold", class: "currency", quantity: "100" },
      ],
    });
    vi.mocked(listUserLedger).mockResolvedValue({ rows: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={["/console/assets/users?owner=u1"]}>
          <UserAssetsPage />
        </MemoryRouter>
      </QueryClientProvider>
    );
    expect(await screen.findByText(/gold/)).toBeTruthy();
    expect(listUserAssets).toHaveBeenCalledWith("u1", { pageSize: 20, pageToken: "" });
    expect(listUserLedger).toHaveBeenCalledWith("u1", { pageSize: 20, pageToken: "" });
    expect((screen.getByLabelText("用户 ID") as HTMLInputElement).value).toBe("u1");
  });
});

describe("AssetDefDetailPage 用户持有列表", () => {
  beforeEach(() => {
    vi.mocked(getAssetDef).mockReset();
    vi.mocked(listDefHolders).mockReset();
  });
  afterEach(() => cleanup());

  function renderDetail() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={["/console/assets/defs/d1"]}>
          <Routes>
            <Route path="/console/assets/defs/:id" element={<AssetDefDetailPage />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>
    );
  }

  it("详情下方列出该定义的持有（UserID / 数量）并对接服务端分页", async () => {
    vi.mocked(getAssetDef).mockResolvedValue({
      id: "d1",
      code: "gold",
      name: "金币",
      class: "currency",
      decimals: 0,
      status: "active",
    });
    vi.mocked(listDefHolders).mockResolvedValue({
      rows: [
        { id: "h1", owner_id: "u1", def_id: "d1", def_code: "gold", class: "currency", quantity: "100" },
        { id: "h2", owner_id: "u2", def_id: "d1", def_code: "gold", class: "currency", quantity: "5" },
      ],
      nextPageToken: "tok-1",
    });
    renderDetail();
    expect(await screen.findByText("用户持有（只读）")).toBeTruthy();
    expect(screen.getByText("u1")).toBeTruthy();
    expect(screen.getByText("u2")).toBeTruthy();
    expect(listDefHolders).toHaveBeenCalledWith("d1", {
      ownerId: undefined,
      pageSize: 20,
      pageToken: "",
    });
  });

  it("UserID 过滤提交后带 owner_id 重查", async () => {
    vi.mocked(getAssetDef).mockResolvedValue({
      id: "d1",
      code: "gold",
      name: "金币",
      class: "currency",
      decimals: 0,
      status: "active",
    });
    vi.mocked(listDefHolders).mockResolvedValue({ rows: [] });
    renderDetail();
    await screen.findByText("用户持有（只读）");
    fireEvent.change(screen.getByLabelText("用户 ID"), { target: { value: "u2" } });
    fireEvent.click(screen.getByRole("button", { name: "查询" }));
    await vi.waitFor(() => {
      expect(listDefHolders).toHaveBeenLastCalledWith("d1", {
        ownerId: "u2",
        pageSize: 20,
        pageToken: "",
      });
    });
  });
});
