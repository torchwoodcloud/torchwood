import {
  type CollectionQuery,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listEventDefinitions, type AnalyticsEventDefinition } from "@/api/analytics";
import { toApiError } from "@/kit/errors";

// analytics 事件字典的列表 fleet 化（§12.9-2）：数据面换 useCollection（游标/
// 页大小进 URL），事件字典量级小（近 30 天活跃事件名），defaultSize 100 沿旧
// 页单页规格——旧页的"加载更多"无限累积收敛为 keyset 分页（行为等价：百条
// 内单页即全量）。

export const eventDefinitionsSearch = collectionQuerySchema({ defaultSize: 100 });

async function fetchEventDefinitions(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<AnalyticsEventDefinition>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const res = await listEventDefinitions(q.window.size, q.window.cursor || undefined);
    return { rows: res.definitions ?? [], nextCursor: res.meta?.next_page_token };
  } catch (err) {
    throw toApiError(err);
  }
}

export const eventDefinitionsResource = defineResource({
  name: "event-definitions",
  getId: (row: AnalyticsEventDefinition) => row.name,
  scope: ["projectId"],
  collection: fetchEventDefinitions,
});
