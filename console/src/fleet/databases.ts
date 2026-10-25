import {
  type CollectionQuery,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import {
  listCollections,
  listDatabases,
  type Collection,
  type Database,
} from "@/api/databases";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2，databases 域）：databases 顶层列表 + Database 详情
// 内嵌的 collections 列表。游标/页大小进 URL（分页栈 = 浏览器历史）；两个
// 端点均无真实服务端过滤参数（仅分页）→ schema 不设 filters/text，页内搜索
// 是本地态投影不进 URL。数据面复用 axios 管线（刷新/项目头），失败在适配器
// 收敛为 ApiError（fleet/errors.ts）。

// —— databases（顶层列表；scope 只有 projectId）——

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页默认（useServerPaging 缺省）。
export const databasesSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchDatabases(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Database>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listDatabases({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const databasesResource = defineResource({
  name: "databases",
  getId: (row: Database) => row.id,
  scope: ["projectId"],
  collection: fetchDatabases,
});

// —— collections（Database 详情内嵌列表；嵌套 scope 加 dbId，ctx 缺参静默空段）——

export const collectionsSearch = collectionQuerySchema({ defaultSize: 20 });

async function fetchCollections(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Collection>> {
  if (ctx.projectId == null || ctx.dbId == null) return { rows: [] };
  try {
    const page = await listCollections(ctx.dbId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const collectionsResource = defineResource({
  name: "collections",
  getId: (row: Collection) => row.id,
  scope: ["projectId", "dbId"],
  collection: fetchCollections,
});
