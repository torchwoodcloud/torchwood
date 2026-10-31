import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import {
  createAssetDef,
  deleteAssetDef,
  getAssetDef,
  listAssetDefs,
  listDefHolders,
  listUserAssets,
  listUserLedger,
  type AssetDef,
  type AssetHolding,
  type AssetLedgerEntry,
} from "@/api/assets";
import { skipToast as SKIP } from "@/api/client";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：资产定义主列表换 useCollection——游标/页大小/
// 时间排序进 URL（分页栈 = 浏览器历史），数据面复用 axios 管线，失败在
// 适配器收敛为 ApiError（kit/errors.ts）。code/名称/类别是页内搜索
// （本地态），不是服务端过滤参数 → 无谓词白名单。
//
// 命令族切片（写路径命令化，§12.3）：建/删挂资源 commands（kit 集合键由
// useCommand 自动失效）；存量 ["asset-defs"] 前缀桥随子视图 kit 化退役
//（详情读换轨 record 键、下拉全量换轨 useCollectionData，读者清零）。
// 结果两分映射同 apiKeys 先例（400→rejected {code,message}、409→conflict、
// 其余 throw ApiError）；api 调用一律 __skipToast（toast 单源 = onSettled）。
// 创建的 decimals/上限文本展示形在 run 内收敛为提交形。
//
// 子视图 kit 化批新增三个伴读子列表资源（def-holders / user-assets /
// user-ledger）：零 URL 形态（useCollectionData），分页窗口由页面经
// useServerPaging 手动喂 window；过滤/主体参数进 scope 对象不进声明键——
// 对象全部键进 queryKey，声明键只定就绪门与失效投影轴。

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
  // 详情读（AssetDefDetailPage，子视图 kit 化批）：404 → null 空态直出
  //（recordOrNull 手写方言，不引 restRecord）。
  record: (id) => recordOrNull(() => getAssetDef(id)) as Promise<AssetDef>,
  // 存量键失效桥（§12.10-②）已退役（子视图 kit 化批）：["asset-defs"] /
  // ["asset-defs-all"] 前缀读者清零（详情读换轨 kit record 键、定义下拉
  // 全量换轨 useCollectionData——同资源缺省失效全 kind 命中）。
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

// —— 伴读子列表资源（子视图 kit 化批）：零 URL 形态（useCollectionData），
// 只读无命令；分页窗口由页面经 useServerPaging 手动喂 window；过滤/主体
// 参数进 scope 对象不进声明键（对象全部键进 queryKey，声明键只定就绪门与
// 失效投影轴）——守卫缺参静默空段 + toApiError 上抛，照 fetchCollections 形态。——

// 定义维度持有列表（AssetDefDetailPage 内嵌）：scope 声明 projectId/defId
//（就绪门 + 失效轴），owner 过滤进 scope 对象（键变化 = 换查询）。
async function fetchDefHolders(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<AssetHolding>> {
  if (ctx.projectId == null || ctx.defId == null) return { rows: [] };
  try {
    const page = await listDefHolders(ctx.defId, {
      ownerId: ctx.owner || undefined,
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const defHoldersResource = defineResource({
  name: "def-holders",
  scope: ["projectId", "defId"],
  collection: fetchDefHolders,
});

// 用户持有列表（UserAssetsPage 持有 tab）：owner 是查询主体但非声明键
//（就绪门由 projectId + 页面 enabled: !!queryOwner 手动门共同承担）。
async function fetchUserAssets(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<AssetHolding>> {
  if (ctx.projectId == null || ctx.owner == null) return { rows: [] };
  try {
    const page = await listUserAssets(ctx.owner, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const userAssetsResource = defineResource({
  name: "user-assets",
  scope: ["projectId"],
  collection: fetchUserAssets,
});

// 用户流水列表（UserAssetsPage 流水 tab）：defCode/ascending 过滤同样进
// scope 对象（ResourceScope 值域是 string——boolean 以 "1" 编码，适配器解码）。
async function fetchUserLedger(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<AssetLedgerEntry>> {
  if (ctx.projectId == null || ctx.owner == null) return { rows: [] };
  try {
    const page = await listUserLedger(ctx.owner, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      defCode: ctx.defCode || undefined,
      ascending: ctx.ascending === "1" || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const userLedgerResource = defineResource({
  name: "user-ledger",
  scope: ["projectId"],
  collection: fetchUserLedger,
});
