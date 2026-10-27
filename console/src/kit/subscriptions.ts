import {
  type CommandResult,
  defineResource,
  type CollectionQuery,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema, filterValues } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  cancelSubscription,
  createPlan,
  deletePlan,
  expireSubscription,
  listPlans,
  listSubscriptions,
  type Subscription,
  type SubscriptionPlan,
} from "@/api/subscriptions";
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：订阅域两个列表（plans / subscriptions）各换
// useCollection——过滤/时间排序/游标/页大小进 URL，数据面复用 axios 管线，
// 失败在适配器收敛为 ApiError。code/名称页内搜索是本地态，不走谓词。
//
// 命令族切片（写路径命令化）：计划建/删挂 plansResource、订阅期末取消/强制
// 过期挂 subscriptionsResource（kit 列表键随 ok 自动失效）；存量
// ["sub-plans"] / ["subscriptions"] 前缀键由资源 legacyKeys 声明桥
//（§12.10-②，页面 onSettled 手动桥退役）。结果两分映射同 apiKeys
//（400→rejected、409→conflict、其余 throw ApiError）；api 调用一律
// __skipToast（toast 单源 = onSettled/结果盒）。

// —— 计划新建：表单值（展示形）在 run 内收敛为提交形 ——
//（grace_days 文本 → 整数回退 0；amount 为最小单位整数字符串——客户端
// rejected 校验在 run 内，非 toast 抢跑）。

export interface CreatePlanForm {
  code: string;
  name: string;
  amount: string;
  currency: string;
  interval: string;
  /** 展示形文本（默认 "3"）。 */
  graceDays: string;
}

const INT64_INPUT = /^-?\d+$/;

/** 表单收敛：amount 非法 → rejected（结果分支，不占失败通道）。 */
function planPayloadOf(form: CreatePlanForm):
  | {
      code: string;
      name: string;
      amount: string;
      currency: string;
      interval: string;
      grace_days: number;
    }
  | { rejected: CommandResult<never> } {
  if (form.amount === "" || !INT64_INPUT.test(form.amount)) {
    return {
      rejected: { status: "rejected", message: "Amount must be an integer in the smallest currency unit" },
    };
  }
  return {
    code: form.code,
    name: form.name,
    amount: form.amount,
    currency: form.currency,
    interval: form.interval,
    grace_days: Number.parseInt(form.graceDays, 10) || 0,
  };
}

// —— plans：无服务端过滤参数（仅分页 + 时间排序）——

// URL 搜索 schema。默认页大小 20 = 旧页默认。
export const plansSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchPlans(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<SubscriptionPlan>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    // 无 order = 服务端历史默认 DESC（不设 schema defaultOrder：DataTable 的
    // none→asc→desc 循环要能落回"无排序"态，默认序会把它钉死在 desc）。
    const dir = q.order?.[0]?.field === "created_at" ? q.order[0].dir : undefined;
    const page = await listPlans({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      sortOrder: dir === "asc" ? "ASC" : dir === "desc" ? "DESC" : undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const plansResource = defineResource({
  name: "subscription-plans",
  getId: (row: SubscriptionPlan) => row.id,
  scope: ["projectId"],
  collection: fetchPlans,
  // 存量键失效桥（§12.10-②）：宽前缀覆盖 ["sub-plans", projectId]（列表桥）
  // 与 ["sub-plans", id]（详情键）两形。
  legacyKeys: () => [["sub-plans"]],
  commands: {
    // 表单值→提交形收敛在 run 内（amount int64 校验、grace_days 文本→整数）
    create: {
      run: async (form: CreatePlanForm): Promise<CommandResult<SubscriptionPlan>> => {
        const payload = planPayloadOf(form);
        if ("rejected" in payload) return payload.rejected;
        return runCommandMapped(() => createPlan(payload, SKIP));
      },
    },
    // 删除已删 → 幂等 ok（apiKeys/documents remove 先例）
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deletePlan(input.id, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// —— subscriptions：与订单同形的服务端过滤（ListSubscriptionsRequest）——

// 谓词 eq 叶子 → ListSubscriptionsFilter 键的白名单（真实服务端过滤参数）；
// created_after/created_before 在 URL 存绝对时刻（ISO），页面在写入边界做
// 墙钟↔绝对换算。
const FILTER_FIELDS = ["user_id", "status", "created_after", "created_before"] as const;

type FilterField = (typeof FILTER_FIELDS)[number];
export type { FilterField };

export function filtersOf(
  predicate: Predicate | undefined
): Partial<Record<FilterField, string>> {
  // kit filterValues 逐叶抽 field→单值（AND/OR 展开同 codec 产物形）；白名单
  // 与字符串值筛选（真实服务端参数面）留在域层。
  const values = filterValues(predicate);
  const out: Partial<Record<FilterField, string>> = {};
  for (const field of FILTER_FIELDS) {
    const v = values[field];
    if (typeof v === "string") out[field] = v;
  }
  return out;
}

async function fetchSubscriptions(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Subscription>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const f = filtersOf(q.predicate);
    const dir = q.order?.[0]?.field === "created_at" ? q.order[0].dir : undefined;
    const page = await listSubscriptions({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      sortOrder: dir === "asc" ? "ASC" : dir === "desc" ? "DESC" : undefined,
      userId: f.user_id,
      status: f.status,
      createdAfter: f.created_after,
      createdBefore: f.created_before,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const subscriptionsResource = defineResource({
  name: "subscriptions",
  getId: (row: Subscription) => row.id,
  scope: ["projectId"],
  collection: fetchSubscriptions,
  // 存量键失效桥（§12.10-②）：宽前缀覆盖 ["subscriptions", projectId]
  //（列表桥）与 ["subscriptions", id]（详情键）两形。
  legacyKeys: () => [["subscriptions"]],
  commands: {
    // 期末取消（custom verb，reason = "console" 操作审计）
    cancel: {
      run: async (input: {
        id: string;
        reason?: string;
      }): Promise<CommandResult<Subscription>> =>
        runCommandMapped(() => cancelSubscription(input.id, input.reason, SKIP)),
    },
    // 强制过期（destructive）
    expire: {
      run: async (input: {
        id: string;
        reason?: string;
      }): Promise<CommandResult<Subscription>> =>
        runCommandMapped(() => expireSubscription(input.id, input.reason, SKIP)),
    },
  },
});

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createPlanCommand = plansResource.commands.create;
export const removePlanCommand = plansResource.commands.remove;
export const cancelSubscriptionCommand = subscriptionsResource.commands.cancel;
export const expireSubscriptionCommand = subscriptionsResource.commands.expire;

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认。
export const subscriptionsSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 20,
});
