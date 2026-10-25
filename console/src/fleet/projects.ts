import {
  defineResource,
  type CollectionQuery,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
import { listProjects, type Project } from "@/api/projects";
import { toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：projects 是平台级资源（GET /server/projects，
// 旧页不随项目作用域）——defineResource 不设 scope，直接请求；游标/页大小
// 进 URL（旧页无服务端过滤——不设 filters）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts）。

// 平台级域无 scope 约定：ctx 恒为 {}，签名只收 q。
async function fetchProjects(q: CollectionQuery): Promise<Segment<Project>> {
  try {
    const page = await listProjects({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const projectsResource = defineResource({
  name: "projects",
  getId: (row: Project) => row.id,
  collection: fetchProjects,
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const projectsSearch = collectionQuerySchema({ defaultSize: 20 });
