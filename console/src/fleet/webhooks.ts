import {
  type CollectionQuery,
  type CommandResult,
  defineResource,
  type ResourceScope,
  type Segment,
} from "@lynx-go/ui-kit/kernel";
import { collectionQuerySchema } from "@lynx-go/ui-kit/schema";
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
import { gatewayErrorCode, toApiError } from "@/fleet/errors";

// 列表 fleet 化（§12.9-2）：Webhooks 主列表走 useCollection（URL 即真相：
// cursor/size 进 URL，分页栈 = 浏览器历史）。数据面复用 axios 管线，失败
// 在适配器收敛为 ApiError（fleet/errors.ts 收敛缝）。
//
// 命令族切片（§12.3）：建/改/删/轮换挂资源 commands（kit 集合键由
// useCommand 自动失效）。结果两分映射同 apiKeys 先例（400→rejected
// {code,message}、409→conflict、其余 throw ApiError）；命令调用一律
// __skipToast（toast 单源 = onSettled）。update 的字段 diff 归调用方
//（页面持原实体快照——url/events 仅变更时随请求发送，proto3 optional）。

// 网关错误体（结果分支用）。
interface GatewayErrorBody {
  error?: { code?: string; message?: string };
}

function gatewayMessage(err: unknown): string {
  if (
    typeof err === "object" &&
    err !== null &&
    "response" in err &&
    (err as { response?: { data?: GatewayErrorBody } }).response?.data?.error?.message
  ) {
    return String((err as { response: { data: GatewayErrorBody } }).response.data.error!.message);
  }
  return err instanceof Error ? err.message : String(err);
}

function isStatus(err: unknown, status: number): boolean {
  return (
    typeof err === "object" &&
    err !== null &&
    "response" in err &&
    (err as { response?: { status?: number } }).response?.status === status
  );
}

/** 结果两分映射：400→rejected、409→conflict、其余 throw（toApiError 收敛）。 */
async function runCommandMapped<Payload>(
  op: () => Promise<Payload>
): Promise<CommandResult<Payload>> {
  try {
    return { status: "ok", payload: await op() };
  } catch (err) {
    if (isStatus(err, 400)) {
      return { status: "rejected", code: gatewayErrorCode(err), message: gatewayMessage(err) };
    }
    if (isStatus(err, 409)) {
      return { status: "conflict", message: gatewayMessage(err) };
    }
    throw toApiError(err);
  }
}

const SKIP = { __skipToast: true } as const;

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

export const webhooksResource = defineResource({
  name: "webhooks",
  getId: (row: Webhook) => row.id,
  scope: ["projectId"],
  collection: fetchWebhooks,
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
  },
});

// 命令句柄提升（demo 同款 commands! 模式——Resource.commands 可选类型面）。
export const createWebhookCommand = webhooksResource.commands!.create;
export const updateWebhookCommand = webhooksResource.commands!.update;
export const removeWebhookCommand = webhooksResource.commands!.remove;
export const rotateWebhookSecretCommand = webhooksResource.commands!.rotateSecret;

// 投递重投（webhook 子资源动作，users 会话撤销同款先例）：独立 descriptor
// 不挂资源——不涉 webhooks 集合语义；存量 ["webhook-deliveries"] 键由页面
// onSettled 手动桥。
export const redeliverDeliveryDescriptor = {
  run: async (input: {
    webhookId: string;
    deliveryId: string;
  }): Promise<CommandResult<WebhookDelivery>> =>
    runCommandMapped(() => redeliverDelivery(input.webhookId, input.deliveryId, SKIP)),
};

// URL 搜索 schema（路由 validateSearch 与绑定共用同一实例）。默认页大小
// 20 = 旧页 useServerPaging 默认；订阅列表无真实服务端过滤参数——不设
// filters 白名单（页内搜索是本地态，不进 URL 谓词）。
export const webhooksSearch = collectionQuerySchema({ defaultSize: 20 });
