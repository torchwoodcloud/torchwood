import {
  defineResource,
  type CollectionQuery,
  type CommandResult,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import {
  createAdmin,
  deleteAdmin,
  listAdmins,
  updateAdmin,
  type Admin,
} from "@/api/admins";
import { skipToast as SKIP } from "@/api/client";
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）+ 命令族切片（§12.3，apiKeys 同款）：admins 是
// 平台级资源（GET /console/admins，旧页不随项目作用域）——defineResource
// 不设 scope，直接请求；游标/页大小进 URL（旧页无服务端过滤——不设
// filters）。数据面复用 axios 管线，失败在适配器收敛为 ApiError
//（kit/errors.ts）。写路径 useCommand：结果两分映射——400→rejected、
// 409→conflict、其余 throw ApiError；命令调用一律 __skipToast（toast 单源 =
// onSettled，防与 axios 拦截器双 toast）；kit 列表键自动失效（descriptor
// 挂 resource），存量键无消费方（旧 ["console-admins"] 列表键已随列表
// useCollection 化退役）——无需手动桥。

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
  commands: {
    create: {
      run: async (input: {
        email: string;
        password: string;
        role: string;
      }): Promise<CommandResult<Admin>> =>
        runCommandMapped(() => createAdmin(input, SKIP)),
    },
    // diff（角色变更/密码非空）在页面提交处收敛——输入自包含，无需工厂闭包。
    update: {
      run: async (input: {
        id: string;
        role?: string;
        password?: string;
      }): Promise<CommandResult<Admin>> =>
        runCommandMapped(() =>
          updateAdmin(input.id, { role: input.role, password: input.password }, SKIP),
        ),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteAdmin(input.id, SKIP);
          return null;
        }),
      // 已删 → 幂等 ok：删除目标消失视为已达成（apiKeys remove 同款）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小 20
// = 旧页默认；无服务端过滤参数——不设 filters。
export const adminsSearch = collectionQuerySchema({ defaultSize: 20 });

// kit 列表键前缀（键形 [kind, 资源名, …]，见 ui-kit keys.ts；前缀匹配即
// 全窗口/全谓词失效）：useCommand 命令已按 resource 自动失效；此键留给
// 命令式编排（N 条聚合等非 useCommand 面）的手动失效。
export const adminsCollectionKey = ["collection", "admins"] as const;

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createAdminCommand = adminsResource.commands.create;
export const updateAdminCommand = adminsResource.commands.update;
export const removeAdminCommand = adminsResource.commands.remove;
