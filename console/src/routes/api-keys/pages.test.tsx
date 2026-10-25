import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/router/compat";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { ApiKeysListPage, ApiKeyNewPage, ApiKeyDetailPage } from "./pages";
import type { APIKey } from "@/api/apiKeys";

// radix Select/Dialog 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "p1" }) }));
vi.mock("@/hooks/useAdminRole", () => ({
  useAdminRole: () => ({ role: "owner", isLoading: false }),
  canWrite: () => true,
  isPlatformAdmin: () => true,
}));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/wellknown", () => ({ fetchApiKeyScopeCatalog: vi.fn().mockResolvedValue([]) }));
vi.mock("@/api/apiKeys", () => ({
  listAPIKeys: vi.fn(),
  getAPIKey: vi.fn(),
  createAPIKey: vi.fn(),
  updateAPIKey: vi.fn(),
  deleteAPIKey: vi.fn(),
}));

import { listAPIKeys, getAPIKey, createAPIKey, updateAPIKey, deleteAPIKey } from "@/api/apiKeys";

const mockList = vi.mocked(listAPIKeys);
const mockGet = vi.mocked(getAPIKey);
const mockCreate = vi.mocked(createAPIKey);
const mockUpdate = vi.mocked(updateAPIKey);
const mockDelete = vi.mocked(deleteAPIKey);

const key: APIKey = {
  id: "key-1",
  name: "prod",
  scopes: ["databases:blog.read"],
  enabled: true,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

function axiosErr(status: number, body?: { code?: string; message?: string }) {
  return Object.assign(new Error("req failed"), {
    isAxiosError: true,
    response: { status, data: body ? { error: body } : undefined },
  });
}

async function renderPage(
  routePath: string,
  element: React.ReactElement,
  entry = routePath,
  // 额外路由：命令 onSettled 里 navigate 的目标必须真实注册，否则
  // not-found rejection 落点随并行时序漂移（全套跑时偶发失败）。
  extraRoutes: { path: string; element: React.ReactElement }[] = [],
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: routePath, element }, ...extraRoutes]}
        initialEntries={[entry]}
      />
    </QueryClientProvider>,
  );
  // RouterProvider 首提交异步：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("ApiKeyNewPage（useCommandForm + reveal）", () => {
  it("提交：表单值收敛（wildcard/勾选/手输）+ reveal 一次性显示 secret", async () => {
    mockCreate.mockResolvedValue({ api_key: key, secret: "sk-live-once" });
    await renderPage("/console/api-keys/new", <ApiKeyNewPage />);

    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "prod" } });
    fireEvent.change(screen.getByLabelText("自定义 Scopes（可选，逗号或换行分隔）"), {
      target: { value: "storage:media" },
    });
    fireEvent.click(screen.getByRole("button", { name: "创建" }));

    await waitFor(() =>
      expect(mockCreate).toHaveBeenCalledWith(
        { name: "prod", scopes: ["storage:media"] },
        { __skipToast: true },
      ),
    );
    // reveal 屏：secret 只在这一帧
    expect(await screen.findByText("sk-live-once")).toBeTruthy();
    expect(screen.getByText("API Key 已创建")).toBeTruthy();
  });

  it("名称必填：空提交不发货，字段错误可见", async () => {
    await renderPage("/console/api-keys/new", <ApiKeyNewPage />);
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    await screen.findByText("必填");
    expect(mockCreate).not.toHaveBeenCalled();
  });

  it("rejected（400）走结果盒：message-first + code 前缀，非 toast 异常", async () => {
    mockCreate.mockRejectedValue(axiosErr(400, { code: "E_INVALID_ARGUMENT", message: "名称无效" }));
    await renderPage("/console/api-keys/new", <ApiKeyNewPage />);
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "创建" }));
    const box = await screen.findByRole("alert");
    expect(box.textContent).toContain("E_INVALID_ARGUMENT");
    expect(box.textContent).toContain("名称无效");
  });
});

describe("ApiKeyDetailPage（编辑对话框 diff + 轮换双命令）", () => {
  it("编辑：仅传变更字段；ok 后对话框关闭", async () => {
    mockGet.mockResolvedValue(key);
    mockUpdate.mockResolvedValue({ ...key, name: "prod2" });
    await renderPage("/console/api-keys/$id", <ApiKeyDetailPage />, "/console/api-keys/key-1");
    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));

    fireEvent.change(await screen.findByLabelText("名称"), { target: { value: "prod2" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));

    await waitFor(() =>
      expect(mockUpdate).toHaveBeenCalledWith("key-1", { name: "prod2" }, { __skipToast: true }),
    );
    await waitFor(() => expect(screen.queryByRole("button", { name: "保存" })).toBeNull());
  });

  it("轮换：① 建 -rotated 同 scopes → secret 屏；② 旧 Key 立即过期", async () => {
    mockGet.mockResolvedValue(key);
    const rotated = { ...key, id: "key-2", name: "prod-rotated" };
    mockCreate.mockResolvedValue({ api_key: rotated, secret: "sk-rotated-once" });
    mockUpdate.mockResolvedValue(key);
    await renderPage("/console/api-keys/$id", <ApiKeyDetailPage />, "/console/api-keys/key-1");
    fireEvent.click(await screen.findByRole("button", { name: "轮换" }));

    fireEvent.click(await screen.findByRole("button", { name: "① 创建新 Key" }));
    await waitFor(() =>
      expect(mockCreate).toHaveBeenCalledWith(
        { name: "prod-rotated", scopes: key.scopes },
        { __skipToast: true },
      ),
    );
    expect(await screen.findByText("sk-rotated-once")).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "② 旧 Key 立即过期" }));
    await waitFor(() => expect(mockUpdate).toHaveBeenCalled());
    const [id, input] = mockUpdate.mock.calls[0];
    expect(id).toBe("key-1");
    expect(typeof (input as { expire_at?: string }).expire_at).toBe("string");
  });

  it("删除：确认后 deleteAPIKey 带 __skipToast", async () => {
    mockGet.mockResolvedValue(key);
    mockDelete.mockResolvedValue(undefined);
    await renderPage(
      "/console/api-keys/$id",
      <ApiKeyDetailPage />,
      "/console/api-keys/key-1",
      // onSettled navigate 回列表——目标路由要在 harness 里注册
      [{ path: "/console/api-keys", element: <div /> }],
    );
    fireEvent.click(await screen.findByRole("button", { name: "删除" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(mockDelete).toHaveBeenCalledWith("key-1", { __skipToast: true }),
    );
  });
});

describe("ApiKeysListPage（行删命令接线）", () => {
  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockList.mockResolvedValue({ rows: [key], nextPageToken: undefined });
    mockDelete.mockResolvedValue(undefined);
    await renderPage("/console/api-keys", <ApiKeysListPage />);
    await screen.findByText("prod");
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(mockDelete).toHaveBeenCalledWith("key-1", { __skipToast: true }),
    );
  });
});
