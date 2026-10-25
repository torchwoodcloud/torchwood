import {
  defineResource,
  type CollectionQuery,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listAdmins, type Admin } from "@/api/admins";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：admins 是平台级资源（GET /console/admins，旧页
// 不随项目作用域）——defineResource 不设 scope，直接请求；游标/页大小进
// URL（旧页无服务端过滤——不设 filters）。数据面复用 axios 管线，失败在
// 适配器收敛为 ApiError（fleet/errors.ts）。写路径（create/update/delete）
// 仍走存量 useMutation——kit 列表键失效经 adminsCollectionKey 手动桥
//（命令族切片后由 useCommand 自动失效取代）。

// 平台级域无 scope 约定：ctx 恒为 {}，签名只收 q。
async function fetchAdmins(q: CollectionQuery): Promise<Segment<Admin>> {
  try {
    const page = await listAdmins({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const adminsResource = defineResource({
  name: "admins",
  getId: (row: Admin) => row.id,
  collection: fetchAdmins,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const adminsSearch = collectionQuerySchema({ defaultSize: 20 });

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：存量 useMutation 写路径的手动失效桥。
export const adminsCollectionKey = ["collection", "admins"] as const;
