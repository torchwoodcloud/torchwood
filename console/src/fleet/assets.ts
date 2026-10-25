import {
  defineResource,
  type CollectionQuery,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listAssetDefs, type AssetDef } from "@/api/assets";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：资产定义主列表换 useCollection——游标/页大小/
// 时间排序进 URL（分页栈 = 浏览器历史），数据面复用 axios 管线，失败在
// 适配器收敛为 ApiError（fleet/errors.ts）。code/名称/类别是页内搜索
// （本地态），不是服务端过滤参数 → 无谓词白名单。

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认。
export const assetDefsSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchAssetDefs(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<AssetDef>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    // 无 order = 服务端历史默认 DESC（与旧页常传 DESC 等价；不设 schema
    // defaultOrder：DataTable 的 none→asc→desc 循环要能落回"无排序"态）。
    const dir = q.order?.[0]?.field === "created_at" ? q.order[0].dir : undefined;
    const page = await listAssetDefs({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      sortOrder: dir === "asc" ? "ASC" : dir === "desc" ? "DESC" : undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const assetDefsResource = defineResource({
  name: "asset-defs",
  getId: (row: AssetDef) => row.id,
  scope: ["projectId"],
  collection: fetchAssetDefs,
});
