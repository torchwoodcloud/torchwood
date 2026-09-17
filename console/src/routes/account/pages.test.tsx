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
  });
  afterEach(() => cleanup());

  it("只读展示账户资料，无编辑控件", async () => {
    renderPage(<AccountProfilePage />);
    expect(await screen.findByText("liqiulin@outlook.com")).toBeTruthy();
    expect(screen.getByText("owner")).toBeTruthy();
    expect(screen.getByText("admin-1")).toBeTruthy();
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(screen.queryByRole("button")).toBeNull();
  });
});

describe("AccountPreferencesPage", () => {
  beforeEach(() => {
    vi.mocked(getCurrentAdmin).mockResolvedValue(me);
    vi.mocked(updateCurrentAdmin).mockReset();
  });
  afterEach(() => cleanup());

  it("选中即暂存，显式保存才提交并更新缓存", async () => {
    vi.mocked(updateCurrentAdmin).mockImplementation(async (input) => ({
      ...me,
      timezone: input.timezone,
    }));
    const { client } = renderPage(<AccountPreferencesPage />);

    const saveButton = () =>
      screen.getByRole("button", { name: /保存/ }) as HTMLButtonElement;
    await screen.findByText(/当前生效：Asia\/Shanghai/);
    expect(saveButton().disabled).toBe(true);

    fireEvent.change(screen.getByPlaceholderText(/搜索时区/), {
      target: { value: "Tokyo" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Asia/Tokyo" }));
    expect(saveButton().disabled).toBe(false);
    expect(updateCurrentAdmin).not.toHaveBeenCalled();

    fireEvent.click(saveButton());
    await waitFor(() =>
      expect(updateCurrentAdmin).toHaveBeenCalledWith({ timezone: "Asia/Tokyo" })
    );
    await waitFor(() =>
      expect(client.getQueryData(["console-admin-me"])).toEqual({
        ...me,
        timezone: "Asia/Tokyo",
      })
    );
    // 保存成功后草稿复位，回到"未改动"态。
    await waitFor(() => expect(saveButton().disabled).toBe(true));
  });

  it("选「跟随浏览器」提交空串清除偏好", async () => {
    vi.mocked(updateCurrentAdmin).mockResolvedValue({ ...me, timezone: undefined });
    renderPage(<AccountPreferencesPage />);
    await screen.findByText(/当前生效：Asia\/Shanghai/);

    fireEvent.click(screen.getByRole("button", { name: /跟随浏览器/ }));
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() =>
      expect(updateCurrentAdmin).toHaveBeenCalledWith({ timezone: "" })
    );
  });
});
