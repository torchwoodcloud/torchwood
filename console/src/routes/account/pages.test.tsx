import type { ReactNode } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AccountPreferencesPage, AccountProfilePage } from "./pages";
import type { Admin } from "@/api/admins";
import { getCurrentAdmin, updateCurrentAdmin } from "@/api/admins";

vi.mock("@/api/admins", () => ({
  getCurrentAdmin: vi.fn(),
  updateCurrentAdmin: vi.fn(),
}));

const me: Admin = {
  id: "admin-1",
  email: "liqiulin@outlook.com",
  role: "owner",
  timezone: "Asia/Shanghai",
  created_at: "2026-09-01T00:00:00Z",
  updated_at: "2026-09-10T00:00:00Z",
};

function renderPage(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={client}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>
  );
  return { ...result, client };
}

describe("AccountProfilePage", () => {
  beforeEach(() => {
    vi.mocked(getCurrentAdmin).mockResolvedValue(me);
    vi.mocked(updateCurrentAdmin).mockReset();
  });
  afterEach(() => cleanup());

  it("展示账户资料与修改密码表单", async () => {
    renderPage(<AccountProfilePage />);
    expect(await screen.findByText("liqiulin@outlook.com")).toBeTruthy();
    expect(screen.getByText("owner")).toBeTruthy();
    expect(screen.getByText("admin-1")).toBeTruthy();
    // 资料只读：字段以文本呈现，编辑控件仅存在于修改密码表单。
    expect(screen.queryByDisplayValue("liqiulin@outlook.com")).toBeNull();
    expect(screen.getByLabelText("当前密码")).toBeTruthy();
    expect(screen.getByLabelText("新密码")).toBeTruthy();
    expect(screen.getByLabelText("确认新密码")).toBeTruthy();
  });

  it("两次新密码不一致时不提交", async () => {
    renderPage(<AccountProfilePage />);
    await screen.findByText("liqiulin@outlook.com");

    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "OldPassw0rd" } });
    fireEvent.change(screen.getByLabelText("新密码"), { target: { value: "NewPassw0rd" } });
    fireEvent.change(screen.getByLabelText("确认新密码"), { target: { value: "OtherPassw0rd" } });
    fireEvent.click(screen.getByRole("button", { name: "修改密码" }));

    expect(screen.getByText("两次输入的新密码不一致")).toBeTruthy();
    expect(updateCurrentAdmin).not.toHaveBeenCalled();
  });

  it("改密成功提交当前/新密码并强制回登录页", async () => {
    const originalLocation = window.location;
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: { href: "" },
    });
    vi.mocked(updateCurrentAdmin).mockResolvedValue({ ...me, timezone: "" });
    renderPage(<AccountProfilePage />);
    await screen.findByText("liqiulin@outlook.com");

    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "OldPassw0rd" } });
    fireEvent.change(screen.getByLabelText("新密码"), { target: { value: "NewPassw0rd" } });
    fireEvent.change(screen.getByLabelText("确认新密码"), { target: { value: "NewPassw0rd" } });
    fireEvent.click(screen.getByRole("button", { name: "修改密码" }));

    await waitFor(() =>
      expect(updateCurrentAdmin).toHaveBeenCalledWith(
        { current_password: "OldPassw0rd", new_password: "NewPassw0rd" },
        { __skipToast: true }
      )
    );
    await waitFor(() =>
      expect(window.location.href).toBe("/console/login?reason=password_changed")
    );
    Object.defineProperty(window, "location", {
      configurable: true,
      writable: true,
      value: originalLocation,
    });
  });
});

describe("AccountPreferencesPage", () => {
  beforeEach(() => {
    vi.mocked(getCurrentAdmin).mockResolvedValue(me);
    vi.mocked(updateCurrentAdmin).mockReset();
  });
  afterEach(() => cleanup());

  it("列表行展示当前时区，弹层内选中即暂存、显式保存才提交", async () => {
    vi.mocked(updateCurrentAdmin).mockImplementation(async (input) => ({
      ...me,
      timezone: input.timezone,
    }));
    const { client } = renderPage(<AccountPreferencesPage />);

    // 行内当前值 = 修改入口；未改动时弹层保存禁用。
    fireEvent.click(await screen.findByRole("button", { name: /Asia\/Shanghai/ }));
    const saveButton = screen.getByRole("button", { name: "保存" }) as HTMLButtonElement;
    expect(saveButton.disabled).toBe(true);

    fireEvent.change(screen.getByPlaceholderText(/搜索时区/), {
      target: { value: "Tokyo" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Asia/Tokyo" }));
    expect(saveButton.disabled).toBe(false);
    expect(updateCurrentAdmin).not.toHaveBeenCalled();

    fireEvent.click(saveButton);
    await waitFor(() =>
      expect(updateCurrentAdmin).toHaveBeenCalledWith(
        { timezone: "Asia/Tokyo" },
        { __skipToast: true }
      )
    );
    await waitFor(() =>
      expect(client.getQueryData(["console-admin-me"])).toEqual({
        ...me,
        timezone: "Asia/Tokyo",
      })
    );
  });

  it("选「跟随浏览器」提交空串清除偏好", async () => {
    vi.mocked(updateCurrentAdmin).mockResolvedValue({ ...me, timezone: undefined });
    renderPage(<AccountPreferencesPage />);
    fireEvent.click(await screen.findByRole("button", { name: /Asia\/Shanghai/ }));

    fireEvent.click(screen.getByRole("button", { name: /跟随浏览器/ }));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() =>
      expect(updateCurrentAdmin).toHaveBeenCalledWith(
        { timezone: "" },
        { __skipToast: true }
      )
    );
  });

  it("未设置偏好时行内显示跟随浏览器", async () => {
    vi.mocked(getCurrentAdmin).mockResolvedValue({ ...me, timezone: "" });
    renderPage(<AccountPreferencesPage />);
    expect(await screen.findByRole("button", { name: /跟随浏览器（/ })).toBeTruthy();
  });
});
