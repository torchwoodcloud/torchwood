import type { ReactNode } from "react";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/test-utils/TestRouter";
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

import {
  getAssetDef,
  listAssetDefs,
  listDefHolders,
  listUserAssets,
  listUserLedger,
  deleteAssetDef,
} from "@/api/assets";

async function wrap(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={client}>
      <TestRouter routes={[{ path: "/*", element: ui }]} />
    </QueryClientProvider>
  );
  // TanStack RouterProvider 首帧异步挂载——flush 微任务后再交还同步查询
  await act(async () => {});
  return result;
}

describe("AssetDefsListPage", () => {
  beforeEach(() => {
    vi.mocked(listAssetDefs).mockReset();
  });
  afterEach(() => cleanup());

  // useCollection 的 from 锚定真实路由 id（/console/assets）——列表页测试
  // 用真实路径挂载（api-keys pages.test.tsx 同款）。
  async function renderListPage() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          routes={[{ path: "/console/assets", element: <AssetDefsListPage /> }]}
          initialEntries={["/console/assets"]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
  }

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
    await renderListPage();
    expect(await screen.findByText("gold")).toBeTruthy();
    expect(screen.queryByText(/Grant/i)).toBeNull();
    expect(screen.queryByText(/Consume/i)).toBeNull();
    expect(screen.queryByText(/Transfer/i)).toBeNull();
    expect(screen.getByText("Query user assets")).toBeTruthy();
  });

  it("URL 即真相：默认页大小/翻页游标透传 listAssetDefs（page_token）", async () => {
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
      nextPageToken: "tok-2",
    });
    await renderListPage();
    await screen.findByText("gold");
    // defaultSize 20 = 旧页默认；首次请求无 token，sort_order 落服务端默认 DESC
    expect(listAssetDefs).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined, sortOrder: undefined })
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(listAssetDefs).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" })
      )
    );
  });

  // 写路径锚（§12.3）：行删确认走 useCommand——api 收到 SKIP 且成功路径不抛。
  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
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
      nextPageToken: undefined,
    });
    vi.mocked(deleteAssetDef).mockResolvedValue(undefined);
    await renderListPage();
    await screen.findByText("gold");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() =>
      expect(deleteAssetDef).toHaveBeenCalledWith("d1", { __skipToast: true })
    );
  });
});

