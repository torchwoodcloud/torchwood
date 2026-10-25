import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useRouterState } from "@tanstack/react-router";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { AuditLogsListPage } from "./pages";
import type { ListAuditLogsParams, ListAuditLogsResponse } from "@/api/auditLogs";

// radix Select 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
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
vi.mock("@/api/auditLogs", () => ({ listAuditLogs: vi.fn() }));

import { listAuditLogs } from "@/api/auditLogs";

const mockList = vi.mocked(listAuditLogs);

function page1(params?: Partial<ListAuditLogsResponse>) {
  return {
    audit_logs: [
      {
        id: "a1",
        action: "/torchwood.server.v1.FunctionsService/UpdateFunction",
        status: "success",
        created_at: "2026-10-20T10:00:00Z",
        metadata: { client: { channel: "cli" }, changes: { limit: { from: 10, to: 20 } } },
      },
      {
        id: "a2",
        action: "/torchwood.server.v1.UsersService/DeleteUser",
        status: "denied",
        created_at: "2026-10-19T09:00:00Z",
      },
    ],
    meta: { total_count: 45, next_page_token: "t2" },
    ...params,
  } satisfies ListAuditLogsResponse;
}

// TanStack 已解析的 search 对象即真相（数组值在 URL 是 JSON 编码串，直接
// 读 location.search 会拿到 %5B…%5D 不可断言）。
function SearchProbe() {
  const search = (useRouterState as unknown as (o: unknown) => Record<string, unknown>)({
    select: (s: { location: { search: object } }) => s.location.search,
  });
  return <div data-testid="loc">{JSON.stringify(search)}</div>;
}

async function renderPage(entry = "/console/audit-logs") {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[
          {
            path: "/console/audit-logs",
            element: (
              <>
                <SearchProbe />
                <AuditLogsListPage />
              </>
            ),
          },
        ]}
        initialEntries={[entry]}
      />
    </QueryClientProvider>
  );
  // RouterProvider 首提交是异步的：冲刷后再断言（仓库惯例）。
  await act(async () => {});
}

const search = () => JSON.parse(screen.getByTestId("loc").textContent!) as Record<string, unknown>;
// react-query 传入的 AbortSignal 是对象，逐参断言只看首个参数。
const lastParams = () => mockList.mock.calls[mockList.mock.calls.length - 1][0] as ListAuditLogsParams;

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.restoreAllMocks();
});

describe("AuditLogsListPage（keyset 切片：URL 即真相）", () => {
  beforeEach(() => {
    mockList.mockImplementation(async () => page1());
  });

  it("初载：默认窗口 page_size=50、无 sort_order（服务端默认 DESC），段渲染", async () => {
    await renderPage();
    await waitFor(() => expect(mockList).toHaveBeenCalled());
    const params = lastParams();
    expect(params.page_size).toBe(50);
    expect(params.sort_order).toBeUndefined();
    expect(screen.getByText(/CLI 更新 Function/)).toBeTruthy();
    expect(screen.getByText(/共 45 条 · 本页 2 条/)).toBeTruthy();
  });

  it("过滤输入写 filter=… 进 URL 并映射请求参数", async () => {
    await renderPage();
    await screen.findByText(/共 45 条 · 本页 2 条/);
    fireEvent.change(screen.getByLabelText("操作"), {
      target: { value: "FunctionsService/UpdateFunction" },
    });
    await waitFor(() => expect(lastParams().action).toBe("FunctionsService/UpdateFunction"));
    expect(search().filter).toEqual(["action:eq:FunctionsService/UpdateFunction"]);
  });

  it("时间列排序循环 asc→desc→无（URL sort 为真相，换向清游标）", async () => {
    await renderPage();
    await screen.findByText(/共 45 条/);
    const head = await screen.findByRole("button", { name: /时间/ });

    fireEvent.click(head);
    await waitFor(() => expect(search().sort).toEqual(["created_at:asc"]));
    await waitFor(() => expect(lastParams().sort_order).toBe("ASC"));

    fireEvent.click(head);
    await waitFor(() => expect(search().sort).toEqual(["created_at:desc"]));
    await waitFor(() => expect(lastParams().sort_order).toBe("DESC"));

    fireEvent.click(head);
    await waitFor(() => expect(search().sort).toBeUndefined());
  });

  it("keyset 翻页：Next 压 cursor 进 URL（push，历史即分页栈），Prev 走浏览器历史", async () => {
    const back = vi.spyOn(window.history, "back").mockImplementation(() => {});
    await renderPage();
    await screen.findByText(/共 45 条/);

    fireEvent.click(screen.getByRole("button", { name: /下一页/ }));
    await waitFor(() => expect(search().cursor).toBe("t2"));
    await waitFor(() => expect(lastParams().page_token).toBe("t2"));

    // 服务端不透明游标 prev 不可本地算 → 块兜底 history.back
    fireEvent.click(screen.getByRole("button", { name: /上一页/ }));
    await waitFor(() => expect(back).toHaveBeenCalled());
  });

  it("页大小切换进 URL 并映射 page_size（keyset 语义：保位置改页宽）", async () => {
    await renderPage();
    await screen.findByText(/共 45 条/);
    // 页大小切换是原生 select（§12.10-⑤ 上游化后）——change 事件驱动
    fireEvent.change(screen.getByLabelText("页大小"), { target: { value: "100" } });
    await waitFor(() => expect(search().size).toBe(100));
    await waitFor(() => expect(lastParams().page_size).toBe(100));
  });

  it("平台视图范围：全部项目展开 include_platform + all_projects", async () => {
    await renderPage();
    await screen.findByText(/共 45 条/);
    fireEvent.click(screen.getByRole("combobox", { name: "视图范围" }));
    fireEvent.click(await screen.findByRole("option", { name: /全部项目/ }));
    await waitFor(() => {
      expect(lastParams().include_platform).toBe(true);
      expect(lastParams().all_projects).toBe(true);
    });
    expect(search().filter).toEqual(["scope:eq:all_projects"]);
  });

  it("空态两分：源空与过滤空文案不同", async () => {
    mockList.mockImplementation(async () => page1({ audit_logs: [], meta: undefined }));
    await renderPage();
    await screen.findByText("暂无审计记录");
    fireEvent.change(screen.getByLabelText("操作"), { target: { value: "NoSuch" } });
    await screen.findByText("没有匹配的审计记录");
  });
});
