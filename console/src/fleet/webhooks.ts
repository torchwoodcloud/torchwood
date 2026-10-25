import {
  type CollectionQuery,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listWebhooks, type Webhook } from "@/api/webhooks";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：Webhooks 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts 收敛缝）。

async function fetchWebhooks(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Webhook>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listWebhooks({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const webhooksResource = defineResource({
  name: "webhooks",
  getId: (row: Webhook) => row.id,
  scope: ["projectId"],
  collection: fetchWebhooks,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；订阅列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const webhooksSearch = collectionQuerySchema({ defaultSize: 20 });
