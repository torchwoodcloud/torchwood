import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type Predicate,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  listOrders,
  manualFulfillOrder,
  refundOrder,
  type PaymentOrder,
} from "@/api/payments";
import { runCommandMapped, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：订单列表换 useCollection——过滤（UserID/状态/
// 创建时间区间）/时间排序/游标/页大小进 URL，数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError。订单 ID/用户的页内搜索是本地态，不走谓词。
//
// 命令族切片（写路径命令化，§12.3）：退款/人工履约挂订单资源——订单实体
// 状态机变更，kit 集合键由 useCommand 自动失效；存量 ["payments-orders"]
// 键（详情仍消费）由页面 onSettled 手动桥。结果两分映射同 apiKeys 先例
//（400→rejected{code,message}、409→conflict、其余 throw ApiError）；api
// 调用一律 __skipToast（toast 单源 = onSettled）。

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
  commands: {
    // 退款（paid → refunded）；reason 缺省 console（操作审计标记）。
    refund: {
      run: async (input: { id: string; reason?: string }): Promise<CommandResult<PaymentOrder>> =>
        runCommandMapped(() =>
          refundOrder(input.id, { reason: input.reason ?? "console" }, SKIP)
        ),
    },
    // 人工履约（paid 订单补标）；payload 解包 { order } 投影订单实体。
    fulfill: {
      run: async (input: { id: string; reason?: string }): Promise<CommandResult<PaymentOrder>> =>
        runCommandMapped(async () => {
          const res = await manualFulfillOrder(input.id, input.reason ?? "console", SKIP);
          return res.order;
        }),
    },
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const refundOrderCommand = ordersResource.commands!.refund;
export const fulfillOrderCommand = ordersResource.commands!.fulfill;

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认。
export const ordersSearch = collectionQuerySchema({
  filters: FILTER_FIELDS.map((field) => ({ field })),
  defaultSize: 20,
});
