import type { ReactNode } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/test-utils/TestRouter";
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

function axiosErr(status: number, body?: { code?: string; message?: string }) {
  return Object.assign(new Error("req failed"), {
    isAxiosError: true,
    response: { status, data: body ? { error: body } : undefined },
  });
}

function renderPage(ui: ReactNode) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const result = render(
    <QueryClientProvider client={client}>
      <TestRouter routes={[{ path: "/*", element: ui }]} />
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
    expect(screen.getByLabelText("Current password")).toBeTruthy();
    expect(screen.getByLabelText("New password")).toBeTruthy();
    expect(screen.getByLabelText("Confirm new password")).toBeTruthy();
  });

  it("两次新密码不一致时不提交", async () => {
    renderPage(<AccountProfilePage />);
    await screen.findByText("liqiulin@outlook.com");

    fireEvent.change(screen.getByLabelText("Current password"), { target: { value: "OldPassw0rd" } });
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "NewPassw0rd" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "OtherPassw0rd" } });
    fireEvent.click(screen.getByRole("button", { name: "Change Password" }));

    expect(screen.getByText("New passwords do not match")).toBeTruthy();
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

    fireEvent.change(screen.getByLabelText("Current password"), { target: { value: "OldPassw0rd" } });
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "NewPassw0rd" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "NewPassw0rd" } });
    fireEvent.click(screen.getByRole("button", { name: "Change Password" }));

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

  it("改密 400 → rejected 走内联错误（结果两分，不跳登录页）", async () => {
    vi.mocked(updateCurrentAdmin).mockRejectedValue(
      axiosErr(400, { code: "E_INVALID_CREDENTIALS", message: "Current password is incorrect" })
    );
    renderPage(<AccountProfilePage />);
    await screen.findByText("liqiulin@outlook.com");

    fireEvent.change(screen.getByLabelText("Current password"), { target: { value: "WrongPassw0rd" } });
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "NewPassw0rd" } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "NewPassw0rd" } });
    fireEvent.click(screen.getByRole("button", { name: "Change Password" }));

    await waitFor(() =>
      expect(updateCurrentAdmin).toHaveBeenCalledWith(
        { current_password: "WrongPassw0rd", new_password: "NewPassw0rd" },
        { __skipToast: true }
      )
    );
    // rejected 是结果分支（mutation.data）：渲染表单内联错误，不跳转
    expect(await screen.findByText("Current password is incorrect")).toBeTruthy();
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
    const saveButton = screen.getByRole("button", { name: "Save" }) as HTMLButtonElement;
    expect(saveButton.disabled).toBe(true);

    fireEvent.change(screen.getByPlaceholderText(/Search time zones/), {
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

  it("选「跟随浏览器」提交空串清除偏好", { timeout: 15_000 }, async () => {
    vi.mocked(updateCurrentAdmin).mockResolvedValue({ ...me, timezone: undefined });
    renderPage(<AccountPreferencesPage />);
    fireEvent.click(await screen.findByRole("button", { name: /Asia\/Shanghai/ }));

    fireEvent.click(screen.getByRole("button", { name: /Follow browser/ }));
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
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
    expect(await screen.findByRole("button", { name: /Follow browser \(/ })).toBeTruthy();
  });
});
