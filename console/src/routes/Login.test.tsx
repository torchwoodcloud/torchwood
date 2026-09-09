import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { getSetupStatus } from "@/api/auth";
import { Login } from "./Login";

// 只 mock 认证依赖;shadcn 表单组件真渲染。
const mockUseAuth = vi.fn();
vi.mock("@/hooks/useAuth", () => ({ useAuth: () => mockUseAuth() }));
vi.mock("@/api/auth", () => ({
  getSetupStatus: vi.fn(),
  signUp: vi.fn(),
  login: vi.fn(),
  logout: vi.fn(),
  refreshSession: vi.fn(),
}));

vi.mocked(getSetupStatus).mockResolvedValue({
  needs_setup: false,
  setup_token_required: false,
});

function LocationProbe() {
  const loc = useLocation();
  return <div data-testid="loc">{loc.pathname + loc.search}</div>;
}

function renderLogin(entry: string) {
  return render(
    <MemoryRouter initialEntries={[entry]}>
      <LocationProbe />
      <Routes>
        <Route path="/console/login" element={<Login />} />
        <Route path="/console" element={<div>console-page</div>} />
      </Routes>
    </MemoryRouter>
  );
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("Login 弹跳断路器", () => {
  it("expired=1 时即使探测成功(已认证)也不自动跳回 console", async () => {
    mockUseAuth.mockReturnValue({ isAuthenticated: true, login: vi.fn() });
    renderLogin("/console/login?expired=1");

    // 探测与渲染稳定后仍停在登录表单——强制跳转来源的会话必须显式重新登录。
    await waitFor(() => {
      expect(getSetupStatus).toHaveBeenCalled();
    });
    expect(screen.getByRole("textbox", { name: "Email" })).toBeTruthy();
    expect(screen.getByTestId("loc").textContent).toBe("/console/login?expired=1");
    expect(screen.getByText("会话已过期，请重新登录")).toBeTruthy();
  });

  it("无 expired 标记且已认证时保持原行为自动跳回 console", async () => {
    mockUseAuth.mockReturnValue({ isAuthenticated: true, login: vi.fn() });
    renderLogin("/console/login");

    await waitFor(() => {
      expect(screen.getByText("console-page")).toBeTruthy();
    });
  });

  it("未认证时不跳转,正常渲染登录表单", async () => {
    mockUseAuth.mockReturnValue({ isAuthenticated: false, login: vi.fn() });
    renderLogin("/console/login");

    await waitFor(() => {
      expect(getSetupStatus).toHaveBeenCalled();
    });
    expect(screen.getByRole("textbox", { name: "Email" })).toBeTruthy();
    expect(screen.queryByText("会话已过期，请重新登录")).toBeNull();
  });
});
