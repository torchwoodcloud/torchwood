import {
  defineResource,
  type CollectionQuery,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  listPlans,
  listSubscriptions,
  type Subscription,
  type SubscriptionPlan,
} from "@/api/subscriptions";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：订阅域两个列表（plans / subscriptions）各换
// useCollection——过滤/时间排序/游标/页大小进 URL，数据面复用 axios 管线，
// 失败在适配器收敛为 ApiError。code/名称页内搜索是本地态，不走谓词。

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
  if (!predicate) return {};
  const leaves = "all" in predicate ? predicate.all : "field" in predicate ? [predicate] : [];
  const out: Partial<Record<FilterField, string>> = {};
  for (const leaf of leaves) {
    if (!("field" in leaf)) continue;
    const field = leaf.field as FilterField;
    if ((FILTER_FIELDS as readonly string[]).includes(field) && typeof leaf.value === "string") {
      out[field] = leaf.value;
    }
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
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认。
export const subscriptionsSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 20,
});
