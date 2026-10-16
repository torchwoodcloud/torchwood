import { api } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

// ——Webhooks 一等公民（functions-v4.md §6，D5）——
// 订阅 outbox 事件脊柱（文档写事件 + 系统行为事件），按 CloudEvents 1.0
// structured mode 投递到用户配置的 HTTPS endpoint。REST 形状以
// proto/server/v1/webhooks.proto 的 google.api.http 注解为准。

// Webhook 是订阅视图。密钥任何形态不回显：明文仅在轮换响应一次性返回
//（WebhookSecret），本视图只出指纹。
export interface Webhook {
  id: string;
  project_id: string;
  url: string;
  // 订阅串列表（原样回显，eventmatch 语法）。
  events: string[];
  // enabled | paused。
  status: string;
  // 签名密钥指纹（"whsec_" + SHA-256 前 8 hex；比对用，非明文）。
  secret_fingerprint: string;
  created_at: string;
  updated_at: string;
}

// WebhookSecret 是轮换响应：明文只出现这一次，调用方立即持久化。
export interface WebhookSecret {
  webhook_id: string;
  secret: string;
  secret_fingerprint: string;
}

// WebhookDelivery 是投递行投影（DLQ 浏览/重投管理面）。status ∈
// pending | delivered | failed（failed 即 DLQ）。
export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  // 事件幂等键（outbox event_id；at-least-once 消费的去重吸收面）。
  event_id: string;
  event_type: string;
  // outbox 全局序（事件产生序；0 = 未知）。
  seq: number;
  attempts: number;
  // 下一次尝试时刻（pending 态可读；终态语义 = 上次尝试时刻）。
  next_attempt_at?: string;
  status: string;
  // 最近一次尝试的响应码（2xx = 送达；0 = 未达 HTTP 层——连不通/超时）。
  last_response_code: number;
  // 最近一次失败的错误摘要（截断留档；成功后清空）。
  last_error: string;
  created_at: string;
  updated_at: string;
}

// 服务端 offset 型分页（clamp 默认 50 / max 100）；对接契约见 pagination.ts。
export async function listWebhooks(params: ListParams): Promise<Page<Webhook>> {
  const res = await api.get<{ webhooks: Webhook[] } & ListMeta>("/server/webhooks", {
    params: pageQuery(params),
  });
  return { rows: res.data.webhooks ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getWebhook(webhookId: string): Promise<Webhook> {
  const res = await api.get<Webhook>(`/server/webhooks/${webhookId}`);
  return res.data;
}

// CreateWebhook 响应为 Webhook 视图（仅指纹）：明文密钥不在创建响应出现，
// CreateWebhookResponse：webhook 视图 + 一次性明文密钥（与 functions
// create 的 target_secret 同语义；之后任何接口只出指纹，变更走轮换）。
export interface CreateWebhookResponse {
  webhook: Webhook;
  secret: string;
}

// 创建订阅：响应携带一次性明文密钥，调用方立即展示/持久化。
export async function createWebhook(input: {
  id?: string;
  url: string;
  events: string[];
  // 缺省 enabled。
  status?: "enabled" | "paused";
}): Promise<CreateWebhookResponse> {
  const res = await api.post<CreateWebhookResponse>("/server/webhooks", input);
  return res.data;
}

// 部分更新：未传字段 = 不修改；events 传入 = 全量替换订阅串集。
export async function updateWebhook(
  webhookId: string,
  input: {
    url?: string;
    events?: string[];
    status?: "enabled" | "paused";
  }
): Promise<Webhook> {
  const res = await api.patch<Webhook>(`/server/webhooks/${webhookId}`, input);
  return res.data;
}

export async function deleteWebhook(webhookId: string): Promise<void> {
  await api.delete(`/server/webhooks/${webhookId}`);
}

// 轮换签名密钥：新钥即刻生效（v1 签名），旧钥保留一个窗口期（v2 双签）。
export async function rotateWebhookSecret(webhookId: string): Promise<WebhookSecret> {
  const res = await api.post<WebhookSecret>(
    `/server/webhooks/${webhookId}:rotate-secret`,
    {}
  );
  return res.data;
}

// 投递行列表（DLQ 浏览面）：按投递时间倒序分页；status 过滤可选。
export interface ListDeliveriesFilter {
  status?: string;
}

export async function listDeliveries(
  webhookId: string,
  params: ListParams & ListDeliveriesFilter
): Promise<Page<WebhookDelivery>> {
  const res = await api.get<{ deliveries: WebhookDelivery[] } & ListMeta>(
    `/server/webhooks/${webhookId}/deliveries`,
    {
      params: {
        ...pageQuery(params),
        status: params.status || undefined,
      },
    }
  );
  return { rows: res.data.deliveries ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getDelivery(
  webhookId: string,
  deliveryId: string
): Promise<WebhookDelivery> {
  const res = await api.get<WebhookDelivery>(
    `/server/webhooks/${webhookId}/deliveries/${deliveryId}`
  );
  return res.data;
}

// 手动重投：终态行（delivered/failed）重置为立即到期（status=pending、
// attempts=0），语义 = 全新一轮投递。
export async function redeliverDelivery(
  webhookId: string,
  deliveryId: string
): Promise<WebhookDelivery> {
  const res = await api.post<WebhookDelivery>(
    `/server/webhooks/${webhookId}/deliveries/${deliveryId}:redeliver`,
    {}
  );
  return res.data;
}
