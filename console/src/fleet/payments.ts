import {
  defineResource,
  type CollectionQuery,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listOrders, type PaymentOrder } from "@/api/payments";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：订单列表换 useCollection——过滤（UserID/状态/
// 创建时间区间）/时间排序/游标/页大小进 URL，数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError。订单 ID/用户的页内搜索是本地态，不走谓词。

// 谓词 eq 叶子 → ListOrdersFilter 键的白名单（真实服务端过滤参数，
// payments.proto ListOrdersRequest 结构化字段）；created_after/created_before
// 在 URL 存绝对时刻（ISO），页面在写入边界做墙钟↔绝对换算。
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

async function fetchOrders(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<PaymentOrder>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const f = filtersOf(q.predicate);
    // 无 order = 服务端历史默认 DESC（不设 schema defaultOrder：DataTable 的
    // none→asc→desc 循环要能落回"无排序"态，默认序会把它钉死在 desc）。
    const dir = q.order?.[0]?.field === "created_at" ? q.order[0].dir : undefined;
    const page = await listOrders({
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

export const ordersResource = defineResource({
  name: "orders",
  getId: (row: PaymentOrder) => row.id,
  scope: ["projectId"],
  collection: fetchOrders,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认。
export const ordersSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 20,
});
