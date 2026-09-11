import type { ReactNode } from "react";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AssetDefsListPage, UserAssetsPage } from "./pages";

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
}));

import { listAssetDefs, listUserAssets, listUserLedger } from "@/api/assets";

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
    vi.mocked(listAssetDefs).mockResolvedValue([
      {
        id: "d1",
        code: "gold",
        name: "金币",
        class: "currency",
        decimals: 0,
        status: "active",
        created_at: "2026-08-20T00:00:00Z",
      },
    ]);
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
    vi.mocked(listUserAssets).mockResolvedValue([
      { id: "h1", def_id: "d1", def_code: "gold", class: "currency", quantity: "100" },
    ]);
    vi.mocked(listUserLedger).mockResolvedValue([]);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <MemoryRouter initialEntries={["/console/assets/users?owner=u1"]}>
          <UserAssetsPage />
        </MemoryRouter>
      </QueryClientProvider>
    );
    expect(await screen.findByText(/gold/)).toBeTruthy();
    expect(listUserAssets).toHaveBeenCalledWith("u1");
    expect(listUserLedger).toHaveBeenCalledWith("u1");
    expect((screen.getByLabelText("用户 ID") as HTMLInputElement).value).toBe("u1");
  });
});
