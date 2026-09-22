import type { ReactNode } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { AssetDefsListPage, AssetDefDetailPage, UserAssetsPage } from "./pages";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/api/admins", () => ({ getCurrentAdmin: vi.fn().mockRejectedValue(new Error("no session")) }));
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

  it("URL owner 参数直达查询，持有以表格呈现并带项目作用域提示", async () => {
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
    expect(await screen.findByText("gold")).toBeTruthy();
    expect(screen.getByText("100")).toBeTruthy();
    expect(screen.getByText("查询项目：")).toBeTruthy();
    expect(screen.getByText("proj-1")).toBeTruthy();
    expect(listUserAssets).toHaveBeenCalledWith("u1", { pageSize: 20, pageToken: "" });
    expect(listUserLedger).toHaveBeenCalledWith("u1", { pageSize: 20, pageToken: "" });
    expect((screen.getByLabelText("用户 ID") as HTMLInputElement).value).toBe("u1");
  });

  it("流水以表格呈现：时间 / 类型 / 资产 / 变动 / 变动后余额", async () => {
    vi.mocked(listUserAssets).mockResolvedValue({ rows: [] });
    vi.mocked(listUserLedger).mockResolvedValue({
      rows: [
        {
          id: "e1",
          def_id: "d1",
          def_code: "jade",
          kind: "grant",
          delta: "6",
          quantity_after: "19",
          created_at: "2026-09-20T15:38:50Z",
        },
        {
          id: "e2",
          def_id: "d1",
          def_code: "jade",
          kind: "consume",
          delta: "-2",
          quantity_after: "17",
          created_at: "2026-09-20T15:42:18Z",
        },
      ],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={["/console/assets/users?owner=u1"]}>
          <UserAssetsPage />
        </MemoryRouter>
      </QueryClientProvider>
    );
    // radix Tabs 内容懒渲染：先切到流水 tab 再断言（radix Trigger 依赖
    // pointer 序列，jsdom 里需先 mouseDown 再 click 才会激活）。
    const ledgerTab = screen.getByRole("tab", { name: "流水" });
    fireEvent.mouseDown(ledgerTab);
    fireEvent.click(ledgerTab);
    expect(await screen.findByText("发放")).toBeTruthy();
    expect(screen.getByText("消耗")).toBeTruthy();
    expect(screen.getByText("+6")).toBeTruthy();
    expect(screen.getByText("-2")).toBeTruthy();
    expect(screen.getByText("19")).toBeTruthy();
    expect(screen.getByText("变动后余额")).toBeTruthy();
  });

  it("流水支持排序切换与按资产过滤，变更后回第一页", async () => {
    vi.mocked(listUserAssets).mockResolvedValue({ rows: [] });
    vi.mocked(listAssetDefs).mockResolvedValue({
      rows: [
        { id: "d1", code: "jade", name: "玉", class: "currency", decimals: 0 },
        { id: "d2", code: "gold", name: "金币", class: "currency", decimals: 0 },
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
    const ledgerTab = screen.getByRole("tab", { name: "流水" });
    fireEvent.mouseDown(ledgerTab);
    fireEvent.click(ledgerTab);
    await screen.findByText("全部资产");

    // 排序切换：desc → asc，分页回第一页
    fireEvent.click(screen.getByRole("button", { name: /最新在前/ }));
    await vi.waitFor(() => {
      expect(listUserLedger).toHaveBeenLastCalledWith("u1", {
        pageSize: 20, pageToken: "", defCode: undefined, ascending: true,
      });
    });

    // 资产过滤：radix Select 同样需要 pointer 序列
    const trigger = screen.getByRole("combobox");
    fireEvent.mouseDown(trigger);
    fireEvent.click(trigger);
    const option = await screen.findByRole("option", { name: "jade" });
    fireEvent.click(option);
    await vi.waitFor(() => {
      expect(listUserLedger).toHaveBeenLastCalledWith("u1", {
        pageSize: 20, pageToken: "", defCode: "jade", ascending: true,
      });
    });
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
