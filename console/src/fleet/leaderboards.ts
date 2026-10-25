import {
  type CommandResult,
  defineResource,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  listBoards,
  createBoard,
  updateBoard,
  deleteBoard,
  deleteBoardEntry,
  voidSettlement,
  rerunSettlement,
  type CreateBoardInput,
  type LeaderboardBoard,
} from "@/api/leaderboards";
import { runCommandMapped, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：排行榜主列表换 useCollection。 boards 端点
// （/console/leaderboards/boards）非分页——一次返回全量，无 next_page_token、
// 无服务端过滤/排序参数 → 适配器忽略窗口/谓词，Segment 不带 nextCursor
//（块按"已到底"降级；页面不渲染分页控件）。榜 ID 搜索是页内本地态。
// 旧页无 projectId 门（fetch 不随项目禁用）→ 适配器不设空段短路，保持
// 现页行为；scope 仍挂 projectId（项目切换 = 缓存分区）。
//
// 命令族切片（§12.3）：榜建/改/删挂资源 commands（kit 集合键由 useCommand
// 自动失效——现页 refresh 桥退场）；条目删除/结算弃奖/重跑是榜子资源写、
// 不改变榜集合——独立 descriptor，失效由页面桥存量
// ["leaderboards-top"/"-periods"/"-settlements"] 键。结果两分映射同 apiKeys
// 先例（400→rejected{code,message}、409→conflict、其余 throw ApiError）；
// 命令调用一律 __skipToast（toast 单源 = onSettled）。

async function fetchBoards(): Promise<Segment<LeaderboardBoard>> {
  try {
    const boards = await listBoards();
    return { rows: boards };
  } catch (err) {
    throw toApiError(err);
  }
}

export const boardsResource = defineResource({
  name: "leaderboards-boards",
  getId: (row: LeaderboardBoard) => row.id,
  scope: ["projectId"],
  collection: fetchBoards,
  commands: {
    create: {
      run: async (input: CreateBoardInput): Promise<CommandResult<LeaderboardBoard>> =>
        runCommandMapped(() => createBoard(input, SKIP)),
    },
    update: {
      run: async (input: {
        id: string;
      } & Partial<CreateBoardInput> & {
          clear_tiebreak?: boolean;
          clear_value_bounds?: boolean;
          sort?: string;
          tie_break?: string;
          period_kind?: string;
          policy?: string;
        }): Promise<CommandResult<LeaderboardBoard>> =>
        runCommandMapped(() => {
          const { id, ...rest } = input;
          return updateBoard(id, rest, SKIP);
        }),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteBoard(input.id, SKIP);
          return null;
        }),
      // delete 已删 → 幂等 ok 同族语义（apiKeys remove 先例）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createBoardCommand = boardsResource.commands!.create;
export const updateBoardCommand = boardsResource.commands!.update;
export const removeBoardCommand = boardsResource.commands!.remove;

// ——榜子资源命令（独立 descriptor：条目/结算写不改变榜集合；失效由页面桥
// 存量 ["leaderboards-*"] 键）——

export const removeBoardEntryDescriptor = {
  run: async (input: {
    boardId: string;
    period: string;
    subjectId: string;
  }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await deleteBoardEntry(input.boardId, input.period, input.subjectId, SKIP);
      return null;
    }),
  treatAs: { "not-found": { status: "ok" as const, payload: null } },
};

export const voidSettlementDescriptor = {
  run: async (input: { boardId: string; period: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await voidSettlement(input.boardId, input.period, SKIP);
      return null;
    }),
};

export const rerunSettlementDescriptor = {
  run: async (input: { boardId: string; period: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await rerunSettlement(input.boardId, input.period, SKIP);
      return null;
    }),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。端点全量返回
// → 无过滤白名单；size 仅作 schema 形参（适配器不消费）。
export const boardsSearch = collectionQuerySchema({ defaultSize: 20 });
