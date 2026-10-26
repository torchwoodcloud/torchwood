import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { createAssetDef, deleteAssetDef, listAssetDefs, type AssetDef } from "@/api/assets";
import { skipToast as SKIP } from "@/api/client";
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：资产定义主列表换 useCollection——游标/页大小/
// 时间排序进 URL（分页栈 = 浏览器历史），数据面复用 axios 管线，失败在
// 适配器收敛为 ApiError（kit/errors.ts）。code/名称/类别是页内搜索
// （本地态），不是服务端过滤参数 → 无谓词白名单。
//
// 命令族切片（写路径命令化，§12.3）：建/删挂资源 commands（kit 集合键由
// useCommand 自动失效）；存量 ["asset-defs"] 前缀键（详情/下拉选项仍消费）
// 由资源 legacyKeys 声明桥（§12.10-②，页面 onSettled 手动桥退役）。结果
// 两分映射同 apiKeys 先例（400→rejected {code,message}、409→conflict、
// 其余 throw ApiError）；api 调用一律 __skipToast（toast 单源 = onSettled）。
// 创建的 decimals/上限文本展示形在 run 内收敛为提交形。

// —— 新建：decimals/上限为文本展示形，run 内收敛提交形（decimals 非法回退
// 0；max_quantity 空 = 不限不发送）——

export interface CreateAssetDefForm {
  code: string;
  name: string;
  class: string;
  decimalsText: string;
  /** 空 = 不限。 */
  maxQuantityText: string;
}

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
  // 存量键失效桥（§12.10-②）：宽前缀覆盖 ["asset-defs", projectId]（列表桥）
  // 与 ["asset-defs", id]（详情键）两形。
  legacyKeys: () => [["asset-defs"]],
  commands: {
    create: {
      run: async (form: CreateAssetDefForm): Promise<CommandResult<AssetDef>> =>
        runCommandMapped(() =>
          createAssetDef(
            {
              code: form.code,
              name: form.name,
              class: form.class,
              decimals: Number.parseInt(form.decimalsText, 10) || 0,
              max_quantity: form.maxQuantityText === "" ? undefined : form.maxQuantityText,
            },
            SKIP
          )
        ),
    },
    // 删除已删 → 幂等 ok（apiKeys remove 先例）
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteAssetDef(input.id, SKIP);
          return null;
        }),
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
  },
});

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createAssetDefCommand = assetDefsResource.commands.create;
export const removeAssetDefCommand = assetDefsResource.commands.remove;
