import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface PaymentOrder {
  id: string;
  project_id?: string;
  user_id?: string;
  provider: string;
  amount: string;
  currency: string;
  purpose_kind: string;
  purpose?: Record<string, unknown>;
  status: string;
  idempotency_key?: string;
  provider_session_id?: string;
  provider_order_id?: string;
  created_at?: string;
  paid_at?: string;
  expires_at?: string;
}

// 服务端订单列表默认 page_size=25、空页才停发 next_page_token；
// 对接服务端分页（契约说明见 pagination.ts），pageSize 必传。
// 结构化过滤（ListOrdersRequest）：user_id/status 精确 + created_at 闭区间
//（时间一律 RFC3339；status = created|paying|paid|failed|closed|refunding|refunded）。
export interface ListOrdersFilter {
  userId?: string;
  status?: string;
  createdAfter?: string;
  createdBefore?: string;
}

export async function listOrders(
  params: ListParams & ListOrdersFilter
): Promise<Page<PaymentOrder>> {
  const res = await api.get<{ orders: PaymentOrder[] } & ListMeta>("/server/payments/orders", {
    params: {
      ...pageQuery(params),
      user_id: params.userId || undefined,
      status: params.status || undefined,
      created_after: params.createdAfter || undefined,
      created_before: params.createdBefore || undefined,
    },
  });
  return { rows: res.data.orders ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getOrder(orderId: string): Promise<PaymentOrder> {
  const res = await api.get<PaymentOrder>(`/server/payments/orders/${orderId}`);
  return res.data;
}

export async function refundOrder(
  orderId: string,
  input?: { amount?: string; reason?: string },
  config?: ApiRequestConfig
): Promise<PaymentOrder> {
  const res = await api.post<PaymentOrder>(
    `/server/payments/orders/${orderId}:refund`,
    { order_id: orderId, ...input },
    config
  );
  return res.data;
}

export async function manualFulfillOrder(
  orderId: string,
  reason?: string,
  config?: ApiRequestConfig
): Promise<{ order: PaymentOrder }> {
  const res = await api.post<{ order: PaymentOrder }>(
    `/server/payments/orders/${orderId}:fulfill`,
    { order_id: orderId, reason },
    config
  );
  return res.data;
}
