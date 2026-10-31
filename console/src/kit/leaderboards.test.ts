import { AxiosError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { CommandDescriptor } from "@lynx-go/uikit-core/kernel";
import {
  getBoard,
  getSettlement,
  listBoardPeriods,
  listBoardTop,
  listSettlements,
} from "@/api/leaderboards";
import {
  boardPeriodsResource,
  boardSettlementsResource,
  boardTopResource,
  boardsResource,
  removeBoardEntryDescriptor,
  rerunSettlementDescriptor,
  voidSettlementDescriptor,
} from "@/kit/leaderboards";

// 详情读 record 槽与子视图 collection 槽（子视图 kit 化批）：404 → null 空态
// 直出（recordOrNull 手写方言）；其余失败 toApiError 上抛；嵌套 ctx 参数由
// scope 传入（声明键 boardId + 活跃参数 period 只进对象）。

vi.mock("@/api/leaderboards", () => ({
  createBoard: vi.fn(),
  deleteBoard: vi.fn(),
  deleteBoardEntry: vi.fn(),
  getBoard: vi.fn(),
  getBoardEntry: vi.fn(),
  getSettlement: vi.fn(),
  listBoards: vi.fn(),
  listBoardPeriods: vi.fn(),
  listBoardTop: vi.fn(),
  listSettlements: vi.fn(),
  rerunSettlement: vi.fn(),
  updateBoard: vi.fn(),
  voidSettlement: vi.fn(),
}));

function httpError(status: number, message: string, code = "NotFound"): AxiosError {
  return new AxiosError("Request failed", "ERR_BAD_REQUEST", undefined, undefined, {
    status,
    statusText: "",
    headers: {},
    config: {} as never,
    data: { error: { code, message } },
  } as never);
}

beforeEach(() => {
  vi.mocked(getBoard).mockReset();
  vi.mocked(getSettlement).mockReset();
  vi.mocked(listBoardPeriods).mockReset();
  vi.mocked(listBoardTop).mockReset();
  vi.mocked(listSettlements).mockReset();
});

describe("boardsResource.record", () => {
  it("读取透传；404 → null；其余失败如实上抛", async () => {
    vi.mocked(getBoard).mockResolvedValue({ id: "daily_final" } as never);
    expect(await boardsResource.record!("daily_final", { projectId: "p1" })).toEqual({
      id: "daily_final",
    });

    vi.mocked(getBoard).mockRejectedValue(httpError(404, "gone"));
    expect(await boardsResource.record!("gone", { projectId: "p1" })).toBeNull();

    vi.mocked(getBoard).mockRejectedValue(httpError(500, "boom", "Internal"));
    await expect(boardsResource.record!("x", { projectId: "p1" })).rejects.toEqual({
      kind: "server",
      status: 500,
      message: "boom",
    });
  });
});

describe("boardPeriodsResource.collection", () => {
  it("ctx.boardId 进适配器；期 key 整取直出", async () => {
    vi.mocked(listBoardPeriods).mockResolvedValue(["2026-10-30", "2026-10-29"]);
    const seg = await boardPeriodsResource.collection!(
      { window: { size: 20 } },
      { projectId: "p1", boardId: "daily_final" }
    );
    expect(vi.mocked(listBoardPeriods)).toHaveBeenCalledWith("daily_final");
    expect(seg).toEqual({ rows: ["2026-10-30", "2026-10-29"] });
  });

  it("缺参静默空段（不发起请求）", async () => {
    expect(
      await boardPeriodsResource.collection!({ window: { size: 20 } }, { projectId: "p1" })
    ).toEqual({ rows: [] });
    expect(vi.mocked(listBoardPeriods)).not.toHaveBeenCalled();
  });
});

describe("boardTopResource.collection", () => {
  it("period 经 scope 对象进适配器（非声明键）；窗口映射 pageSize/pageToken；total 透传", async () => {
    vi.mocked(listBoardTop).mockResolvedValue({
      rows: [{ subject_id: "u1", value: "10", rank: 1, position: 1 }],
      nextPageToken: "tok-2",
      period: "2026-10-30",
      total: 42,
    });
    const seg = await boardTopResource.collection!(
      { window: { size: 20, cursor: "tok-1" } },
      { projectId: "p1", boardId: "daily_final", period: "2026-10-30" }
    );
    expect(vi.mocked(listBoardTop)).toHaveBeenCalledWith("daily_final", {
      period: "2026-10-30",
      pageSize: 20,
      pageToken: "tok-1",
    });
    expect(seg).toEqual({
      rows: [{ subject_id: "u1", value: "10", rank: 1, position: 1 }],
      nextCursor: "tok-2",
      total: 42,
    });
  });

  it("空 period → 不传（服务端默认期，与旧页 `activePeriod || undefined` 同形）", async () => {
    vi.mocked(listBoardTop).mockResolvedValue({ rows: [], period: "", total: 0 });
    await boardTopResource.collection!(
      { window: { size: 20 } },
      { projectId: "p1", boardId: "b1", period: "" }
    );
    expect(vi.mocked(listBoardTop)).toHaveBeenCalledWith("b1", {
      period: undefined,
      pageSize: 20,
      pageToken: undefined,
    });
  });

  it("缺参静默空段（不发起请求）", async () => {
    expect(
      await boardTopResource.collection!({ window: { size: 20 } }, { projectId: "p1" })
    ).toEqual({ rows: [] });
    expect(vi.mocked(listBoardTop)).not.toHaveBeenCalled();
  });
});

describe("boardSettlementsResource.collection", () => {
  it("固定 limit 50 语义保真（端点非分页，窗口不消费）", async () => {
    vi.mocked(listSettlements).mockResolvedValue([
      { board_id: "b1", period: "2026-10-30", status: "settled" },
    ]);
    const seg = await boardSettlementsResource.collection!(
      { window: { size: 20 } },
      { projectId: "p1", boardId: "b1" }
    );
    expect(vi.mocked(listSettlements)).toHaveBeenCalledWith("b1", 50);
    expect(seg.rows).toEqual([{ board_id: "b1", period: "2026-10-30", status: "settled" }]);
  });

  it("缺参静默空段（不发起请求）", async () => {
    expect(
      await boardSettlementsResource.collection!({ window: { size: 20 } }, { boardId: "b1" })
    ).toEqual({ rows: [] });
    expect(vi.mocked(listSettlements)).not.toHaveBeenCalled();
  });
});

describe("boardSettlementsResource.record", () => {
  it("ctx.boardId 进适配器（id = period）；404 → null", async () => {
    vi.mocked(getSettlement).mockResolvedValue({
      settlement: { board_id: "b1", period: "2026-10-30", status: "settled" },
      grants: [],
    } as never);
    const rec = await boardSettlementsResource.record!("2026-10-30", {
      projectId: "p1",
      boardId: "b1",
    });
    expect(vi.mocked(getSettlement)).toHaveBeenCalledWith("b1", "2026-10-30");
    expect(rec).toEqual({
      settlement: { board_id: "b1", period: "2026-10-30", status: "settled" },
      grants: [],
    });

    vi.mocked(getSettlement).mockRejectedValue(httpError(404, "gone"));
    expect(
      await boardSettlementsResource.record!("gone", { projectId: "p1", boardId: "b1" })
    ).toBeNull();
  });
});

describe("榜子资源命令的失效声明（legacyKeys 宽桥退役 → invalidates 收窄）", () => {
  it("条目删除 → top + periods（期集合由条目派生）；结算面剔除", () => {
    expect(removeBoardEntryDescriptor.invalidates).toEqual([
      { resource: "leaderboards-top" },
      { resource: "leaderboards-periods" },
    ]);
  });
  it("弃奖/重跑 → settlements 全 kind（列表 + 详情 record，反身缺口#2 闭合）", () => {
    expect(voidSettlementDescriptor.invalidates).toEqual([
      { resource: "leaderboards-settlements" },
    ]);
    expect(rerunSettlementDescriptor.invalidates).toEqual([
      { resource: "leaderboards-settlements" },
    ]);
  });
  it("legacy 前缀桥退役：descriptor 级声明已清零", () => {
    // 裸 descriptor 的推断类型只保留字面量键——缺席断言走声明面视图。
    const declarationOf = (d: Pick<CommandDescriptor<never, never>, "invalidates" | "legacyKeys">) =>
      d.legacyKeys;
    expect(declarationOf(removeBoardEntryDescriptor)).toBeUndefined();
    expect(declarationOf(voidSettlementDescriptor)).toBeUndefined();
    expect(declarationOf(rerunSettlementDescriptor)).toBeUndefined();
  });
});
