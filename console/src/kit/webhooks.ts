import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/uikit-core/kernel";
import { collectionQuerySchema } from "@lynx-go/uikit-core/schema";
import { skipToast as SKIP } from "@/api/client";
import {
  listWebhooks,
  getWebhook,
  listDeliveries,
  createWebhook,
  updateWebhook,
  deleteWebhook,
  rotateWebhookSecret,
  redeliverDelivery,
  type Webhook,
  type WebhookDelivery,
  type CreateWebhookResponse,
  type WebhookSecret,
} from "@/api/webhooks";
import { recordOrNull, runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：Webhooks 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：建/改/删/轮换挂资源 commands（kit 集合键 + 详情
// record 键由 useCommand 缺省同资源失效自动命中）；投递重投是子资源动作
// ——invalidates 显式声明只打 webhook-deliveries 资源（声明优先 = 整体
// 替换缺省，不失效 webhooks 集合语义保持）。结果两分映射同 apiKeys 先例
//（400→rejected{code,message}、409→conflict、其余 throw ApiError）；命令
// 调用一律 __skipToast（toast 单源 = onSettled）。update 的字段 diff 归
// 调用方（页面持原实体快照——url/events 仅变更时随请求发送，proto3
// optional）。
//
// 子视图 kit 化批：详情读 record 槽 + webhook-deliveries 子视图资源化——
// 存量 ["webhooks"] / ["webhook-deliveries"] 前缀桥随读者清零全量退役。

async function fetchWebhooks(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<Webhook>> {
  if (ctx.projectId == null) return { rows: [] };
  try {
    const page = await listWebhooks({
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

// ——deliveries（Webhook 详情内嵌投递记录列表（DLQ 浏览/重投管理面）；嵌套
// scope 加 webhookId，ctx 缺参静默空段）。status 过滤走 scope 旁路键（不进
// 声明键——全量对象进 queryKey，声明键只定就绪门与失效投影轴）。——
async function fetchDeliveries(
  q: CollectionQuery,
  ctx: ResourceScope
): Promise<Segment<WebhookDelivery>> {
  if (ctx.projectId == null || ctx.webhookId == null) return { rows: [] };
  try {
    const page = await listDeliveries(ctx.webhookId, {
      pageSize: q.window.size,
      pageToken: q.window.cursor || undefined,
      status: ctx.status || undefined,
    });
    return { rows: page.rows, nextCursor: page.nextPageToken };
  } catch (err) {
    throw toApiError(err);
  }
}

export const webhookDeliveriesResource = defineResource({
  name: "webhook-deliveries",
  getId: (row: WebhookDelivery) => row.id,
  scope: ["projectId", "webhookId"],
  collection: fetchDeliveries,
});

// 投递重投（webhook 子资源动作）：重置投递行为新一轮投递——不改变
// webhook 实体。invalidates 显式声明只打 webhook-deliveries 子视图资源；
// 声明优先 = 整体替换缺省，webhooks 集合/详情 record 不失效语义保持。
// allScopes 显式逃逸：命令挂 webhooks 资源，生效 scope 只有 projectId
//（context 采纳），webhookId 投影必 miss——宽度与旧 ["webhook-deliveries"]
// 前缀桥一致（databases create→collections 同款裁决）。
export const redeliverDeliveryDescriptor = {
  run: async (input: {
    webhookId: string;
    deliveryId: string;
  }): Promise<CommandResult<WebhookDelivery>> =>
    runCommandMapped(() => redeliverDelivery(input.webhookId, input.deliveryId, SKIP)),
  invalidates: [{ resource: "webhook-deliveries", allScopes: true }],
};

export const webhooksResource = defineResource({
  name: "webhooks",
  getId: (row: Webhook) => row.id,
  scope: ["projectId"],
  collection: fetchWebhooks,
  // 详情读（WebhookDetailPage，子视图 kit 化批）：404 → null 空态直出
  //（recordOrNull 手写方言，不引 restRecord）；update/rotate/remove 命令的
  // 缺省同资源失效（全 kind）现命中此键——保存后详情自刷新。
  record: (id) => recordOrNull(() => getWebhook(id)) as Promise<Webhook>,
  // 存量键失效桥已退役（子视图 kit 化批）：["webhooks"] /
  // ["webhook-deliveries"] 前缀读者清零——详情读走 kit record 键（缺省同
  // 资源失效命中），投递列表走 webhook-deliveries 资源键（redeliver 命令
  // 显式 invalidates 打其上）。
  commands: {
    // 创建 payload 携带一次性明文密钥（页面 reveal，关闭即弃）。
    create: {
      run: async (input: {
        url: string;
        events: string[];
        status?: "enabled" | "paused";
      }): Promise<CommandResult<CreateWebhookResponse>> =>
        runCommandMapped(() => createWebhook(input, SKIP)),
    },
    update: {
      run: async (input: {
        id: string;
        url?: string;
        events?: string[];
        status?: "enabled" | "paused";
      }): Promise<CommandResult<Webhook>> =>
        runCommandMapped(() => {
          const { id, ...rest } = input;
          return updateWebhook(id, rest, SKIP);
        }),
    },
    remove: {
      run: async (input: { id: string }): Promise<CommandResult<null>> =>
        runCommandMapped(async () => {
          await deleteWebhook(input.id, SKIP);
          return null;
        }),
      // delete 已删 → 幂等 ok 同族语义（apiKeys remove 先例）
      treatAs: { "not-found": { status: "ok" as const, payload: null } },
    },
    rotateSecret: {
      run: async (input: { id: string }): Promise<CommandResult<WebhookSecret>> =>
        runCommandMapped(() => rotateWebhookSecret(input.id, SKIP)),
    },
    // 子资源动作（非实体变更）：invalidates 在 descriptor 内声明（见上——
    // 只打 webhook-deliveries 资源键）。
    redeliver: redeliverDeliveryDescriptor,
  },
});

// 命令句柄提升（§12.10-⑥：commands 在 spec 出现 ⇒ 类型非可选，免 !）。
export const createWebhookCommand = webhooksResource.commands.create;
export const updateWebhookCommand = webhooksResource.commands.update;
export const removeWebhookCommand = webhooksResource.commands.remove;
export const rotateWebhookSecretCommand = webhooksResource.commands.rotateSecret;
export const redeliverDeliveryCommand = webhooksResource.commands.redeliver;

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；订阅列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const webhooksSearch = collectionQuerySchema({ defaultSize: 20 });
