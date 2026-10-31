import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  createBoard,
  deleteBoard,
  deleteBoardEntry,
  getBoard,
  getSettlement,
  listBoardPeriods,
  listBoardTop,
  listBoards,
  listSettlements,
  rerunSettlement,
  updateBoard,
  voidSettlement,
  type CreateBoardInput,
  type LeaderboardBoard,
  type LeaderboardSettlement,
  type LeaderboardSettlementGrant,
  type LeaderboardTopEntry,
} from "@/api/leaderboards";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：排行榜主列表换 useCollection。 boards 端点
// （/console/leaderboards/boards）非分页——一次返回全量，无 next_page_token、
// 无服务端过滤/排序参数 → 适配器忽略窗口/谓词，Segment 不带 nextCursor
//（块按"已到底"降级；页面不渲染分页控件）。榜 ID 搜索是页内本地态。
// 旧页无 projectId 门（fetch 不随项目禁用）→ 适配器不设空段短路，保持
// 现页行为；scope 仍挂 projectId（项目切换 = 缓存分区）。
//
// 命令族切片（§12.3）：榜建/改/删挂资源 commands（kit 集合键由 useCommand
// 自动失效——现页 refresh 桥退场）；条目删除/结算弃奖/重跑是榜子资源写、
// 不改变榜集合——独立 descriptor，失效 = invalidates 声明（子视图 kit 化批：
// 存量 ["leaderboards-top"/"-periods"/"-settlements"] 前缀宽桥随详情读换轨
// kit 键退役，按各命令真实变更面收窄）。结果两分映射同 apiKeys 先例
//（400→rejected{code,message}、409→conflict、其余 throw ApiError）；命令
// 调用一律 __skipToast（toast 单源 = onSettled）。

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
  // 详情读（BoardDetailPage，子视图 kit 化批——反身缺口#1 闭合：旧详情读
  // ["leaderboards-board", boardId] 无失效桥，updateBoard ok 后详情不刷新；
  // 现为 kit record 键，榜资源命令缺省同资源失效全 kind 命中）：404 → null
  // 空态直出（recordOrNull 手写方言，不引 restRecord）。
  record: (id) => recordOrNull(() => getBoard(id)) as Promise<LeaderboardBoard>,
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

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createBoardCommand = boardsResource.commands.create;
export const updateBoardCommand = boardsResource.commands.update;
export const removeBoardCommand = boardsResource.commands.remove;

// ——榜子视图读（子视图 kit 化批：期列表 / top 分页榜 / 结算列表 + 详情读）——

