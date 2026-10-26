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
import { runCommandMapped, toApiError } from "@/kit/errors";

// 列表 fleet 化（§12.9-2）：Webhooks 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（kit/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：建/改/删/轮换挂资源 commands（kit 集合键由
// useCommand 自动失效）；投递重投是子资源动作——挂资源但显式 invalidates: []
// 不失效 webhooks 集合，存量 ["webhook-deliveries"] 键经资源 legacyKeys 桥
//（§12.10-② expireOld 样板：挂载句柄注入生效）。结果两分映射同 apiKeys
// 先例（400→rejected{code,message}、409→conflict、其余 throw ApiError）；
// 命令调用一律 __skipToast（toast 单源 = onSettled）。update 的字段 diff
// 归调用方（页面持原实体快照——url/events 仅变更时随请求发送，proto3
// optional）。

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

// 投递重投（webhook 子资源动作）：重置投递行为新一轮投递——不改变
// webhook 实体，invalidates: [] 显式不失效 webhooks 集合（kit 键）；存量
// ["webhook-deliveries"] 键经资源 legacyKeys 桥。挂资源（expireOld 样板）
// 仅为让 legacyKeys 注入生效——页面用挂载句柄。
export const redeliverDeliveryDescriptor = {
  run: async (input: {
    webhookId: string;
    deliveryId: string;
  }): Promise<CommandResult<WebhookDelivery>> =>
    runCommandMapped(() => redeliverDelivery(input.webhookId, input.deliveryId, SKIP)),
  invalidates: [],
};

export const webhooksResource = defineResource({
  name: "webhooks",
  getId: (row: Webhook) => row.id,
  scope: ["projectId"],
  collection: fetchWebhooks,
  // 存量键失效桥（§12.10-②）：命令 ok 后除 kit 键外失效存量 ["webhooks"]
  // /["webhook-deliveries"] 前缀键——页面 onSettled 手动桥退役。
  legacyKeys: () => [["webhooks"], ["webhook-deliveries"]],
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
    // 子资源动作（非实体变更）：invalidates: [] 在 descriptor 内声明（见上）。
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
