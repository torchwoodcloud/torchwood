import {
  type CollectionQuery,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listFunctions, type FunctionItem } from "@/api/functions";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：Functions 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts 收敛缝）。

async function fetchFunctions(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<FunctionItem>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listFunctions({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const functionsResource = defineResource({
  name: "functions",
  getId: (row: FunctionItem) => row.id,
  scope: ["projectId"],
  collection: fetchFunctions,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；函数列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const functionsSearch = collectionQuerySchema({ defaultSize: 20 });