// 期列表：端点整取（返回已有条目的期 key 降序，无分页无过滤）→ 适配器忽略
// 窗口；行 = string（期 key 即身份）。
async function fetchBoardPeriods(
  _q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<string>> {
  if (ctx.projectId == null || ctx.boardId == null) return { rows: [] };
  try {
    return { rows: await listBoardPeriods(ctx.boardId) };
  } catch (err) {
    throw toApiError(err);
  }
}

export const boardPeriodsResource = defineResource({
  name: "leaderboards-periods",
  getId: (period: string) => period,
  scope: ["projectId", "boardId"],
  collection: fetchBoardPeriods,
});

// top 榜：服务端分页（offset 型 page_token）+ 期谓词；period 由页面放进
// scope 对象（不进声明键——对象全键进 queryKey，声明键只定就绪门与失效
// 投影轴，§12.10-③），适配器经 ctx.period 消费（空 = 服务端默认期，与旧页
// `activePeriod || undefined` 同形）。
async function fetchBoardTop(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<LeaderboardTopEntry>> {
  if (ctx.projectId == null || ctx.boardId == null) return { rows: [] };
  try {
    const page = await listBoardTop(ctx.boardId, {
      period: ctx.period || undefined,
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    // total = 该期真实总条数（api 层已解包），页面统计条直出。
    return { rows: page.rows, nextCursor: page.nextPageToken, total: page.total };
  } catch (err) {
    throw toApiError(err);
  }
}

export const boardTopResource = defineResource({
  name: "leaderboards-top",
  getId: (row: LeaderboardTopEntry) => row.subject_id,
  scope: ["projectId", "boardId"],
  collection: fetchBoardTop,
});

// getSettlement 详情信封（结算行展开读消费 grants）。kit 单 T 资源下与
// collection 行类型（扁平 settlement）不同形——信封经声明点断言收编（见
// boardSettlementsResource.record）。
export interface LeaderboardSettlementDetail {
  settlement: LeaderboardSettlement;
  grants: LeaderboardSettlementGrant[];
}

// 结算列表：旧页 listSettlements(boardId, 50) 固定 limit 50 语义在适配器内
// 保真（端点非分页，适配器不消费窗口）。
async function fetchBoardSettlements(
  _q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<LeaderboardSettlement>> {
  if (ctx.projectId == null || ctx.boardId == null) return { rows: [] };
  try {
    return { rows: await listSettlements(ctx.boardId, 50) };
  } catch (err) {
    throw toApiError(err);
  }
}

export const boardSettlementsResource = defineResource({
  name: "leaderboards-settlements",
  getId: (row: LeaderboardSettlement) => row.period,
  scope: ["projectId", "boardId"],
  collection: fetchBoardSettlements,
  // 详情读（SettlementRow 展开行——对话框内读，子视图 kit 化批——反身缺口#2
  // 闭合：旧详情读 ["leaderboards-settlement-detail", ...] 无失效桥，void/
  // rerun ok 后展开行不刷新；现为 kit record 键，结算命令 invalidates 全 kind
  // 命中）：404 → null 空态直出。kit 单 T 资源（collection/record 共享行类型）
  // 下 getSettlement 信封与行类型不同形——单次断言收编为行类型，页面按信封
  // 形投影 grants（LeaderboardSettlementDetail）。ctx.boardId 由 scopeReady
  // 就绪门保证，! 仅类型收窄。
  record: (id, ctx) =>
    recordOrNull(() => getSettlement(ctx.boardId!, id)) as unknown as Promise<LeaderboardSettlement>,
});

// ——榜子资源命令（独立 descriptor：条目/结算写不改变榜集合；失效 =
// invalidates 声明（子视图 kit 化批：存量 ["leaderboards-*"] 前缀宽桥退役，
// 按真实变更面收窄——详情读已全部换轨 kit 键）。裸 descriptor 无 __resource
// 锚（无缺省失效、不采纳 Provider context）→ 页面显式传全量 scope（projectId
// 是环境态不在 input——half-scope 陷阱，scopeOf 只覆盖一半不可用）。——

// 条目删除：影响 top 榜行与期集合（ListPeriods 由已有条目派生，删空即消失）；
// 结算面无关——不再连带 leaderboards-settlements。
export const removeBoardEntryDescriptor = {
  invalidates: [{ resource: "leaderboards-top" }, { resource: "leaderboards-periods" }],
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

// 弃奖/重跑：只动结算面（列表 collection + 详情 record 全 kind；反身缺口#2
// 由 record kind 命中闭合）；榜行/期无关——不再连带 top/periods。
export const voidSettlementDescriptor = {
  invalidates: [{ resource: "leaderboards-settlements" }],
  run: async (input: { boardId: string; period: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await voidSettlement(input.boardId, input.period, SKIP);
      return null;
    }),
};

export const rerunSettlementDescriptor = {
  invalidates: [{ resource: "leaderboards-settlements" }],
  run: async (input: { boardId: string; period: string }): Promise<CommandResult<null>> =>
    runCommandMapped(async () => {
      await rerunSettlement(input.boardId, input.period, SKIP);
      return null;
    }),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。端点全量返回
// → 无过滤白名单；size 仅作 schema 形参（适配器不消费）。
export const boardsSearch = collectionQuerySchema({ defaultSize: 20 });
