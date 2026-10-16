import { api } from "./client";
import type { ApiRequestConfig } from "./client";
import { pageQuery, type ListMeta, type ListParams, type Page } from "./pagination";

// ——Functions v4（docs/design/functions-v4.md）——
// 函数 = 常驻服务（endpoint target），torchwood 只做路由、身份、记账与投递；
// 执行面（打包/构建/部署/池/运行时/变量）已整体外包。一次调用统一为
// invocation（v4 §2 重命名：CreateExecution → InvokeFunction / executions →
// invocations）。

// FunctionTarget 投影（v4 §3 单一 endpoint kind）：密钥任何形态不回显——
// 明文仅在创建/轮换响应一次性返回，管理面只出指纹。
export interface FunctionTarget {
  url: string;
  // 签名密钥指纹（"twfnsec_" + SHA-256 前 8 hex；比对用，非明文）。
  secret_fingerprint: string;
  // 最近一次密钥轮换时刻（宽限窗内旧钥 v2 双签）。
  secret_rotated_at?: string;
}

export interface FunctionItem {
  id: string;
  project_id: string;
  name: string;
  timeout_seconds: number;
  enabled: boolean;
  // 执行身份声明（P0）：每项形如 "<resource>:<op>"（如 "assets:write"），
  // 词表校验在服务端；空数组 = 无平台访问权限（fail-closed）。
  declared_scopes: string[];
  // ——客户端调用面策略（P2）——
  client_callable: boolean;
  // 字段保留、一期禁用（服务端遇 true 显式报错）。
  client_anonymous_allowed: boolean;
  client_per_user_limit: number;
  client_limit_window: "minute" | "hour" | "day";
  // endpoint target（v4）：执行位置 = 常驻服务调用终点。
  target: FunctionTarget;
  created_at: string;
  updated_at: string;
}

// Invocation 是一次函数调用记录（torchwood 只发起调用、不执行；日志归
// substrate 侧，这里只见网关往返结果）。status ∈ queued|running|completed|failed
//（v4 收敛：building 态随补构建退役）。
export interface Invocation {
  id: string;
  function_id: string;
  status: string;
  response: string;
  status_code: number;
  duration_ms: number;
  error: string;
  response_truncated: boolean;
  created_at: string;
  updated_at: string;
}

