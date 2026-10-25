import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { TestRouter } from "@/router/compat";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { LeaderboardsListPage } from "./pages";
import type { LeaderboardBoard } from "@/api/leaderboards";

// radix Select/Dialog 在 jsdom 里滚动选中项：scrollIntoView 未实现，桩掉。
beforeAll(() => {
  Element.prototype.scrollIntoView = vi.fn();
});

vi.mock("@/hooks/useAuth", () => ({ useAuth: () => ({ projectId: "proj-1" }) }));
vi.mock("@/hooks/useTimezone", () => ({ useUserTimezone: () => "UTC" }));
vi.mock("@/api/leaderboards", () => ({
  createBoard: vi.fn(),
  deleteBoard: vi.fn(),
  deleteBoardEntry: vi.fn(),
  getBoard: vi.fn(),
  getBoardEntry: vi.fn(),
  getSettlement: vi.fn(),
  listSettlements: vi.fn(),
  LEADERBOARD_SORTS: ["desc", "asc"],
  LEADERBOARD_TIE_BREAKS: ["parallel", "earliest", "latest"],
  LEADERBOARD_PERIOD_KINDS: ["daily", "weekly", "monthly", "none"],
  LEADERBOARD_POLICIES: ["best", "latest", "sum"],
  listBoardPeriods: vi.fn(),
  listBoardTop: vi.fn(),
  listBoards: vi.fn(),
  rerunSettlement: vi.fn(),
  updateBoard: vi.fn(),
  voidSettlement: vi.fn(),
}));

import { listBoards, deleteBoard } from "@/api/leaderboards";

const mockListBoards = vi.mocked(listBoards);
const mockDeleteBoard = vi.mocked(deleteBoard);

const boards: LeaderboardBoard[] = [
  {
    id: "daily_final",
    sort: "desc",
    tie_break: "parallel",
    period_kind: "daily",
    period_tz: "Asia/Shanghai",
    policy: "best",
    client_submit: true,
    per_subject_submit_limit: 100,
    retention_periods: 0,
    subject_kind: "user",
    updated_at: "2026-10-01T00:00:00Z",
  },
  {
    id: "weekly_score",
    sort: "asc",
    tie_break: "earliest",
    period_kind: "none",
    policy: "sum",
    client_submit: false,
  },
];

// useCollection 的 from 锚定真实路由 id（/console/leaderboards）。
async function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <TestRouter
        routes={[{ path: "/console/leaderboards", element: <LeaderboardsListPage /> }]}
        initialEntries={["/console/leaderboards"]}
      />
    </QueryClientProvider>
  );
  await act(async () => {});
}

describe("LeaderboardsListPage", () => {
  beforeEach(() => {
    mockListBoards.mockReset();
  });
  afterEach(() => cleanup());

  it("列出全部榜（榜 ID 链接到详情），行操作保留", async () => {
    mockListBoards.mockResolvedValue(boards);
    await renderList();
    expect(await screen.findByText("daily_final")).toBeTruthy();
    expect(screen.getByText("weekly_score")).toBeTruthy();
    const link = screen.getByRole("link", { name: "daily_final" }) as HTMLAnchorElement;
    expect(link.getAttribute("href")).toContain("/console/leaderboards/daily_final");
    // 行操作：每行一个删除入口（编辑对话框触发钮为图标钮，无文案断言）
    expect(screen.getAllByRole("button", { name: "删除" }).length).toBe(2);
  });

  it("非分页端点即真相：listBoards 单次全量拉取，无分页控件", async () => {
    mockListBoards.mockResolvedValue(boards);
    await renderList();
    await screen.findByText("daily_final");
    // boards 端点无 page_token/过滤参数 → 适配器不透传窗口，只拉一次
    expect(mockListBoards).toHaveBeenCalledTimes(1);
    expect(mockListBoards).toHaveBeenCalledWith();
    expect(screen.queryByRole("button", { name: /下一页/ })).toBeNull();
    expect(screen.queryByRole("combobox", { name: "页大小" })).toBeNull();
  });

  it("榜 ID 页内搜索为本地态：输入即过滤当前结果", async () => {
    mockListBoards.mockResolvedValue(boards);
    await renderList();
    await screen.findByText("daily_final");
    fireEvent.change(screen.getByPlaceholderText(/搜索榜 ID/), {
      target: { value: "weekly" },
    });
    expect(screen.getByText("weekly_score")).toBeTruthy();
    expect(screen.queryByText("daily_final")).toBeNull();
    // 清空恢复全部行
    fireEvent.change(screen.getByPlaceholderText(/搜索榜 ID/), { target: { value: "" } });
    expect(screen.getByText("daily_final")).toBeTruthy();
  });

  // 写路径锚（§12.3）：行删确认走 useCommand——api 收到 SKIP 且成功路径不抛。
  it("行删除确认走 useCommand（skipToast + 结果 ok 不抛）", async () => {
    mockListBoards.mockResolvedValue([boards[0]]);
    mockDeleteBoard.mockResolvedValue(undefined);
    await renderList();
    await screen.findByText("daily_final");
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    // torchwood 存量确认壳：确认钮与触发钮同名，scope 到对话框内
    const dialog = await screen.findByRole("dialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));
    await waitFor(() =>
      expect(mockDeleteBoard).toHaveBeenCalledWith("daily_final", { __skipToast: true }),
    );
  });
});
