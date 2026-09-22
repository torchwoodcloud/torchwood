import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

export interface SubscriptionPlan {
  id: string;
  project_id?: string;
  code: string;
  name: string;
  amount: string;
  currency: string;
  interval: string;
  interval_days?: string;
  grace_days?: number;
  trial_days?: number;
  status?: string;
  created_at?: string;
  updated_at?: string;
}

export interface Subscription {
  id: string;
  project_id?: string;
  user_id?: string;
  plan_id?: string;
  plan_code?: string;
  mode: string;
  status: string;
  current_period_start?: string;
  current_period_end?: string;
  cancel_at_period_end?: boolean;
  grace_until?: string;
  created_at?: string;
}

// 服务端 plans/subscriptions 列表默认 page_size=25、空页才停发
// next_page_token；对接服务端分页（契约说明见 pagination.ts），pageSize 必传。
export async function listPlans(params: ListParams): Promise<Page<SubscriptionPlan>> {
  const res = await api.get<{ plans: SubscriptionPlan[] } & ListMeta>(
    "/server/subscriptions/plans",
    { params: pageQuery(params) }
  );
  return { rows: res.data.plans ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getPlan(planId: string): Promise<SubscriptionPlan> {
  const res = await api.get<SubscriptionPlan>(`/server/subscriptions/plans/${planId}`);
  return res.data;
}

export async function createPlan(input: {
  code: string;
  name: string;
  amount: string;
  currency: string;
  interval: string;
  interval_days?: string;
  grace_days?: number;
  trial_days?: number;
}): Promise<SubscriptionPlan> {
  const res = await api.post<SubscriptionPlan>("/server/subscriptions/plans", input);
  return res.data;
}

export async function updatePlan(
  planId: string,
  input: { name?: string; status?: string }
): Promise<SubscriptionPlan> {
  const res = await api.patch<SubscriptionPlan>(`/server/subscriptions/plans/${planId}`, {
    plan_id: planId,
    ...input,
  });
  return res.data;
}

export async function deletePlan(planId: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/subscriptions/plans/${planId}`, config);
}

// 结构化过滤（ListSubscriptionsRequest）：user_id/status 精确 + created_at
// 闭区间（时间一律 RFC3339；status = trialing|active|past_due|canceled|expired）。
export interface ListSubscriptionsFilter {
  userId?: string;
  status?: string;
  createdAfter?: string;
  createdBefore?: string;
}

export async function listSubscriptions(
  params: ListParams & ListSubscriptionsFilter
): Promise<Page<Subscription>> {
  const res = await api.get<{ subscriptions: Subscription[] } & ListMeta>(
    "/server/subscriptions",
    {
      params: {
        ...pageQuery(params),
        user_id: params.userId || undefined,
        status: params.status || undefined,
        created_after: params.createdAfter || undefined,
        created_before: params.createdBefore || undefined,
      },
    }
  );
  return { rows: res.data.subscriptions ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getSubscription(subscriptionId: string): Promise<Subscription> {
  const res = await api.get<Subscription>(`/server/subscriptions/${subscriptionId}`);
  return res.data;
}

export async function cancelSubscription(subscriptionId: string, reason?: string): Promise<Subscription> {
  const res = await api.post<Subscription>(`/server/subscriptions/${subscriptionId}:cancel`, {
    subscription_id: subscriptionId,
    reason,
  });
  return res.data;
}

export async function expireSubscription(subscriptionId: string, reason?: string): Promise<Subscription> {
  const res = await api.post<Subscription>(`/server/subscriptions/${subscriptionId}:expire`, {
    subscription_id: subscriptionId,
    reason,
  });
  return res.data;
}
