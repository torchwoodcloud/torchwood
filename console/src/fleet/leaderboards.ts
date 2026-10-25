import {
  defineResource,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listBoards, type LeaderboardBoard } from "@/api/leaderboards";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：排行榜主列表换 useCollection。 boards 端点
// （/console/leaderboards/boards）非分页——一次返回全量，无 next_page_token、
// 无服务端过滤/排序参数 → 适配器忽略窗口/谓词，Segment 不带 nextCursor
//（块按"已到底"降级；页面不渲染分页控件）。榜 ID 搜索是页内本地态。
// 旧页无 projectId 门（fetch 不随项目禁用）→ 适配器不设空段短路，保持
// 现页行为；scope 仍挂 projectId（项目切换 = 缓存分区）。

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
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。端点全量返回
// → 无过滤白名单；size 仅作 schema 形参（适配器不消费）。
export const boardsSearch = collectionQuerySchema({ defaultSize: 20 });
