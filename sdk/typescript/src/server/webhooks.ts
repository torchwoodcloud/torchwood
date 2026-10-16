import type { HttpTransport } from "../http.js";
import type { ListMeta } from "../types.js";

// ——Webhooks 一等公民（functions-v4.md §6，D5）——
// 订阅 outbox 事件脊柱（文档写事件 + 系统行为事件），按 CloudEvents 1.0
// structured mode 投递到用户配置的 HTTPS endpoint。REST 形状以
// proto/server/v1/webhooks.proto 的 google.api.http 注解为准。

/** Webhook 订阅视图。密钥任何形态不回显：明文仅在轮换响应一次性返回。 */
export interface Webhook {
  id: string;
  project_id: string;
  url: string;
  /** 订阅串列表（原样回显，eventmatch 语法，与函数事件触发器同款）。 */
  events: string[];
  /** enabled | paused。 */
  status: string;
  /** 签名密钥指纹（"whsec_" + SHA-256 前 8 hex；比对用，非明文）。 */
  secret_fingerprint: string;
  created_at: string;
  updated_at: string;
}

/** RotateWebhookSecret 响应：明文只出现这一次，调用方立即持久化。 */
export interface WebhookSecret {
  webhook_id: string;
  /** 签名密钥明文（whsec_ 前缀 + 32 字节 base64url）。之后只出指纹。 */
  secret: string;
  secret_fingerprint: string;
}

/** 投递行投影（DLQ 浏览/重投管理面）。status ∈ pending | delivered | failed。 */
export interface WebhookDelivery {
  id: string;
  webhook_id: string;
  /** 事件幂等键（outbox event_id；at-least-once 消费的去重吸收面）。 */
  event_id: string;
  event_type: string;
  /** outbox 全局序（事件产生序；0 = 未知）。 */
  seq: string | number;
  /** 已执行投递尝试次数。 */
  attempts: number;
  /** 下一次尝试时刻（pending 态可读；终态语义 = 上次尝试时刻）。 */
  next_attempt_at?: string;
  status: string;
  /** 最近一次尝试的响应码（2xx = 送达；0 = 未达 HTTP 层——连不通/超时）。 */
  last_response_code: number;
  /** 最近一次失败的错误摘要（截断留档；成功后清空）。 */
  last_error?: string;
  created_at: string;
  updated_at: string;
}

export class WebhooksService {
  constructor(private readonly http: HttpTransport) {}

  // 创建订阅：url 强制 https；订阅串复用 eventmatch 语法（1..64 条 × ≤200B，
  // 格式校验在服务端）。响应为 Webhook 视图（仅指纹）——签名密钥明文不在
  // 创建响应出现，需要明文请创建后立即 rotateSecret（与 Go 侧 handler 一致）。
  async create(input: {
    /** 可选（服务端生成 "wh-<uuid>"）；^$|^[a-z0-9][a-z0-9_-]{0,62}$。 */
    id?: string;
    url: string;
    events: string[];
    /** 缺省 enabled。 */
    status?: "enabled" | "paused";
  }): Promise<Webhook> {
    return this.http.request<Webhook>("POST", "/v1/server/webhooks", {
      auth: "apiKey",
      body: input,
    });
  }

  async list(
    params?: { page_size?: number; page_token?: string }
  ): Promise<{ webhooks: Webhook[]; meta?: ListMeta }> {
    const query: Record<string, string | number | undefined> = {
      page_size: params?.page_size,
      page_token: params?.page_token,
    };
    for (const k of Object.keys(query)) {
      if (query[k] === undefined || query[k] === "") delete query[k];
    }
    const res = await this.http.request<{ webhooks: Webhook[]; meta?: ListMeta }>(
      "GET",
      "/v1/server/webhooks",
      { auth: "apiKey", query }
    );
    return { webhooks: res.webhooks ?? [], meta: res.meta };
  }

  async get(webhookId: string): Promise<Webhook> {
    return this.http.request<Webhook>(
      "GET",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}`,
      { auth: "apiKey" }
    );
  }

  // 部分更新：未传字段 = 不修改；events 传入 = 全量替换订阅串集；密钥变更
  // 走 rotateSecret。
  async update(
    webhookId: string,
    input: {
      url?: string;
      events?: string[];
      status?: "enabled" | "paused";
    }
  ): Promise<Webhook> {
    return this.http.request<Webhook>(
      "PATCH",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}`,
      {
        auth: "apiKey",
        body: { webhook_id: webhookId, ...input },
      }
    );
  }

  async delete(webhookId: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}`,
      { auth: "apiKey" }
    );
  }

  // 轮换签名密钥：新钥即刻生效（v1 签名），旧钥保留一个窗口期（v2 双签）。
  async rotateSecret(webhookId: string): Promise<WebhookSecret> {
    return this.http.request<WebhookSecret>(
      "POST",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}:rotate-secret`,
      { auth: "apiKey", body: {} }
    );
  }

  // 投递行列表（DLQ 浏览面）：按投递时间倒序分页；status 过滤可选
  //（pending | delivered | failed，空 = 全部）。
  async listDeliveries(
    webhookId: string,
    params?: {
      page_size?: number;
      page_token?: string;
      status?: string;
    }
  ): Promise<{ deliveries: WebhookDelivery[]; meta?: ListMeta }> {
    const query: Record<string, string | number | undefined> = {
      page_size: params?.page_size,
      page_token: params?.page_token,
      status: params?.status,
    };
    for (const k of Object.keys(query)) {
      if (query[k] === undefined || query[k] === "") delete query[k];
    }
    const res = await this.http.request<{ deliveries: WebhookDelivery[]; meta?: ListMeta }>(
      "GET",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}/deliveries`,
      { auth: "apiKey", query }
    );
    return { deliveries: res.deliveries ?? [], meta: res.meta };
  }

  async getDelivery(webhookId: string, deliveryId: string): Promise<WebhookDelivery> {
    return this.http.request<WebhookDelivery>(
      "GET",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}/deliveries/${encodeURIComponent(deliveryId)}`,
      { auth: "apiKey" }
    );
  }

  // 手动重投：终态行（delivered/failed）重置为立即到期（status=pending、
  // attempts=0），由 worker 到期扫描领取重投；语义 = 全新一轮投递。
  async redeliver(webhookId: string, deliveryId: string): Promise<WebhookDelivery> {
    return this.http.request<WebhookDelivery>(
      "POST",
      `/v1/server/webhooks/${encodeURIComponent(webhookId)}/deliveries/${encodeURIComponent(deliveryId)}:redeliver`,
      { auth: "apiKey", body: {} }
    );
  }
}