// 服务端函数列表默认 page_size=50（in-memory crud，created_at DESC）；对接
// 服务端分页（契约说明见 pagination.ts），pageSize 必传。
export async function listFunctions(params: ListParams): Promise<Page<FunctionItem>> {
  const res = await api.get<{ functions: FunctionItem[] } & ListMeta>("/server/functions", {
    params: pageQuery(params),
  });
  return { rows: res.data.functions ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getFunction(id: string): Promise<FunctionItem> {
  const res = await api.get<FunctionItem>(`/server/functions/${id}`);
  return res.data;
}

// CreateFunctionResponse = 函数视图 + 一次性 target 签名密钥明文（仅此一次；
// 之后任何面只出指纹）。
export interface CreateFunctionResult {
  function: FunctionItem;
  target_secret: string;
}

export async function createFunction(input: {
  id: string;
  name: string;
  timeout_seconds?: number;
  enabled?: boolean;
  declared_scopes?: string[];
  client_callable?: boolean;
  client_per_user_limit?: number;
  client_limit_window?: "minute" | "hour" | "day";
  // endpoint target url（v4）：函数常驻服务的调用终点（生产强制 https）。
  target_url: string;
  // 可选自定义签名密钥（twfnsec_ 前缀；空 = 服务端生成）。
  target_secret?: string;
}): Promise<CreateFunctionResult> {
  const res = await api.post<CreateFunctionResult>("/server/functions", input);
  return res.data;
}

export async function updateFunction(
  id: string,
  input: {
    name?: string;
    timeout_seconds?: number;
    enabled?: boolean;
    client_callable?: boolean;
    client_anonymous_allowed?: boolean;
    client_per_user_limit?: number;
    client_limit_window?: "minute" | "hour" | "day";
    // target url（v4）：未设置 = 不修改；密钥变更走 rotateFunctionSecret。
    target_url?: string;
  }
): Promise<FunctionItem> {
  const res = await api.patch<FunctionItem>(`/server/functions/${id}`, input);
  return res.data;
}

export async function deleteFunction(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/functions/${id}`, config);
}

// FunctionSecret 是轮换响应：明文只出现这一次，调用方立即部署到函数侧。
export interface FunctionSecret {
  function_id: string;
  secret: string;
  secret_fingerprint: string;
}

// 轮换 endpoint target 签名密钥：新钥即刻生效（v1 签名），旧钥保留一个宽限
// 窗（v2 双签）——函数侧持旧钥的 FDK 过渡期不断流。
export async function rotateFunctionSecret(id: string): Promise<FunctionSecret> {
  const res = await api.post<FunctionSecret>(`/server/functions/${id}:rotate-secret`, {});
  return res.data;
}

// 全量替换函数 declared_scopes（执行身份，P0）。空数组 = 撤销全部平台访问。
export async function setFunctionScopes(
  functionId: string,
  declaredScopes: string[]
): Promise<FunctionItem> {
  const res = await api.put<FunctionItem>(
    `/server/functions/${functionId}/scopes`,
    { declared_scopes: declaredScopes }
  );
  return res.data;
}

// 调用一个函数（v4：CreateExecution 改名 InvokeFunction；目标恒为函数当前
// 的 endpoint target）。async=true 时立即返回 queued invocation。
export async function invokeFunction(
  functionId: string,
  input: { data?: string; async?: boolean }
): Promise<Invocation> {
  const res = await api.post<Invocation>(
    `/server/functions/${functionId}:invoke`,
    input
  );
  return res.data;
}

// 调用记录列表（ListInvocationsRequest）：分页 + status 精确 + created_at 闭
// 区间过滤（排障面）；status ∈ queued|running|completed|failed，时间一律
// RFC3339。
export interface ListInvocationsFilter {
  status?: string;
  createdAfter?: string;
  createdBefore?: string;
}

export async function listInvocations(
  functionId: string,
  params: ListParams & ListInvocationsFilter
): Promise<Page<Invocation>> {
  const res = await api.get<{ invocations: Invocation[] } & ListMeta>(
    `/server/functions/${functionId}/invocations`,
    {
      params: {
        ...pageQuery(params),
        status: params.status || undefined,
        created_after: params.createdAfter || undefined,
        created_before: params.createdBefore || undefined,
      },
    }
  );
  return { rows: res.data.invocations ?? [], nextPageToken: res.data.meta?.next_page_token };
}

export async function getInvocation(
  functionId: string,
  invocationId: string
): Promise<Invocation> {
  const res = await api.get<Invocation>(
    `/server/functions/${functionId}/invocations/${invocationId}`
  );
  return res.data;
}

// ——触发器管理（P1 触发器模块）——

export interface FunctionTrigger {
  id: string;
  function_id: string;
  type: "http" | "cron" | "event";
  enabled: boolean;
  // http 专有
  response_mode?: string;
  ack_body?: string;
  handshake?: string;
  body_limit_bytes?: number;
  // token 明文仅在管理面返回。
  token?: string;
  invoke_path?: string;
  // cron 专有
  expr?: string;
  misfire?: string;
  next_run_at?: string;
  // event 专有（v3 切片 D）
  events?: string[];
  created_at: string;
  updated_at: string;
}

export interface CreateTriggerInput {
  type: "http" | "cron" | "event";
  http?: {
    response_mode: string;
    ack_body?: string;
    handshake?: string;
    body_limit_bytes?: number;
  };
  cron?: {
    expr: string;
    misfire?: string;
  };
  event?: {
    events: string[];
  };
}

// 单函数的触发器配置子资源，量级由人手配置决定（无服务端分页契约）。
export async function listFunctionTriggers(
  functionId: string
): Promise<FunctionTrigger[]> {
  const res = await api.get<{ triggers: FunctionTrigger[] }>(
    `/server/functions/${functionId}/triggers`
  );
  return res.data.triggers ?? [];
}

export async function createFunctionTrigger(
  functionId: string,
  input: CreateTriggerInput
): Promise<FunctionTrigger> {
  const res = await api.post<FunctionTrigger>(
    `/server/functions/${functionId}/triggers`,
    { function_id: functionId, ...input }
  );
  return res.data;
}

export async function deleteFunctionTrigger(
  functionId: string,
  triggerId: string
): Promise<void> {
  await api.delete(
    `/server/functions/${functionId}/triggers/${triggerId}`
  );
}

export async function rotateFunctionTriggerToken(
  functionId: string,
  triggerId: string
): Promise<FunctionTrigger> {
  const res = await api.post<FunctionTrigger>(
    `/server/functions/${functionId}/triggers/${triggerId}:rotate-token`,
    {}
  );
  return res.data;
}
