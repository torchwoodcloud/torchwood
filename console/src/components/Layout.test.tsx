import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Layout } from "./Layout";
import { getCurrentAdmin } from "@/api/admins";
import { listProjects } from "@/api/projects";

vi.mock("@/hooks/useAuth", () => ({
  useAuth: () => ({ logout: vi.fn(), projectId: null, selectProject: vi.fn() }),
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/admins", () => ({ getCurrentAdmin: vi.fn() }));
vi.mock("@/api/projects", () => ({ listProjects: vi.fn() }));

const COLLAPSED_KEY = "TORCHWOOD_console_sidebar_collapsed";

function renderLayout() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/console"]}>
        <Routes>
          <Route path="/console" element={<Layout />}>
            <Route index element={<div>child-page</div>} />
          </Route>
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
  // 第一个 aside = 桌面侧栏（第二个为移动端抽屉）。
  const desktopSidebar = result.container.querySelector("aside") as HTMLElement;
  return { ...result, desktopSidebar };
}

describe("Layout 侧栏收起", () => {
  beforeEach(() => {
    localStorage.clear();
    vi.mocked(getCurrentAdmin).mockResolvedValue({
      id: "admin-1",
      email: "ops@example.com",
      role: "owner",
    });
    vi.mocked(listProjects).mockResolvedValue([]);
  });
  afterEach(() => {
    cleanup();
    vi.clearAllMocks();
  });

  it("默认展开，点「收起菜单」切到图标栏并持久化", async () => {
    const { desktopSidebar } = renderLayout();
    const collapseButton = await screen.findByTitle("收起菜单");
    expect(desktopSidebar.className).toContain("w-64");
    expect(within(desktopSidebar).getByText("Storage")).toBeTruthy();

    fireEvent.click(collapseButton);
    expect(desktopSidebar.className).toContain("w-16");
    expect(localStorage.getItem(COLLAPSED_KEY)).toBe("1");
    // 收起后标签隐藏，仅剩 title 提示（移动端抽屉恒展开，按桌面侧栏断言）。
    expect(within(desktopSidebar).queryByText("Storage")).toBeNull();
    expect(within(desktopSidebar).getByTitle("Storage")).toBeTruthy();
    expect(screen.getByTitle("展开菜单")).toBeTruthy();
  });

  it("持久化收起态在刷新后生效，点「展开菜单」恢复", async () => {
    localStorage.setItem(COLLAPSED_KEY, "1");
    const { desktopSidebar } = renderLayout();
    const expandButton = await screen.findByTitle("展开菜单");
    expect(desktopSidebar.className).toContain("w-16");
    expect(within(desktopSidebar).queryByText("Storage")).toBeNull();

    fireEvent.click(expandButton);
    expect(desktopSidebar.className).toContain("w-64");
    expect(localStorage.getItem(COLLAPSED_KEY)).toBe("0");
    expect(within(desktopSidebar).getByText("Storage")).toBeTruthy();
  });
});