describe("UserAssetsPage", () => {
  beforeEach(() => {
    vi.mocked(listUserAssets).mockReset();
    vi.mocked(listUserLedger).mockReset();
  });
  afterEach(() => cleanup());

  it("只读查询表单，无资产写按钮", async () => {
    await wrap(<UserAssetsPage />);
    expect(screen.getByLabelText("User ID")).toBeTruthy();
    expect(screen.getByText("Query")).toBeTruthy();
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
    vi.mocked(listAssetDefs).mockResolvedValue({
      rows: [{ id: "d1", code: "gold", name: "金币", class: "currency", decimals: 0 }],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          initialEntries={["/console/assets/users?owner=u1"]}
          routes={[{ path: "/console/assets/users", element: <UserAssetsPage /> }]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
    expect(await screen.findByText("gold")).toBeTruthy();
    expect(await screen.findByText("金币")).toBeTruthy();
    expect(screen.getByText("100")).toBeTruthy();
    expect(screen.getByText("Querying project:")).toBeTruthy();
    expect(screen.getByText("proj-1")).toBeTruthy();
    expect(listUserAssets).toHaveBeenCalledWith("u1", { pageSize: 20, pageToken: "" });
    expect(listUserLedger).toHaveBeenCalledWith("u1", { pageSize: 20, pageToken: "" });
    expect((screen.getByLabelText("User ID") as HTMLInputElement).value).toBe("u1");
  });

  it("持有支持当前页内按 code/名称搜索", async () => {
    vi.mocked(listUserAssets).mockResolvedValue({
      rows: [
        { id: "h1", def_id: "d1", def_code: "gold", class: "currency", quantity: "100" },
        { id: "h2", def_id: "d2", def_code: "cdx_pixiu", class: "stack", quantity: "1" },
      ],
    });
    vi.mocked(listUserLedger).mockResolvedValue({ rows: [] });
    vi.mocked(listAssetDefs).mockResolvedValue({
      rows: [
        { id: "d1", code: "gold", name: "金币", class: "currency", decimals: 0 },
        { id: "d2", code: "cdx_pixiu", name: "貔貅", class: "stack", decimals: 0 },
      ],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          initialEntries={["/console/assets/users?owner=u1"]}
          routes={[{ path: "/console/assets/users", element: <UserAssetsPage /> }]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
    expect(await screen.findByText("金币")).toBeTruthy();
    expect(screen.getByText("貔貅")).toBeTruthy();

    // 按 code 片段搜索：只留 cdx_pixiu 行
    fireEvent.change(screen.getByPlaceholderText(/Search code \/ name \/ class/), { target: { value: "cdx" } });
    expect(screen.getByText("貔貅")).toBeTruthy();
    expect(screen.queryByText("金币")).toBeNull();
    expect(screen.queryByText("No matches on this page")).toBeNull();

    // 按中文名称搜索：只留 gold 行；无匹配显示占位提示
    fireEvent.change(screen.getByPlaceholderText(/Search code \/ name \/ class/), { target: { value: "金币" } });
    expect(screen.getByText("金币")).toBeTruthy();
    expect(screen.queryByText("貔貅")).toBeNull();
    fireEvent.change(screen.getByPlaceholderText(/Search code \/ name \/ class/), { target: { value: "nope" } });
    expect(screen.getByText("No matches on this page")).toBeTruthy();
    expect(screen.queryByText("金币")).toBeNull();
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
    vi.mocked(listAssetDefs).mockResolvedValue({
      rows: [{ id: "d1", code: "jade", name: "玉", class: "currency", decimals: 0 }],
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          initialEntries={["/console/assets/users?owner=u1"]}
          routes={[{ path: "/console/assets/users", element: <UserAssetsPage /> }]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
    // radix Tabs 内容懒渲染：先切到流水 tab 再断言（radix Trigger 依赖
    // pointer 序列，jsdom 里需先 mouseDown 再 click 才会激活）。
    const ledgerTab = screen.getByRole("tab", { name: "Ledger" });
    fireEvent.mouseDown(ledgerTab);
    fireEvent.click(ledgerTab);
    expect(await screen.findByText("Grant")).toBeTruthy();
    expect(screen.getByText("Consume")).toBeTruthy();
    expect(screen.getAllByText("玉").length).toBe(2);
    expect(screen.getAllByText("+6").length).toBe(1);
    expect(screen.getByText("-2")).toBeTruthy();
    expect(screen.getByText("19")).toBeTruthy();
    expect(screen.getByText("Balance after")).toBeTruthy();
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
        <TestRouter
          initialEntries={["/console/assets/users?owner=u1"]}
          routes={[{ path: "/console/assets/users", element: <UserAssetsPage /> }]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
    const ledgerTab = screen.getByRole("tab", { name: "Ledger" });
    fireEvent.mouseDown(ledgerTab);
    fireEvent.click(ledgerTab);
    await screen.findByText("All assets");

    // 排序切换：desc → asc，分页回第一页
    fireEvent.click(screen.getByRole("button", { name: /Newest first/ }));
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

  async function renderDetail() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <TestRouter
          initialEntries={["/console/assets/defs/d1"]}
          routes={[{ path: "/console/assets/defs/:id", element: <AssetDefDetailPage /> }]}
        />
      </QueryClientProvider>
    );
    await act(async () => {});
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
    await renderDetail();
    expect(await screen.findByText("User holdings (read-only)")).toBeTruthy();
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
    await renderDetail();
    await screen.findByText("User holdings (read-only)");
    fireEvent.change(screen.getByLabelText("User ID"), { target: { value: "u2" } });
    fireEvent.click(screen.getByRole("button", { name: "Query" }));
    await vi.waitFor(() => {
      expect(listDefHolders).toHaveBeenLastCalledWith("d1", {
        ownerId: "u2",
        pageSize: 20,
        pageToken: "",
      });
    });
  });
});
