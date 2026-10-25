import {
  defineResource,
  type CollectionQuery,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listGroups, type Group } from "@/api/groups";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：groups 列表换 useCollection——游标/页大小进 URL
//（旧页无服务端过滤——不设 filters）；数据面复用 axios 管线（刷新/项目头），
// 失败在适配器收敛为 ApiError（fleet/errors.ts）。删除等写路径仍走存量
// useMutation——kit 列表键失效经 groupsCollectionKey 手动桥（命令族切片
// 后由 useCommand 自动失效取代）。

async function fetchGroups(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Group>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listGroups({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const groupsResource = defineResource({
  name: "groups",
  getId: (row: Group) => row.id,
  scope: ["projectId"],
  collection: fetchGroups,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const groupsSearch = collectionQuerySchema({ defaultSize: 20 });

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：存量 useMutation 删除后的手动失效桥。
export const groupsCollectionKey = ["collection", "groups"] as const;
