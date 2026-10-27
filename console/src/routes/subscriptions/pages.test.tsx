import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/test-utils/TestRouter";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { PlanNewPage, PlansListPage, SubscriptionsListPage } from "./pages";

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
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/subscriptions", () => ({
  listPlans: vi.fn(),
  getPlan: vi.fn(),
  createPlan: vi.fn(),
  deletePlan: vi.fn(),
  listSubscriptions: vi.fn(),
  getSubscription: vi.fn(),
  cancelSubscription: vi.fn(),
  expireSubscription: vi.fn(),
}));

import { createPlan, deletePlan, listPlans, listSubscriptions } from "@/api/subscriptions";

const mockListPlans = vi.mocked(listPlans);
const mockListSubs = vi.mocked(listSubscriptions);
const mockCreatePlan = vi.mocked(createPlan);
const mockDeletePlan = vi.mocked(deletePlan);

const plan = {
  id: "p1",
  code: "pro",
  name: "Pro",
  amount: "999",
  currency: "USD",
  interval: "month",
  status: "active",
};

const sub = {
  id: "s1",
  user_id: "u1",
  plan_code: "pro",
  mode: "platform",
  status: "active",
};

// useCollection 的 from 锚定真实路由 id——两个列表各挂各自真实路径。
async function renderPage(
  path: string,
  element: React.ReactElement,
  entry = path,
  // 额外路由：命令 onSettled 里 navigate 的目标必须真实注册（api-keys 测试
  // 同款——not-found rejection 落点随并行时序漂移）。
  extraRoutes: { path: string; element: React.ReactElement }[] = [],
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter routes={[{ path, element }, ...extraRoutes]} initialEntries={[entry]} />
    </QueryClientProvider>
  );
  await act(async () => {});
}

describe("PlansListPage", () => {
  beforeEach(() => {
    mockListPlans.mockReset();
  });
  afterEach(() => cleanup());

  it("展示计划金额为最小单位", async () => {
    mockListPlans.mockResolvedValue({ rows: [plan] });
    await renderPage("/console/subscriptions/plans", <PlansListPage />);
    expect(await screen.findByText("999 USD")).toBeTruthy();
    expect(screen.getByText("pro")).toBeTruthy();
  });

  it("URL 即真相：默认页大小/翻页游标透传 listPlans（page_token）", async () => {
    mockListPlans.mockResolvedValue({ rows: [plan], nextPageToken: "tok-2" });
    await renderPage("/console/subscriptions/plans", <PlansListPage />);
    await screen.findByText("999 USD");
    // defaultSize 20 = 旧页默认；首次请求无 token，sort_order 落服务端默认 DESC
    expect(mockListPlans).toHaveBeenLastCalledWith(
      expect.objectContaining({ pageSize: 20, pageToken: undefined, sortOrder: undefined })
    );
    // 下一页 → 游标 push 进 URL → 请求带 page_token
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(mockListPlans).toHaveBeenLastCalledWith(
        expect.objectContaining({ pageToken: "tok-2" })
      )
    );
  });

  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockListPlans.mockResolvedValue({ rows: [plan], nextPageToken: undefined });
    mockDeletePlan.mockResolvedValue(undefined);
    await renderPage("/console/subscriptions/plans", <PlansListPage />);
    await screen.findByText("999 USD");
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete" }));
    await waitFor(() =>
      expect(mockDeletePlan).toHaveBeenCalledWith("p1", { __skipToast: true })
    );
  });
});

describe("PlanNewPage（写路径命令化）", () => {
  it("创建：grace_days 文本收敛为整数 + __skipToast", async () => {
    mockCreatePlan.mockResolvedValue({ ...plan, id: "plan-9" });
    await renderPage(
      "/console/subscriptions/plans/new",
      <PlanNewPage />,
      "/console/subscriptions/plans/new",
      // ok 后 navigate 到详情——目标路由注册
      [{ path: "/console/subscriptions/plans/$id", element: <div /> }],
    );
    fireEvent.change(screen.getByLabelText("Code"), { target: { value: "pro" } });
    fireEvent.change(screen.getByLabelText("Name"), { target: { value: "Pro" } });
    fireEvent.change(screen.getByLabelText("Amount (minor units)"), {
      target: { value: "999" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() =>
      expect(mockCreatePlan).toHaveBeenCalledWith(
        {
          code: "pro",
          name: "Pro",
          amount: "999",
          currency: "USD",
          interval: "month",
          grace_days: 3,
        },
        { __skipToast: true },
      ),
    );
  });
});

describe("SubscriptionsListPage", () => {
  beforeEach(() => {
    mockListSubs.mockReset();
  });
  afterEach(() => cleanup());

  it("URL 即真相：UserID 过滤写 filter 进 URL 并映射请求参数（清游标）", async () => {
    mockListSubs.mockResolvedValue({ rows: [sub], nextPageToken: "tok-9" });
    await renderPage("/console/subscriptions", <SubscriptionsListPage />);
    await screen.findByText("s1");

    // 自由文本输入本地态，提交（查询）才写 URL 谓词
    fireEvent.change(screen.getByLabelText("User ID"), { target: { value: "u1" } });
    fireEvent.click(screen.getByRole("button", { name: "Apply" }));
    await waitFor(() =>
      expect(mockListSubs).toHaveBeenLastCalledWith(
        expect.objectContaining({ userId: "u1", pageToken: undefined })
      )
    );

    // 翻页后过滤再变化 → 清游标回第一页（keyset 游标与参数组合耦合）
    fireEvent.click(screen.getByRole("button", { name: "Next" }));
    await waitFor(() =>
      expect(mockListSubs).toHaveBeenLastCalledWith(
        expect.objectContaining({ userId: "u1", pageToken: "tok-9" })
      )
    );
  });
});
