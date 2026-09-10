import { api } from "./client";
import type { ApiRequestConfig } from "./client";

export interface FunctionItem {
  id: string;
  project_id: string;
  name: string;
  runtime: string;
  entrypoint: string;
  timeout_seconds: number;
  spec: string;
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
  // ——池策略（v3 多路复用，functions-v3.md §5/OQ2；只读视图，
  // 管理经 updateFunction 的可选字段）——
  min_instances: number;
  max_instances: number;
  idle_ttl_seconds: number;
  max_requests_per_instance: number;
  concurrency: number;
  created_at: string;
  updated_at: string;
}

export interface RuntimeInfo {
  id: string;
  name: string;
  entrypoint: string;
}

export interface SpecificationInfo {
  id: string;
  cpu: string;
  memory: string;
}

export interface Deployment {
  id: string;
  function_id: string;
  size: number;
  status: string;
  error: string;
  created_at: string;
  updated_at: string;
}

export interface Execution {
  id: string;
  function_id: string;
  deployment_id: string;
  status: string;
  response: string;
  stdout: string;
  stderr: string;
  status_code: number;
  duration_ms: number;
  error: string;
  response_truncated: boolean;
  stdout_truncated: boolean;
  stderr_truncated: boolean;
  created_at: string;
  updated_at: string;
}

export interface Variable {
  key: string;
  value: string;
}

// SECRET_MASK 与后端 internal/app/functions/variables.go 的 secretMask 一致：
// GetVariables/SetVariables 响应中非空值以该串脱敏；SetVariables 请求中值等于
// 该串的 key 表示「保留旧值」，后端不覆盖。
export const SECRET_MASK = "******";

export async function listRuntimes(): Promise<RuntimeInfo[]> {
  const res = await api.get<{ runtimes: RuntimeInfo[] }>("/server/functions:runtimes");
  return res.data.runtimes ?? [];
}

export async function listSpecifications(): Promise<SpecificationInfo[]> {
  const res = await api.get<{ specifications: SpecificationInfo[] }>(
    "/server/functions:specifications"
  );
  return res.data.specifications ?? [];
}

export async function listFunctions(): Promise<FunctionItem[]> {
  const res = await api.get<{ functions: FunctionItem[] }>("/server/functions");
  return res.data.functions ?? [];
}

export async function getFunction(id: string): Promise<FunctionItem> {
  const res = await api.get<FunctionItem>(`/server/functions/${id}`);
  return res.data;
}

export async function createFunction(input: {
  id: string;
  name: string;
  runtime: string;
  entrypoint?: string;
  timeout_seconds?: number;
  spec?: string;
  enabled?: boolean;
}): Promise<FunctionItem> {
  const res = await api.post<FunctionItem>("/server/functions", input);
  return res.data;
}

export async function updateFunction(
  id: string,
  input: {
    name?: string;
    entrypoint?: string;
    timeout_seconds?: number;
    spec?: string;
    enabled?: boolean;
    // 客户端调用面策略（P2）。
    client_callable?: boolean;
    client_anonymous_allowed?: boolean;
    client_per_user_limit?: number;
    client_limit_window?: "minute" | "hour" | "day";
    // 池策略（v3 §5/OQ2；proto3 optional——未传 = 不修改）。
    min_instances?: number;
    max_instances?: number;
    idle_ttl_seconds?: number;
    max_requests_per_instance?: number;
    concurrency?: number;
  }
): Promise<FunctionItem> {
  const res = await api.patch<FunctionItem>(`/server/functions/${id}`, input);
  return res.data;
}

export async function deleteFunction(id: string, config?: ApiRequestConfig): Promise<void> {
  await api.delete(`/server/functions/${id}`, config);
}

export async function listDeployments(functionId: string): Promise<Deployment[]> {
  const res = await api.get<{ deployments: Deployment[] }>(
    `/server/functions/${functionId}/deployments`
  );
  return res.data.deployments ?? [];
}

export async function uploadDeployment(
  functionId: string,
  file: File
): Promise<Deployment> {
  const form = new FormData();
  form.append("code", file);
  const res = await api.post<Deployment>(
    `/server/functions/${functionId}/deployments/code`,
    form,
    { headers: { "Content-Type": "multipart/form-data" } }
  );
  return res.data;
}

export async function deleteDeployment(
  functionId: string,
  deploymentId: string
): Promise<void> {
  await api.delete(
    `/server/functions/${functionId}/deployments/${deploymentId}`
  );
}

export async function getVariables(functionId: string): Promise<Variable[]> {
  const res = await api.get<{ variables: Variable[] }>(
    `/server/functions/${functionId}/variables`
  );
  return res.data.variables ?? [];
}

export async function setVariables(
  functionId: string,
  variables: Variable[]
): Promise<Variable[]> {
  const res = await api.put<{ variables: Variable[] }>(
    `/server/functions/${functionId}/variables`,
    { variables }
  );
  return res.data.variables ?? [];
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

export async function createExecution(
  functionId: string,
  input: { deployment_id?: string; data?: string; async?: boolean }
): Promise<Execution> {
  const res = await api.post<Execution>(
    `/server/functions/${functionId}/executions`,
    input
  );
  return res.data;
}

export async function listExecutions(functionId: string): Promise<Execution[]> {
  const res = await api.get<{ executions: Execution[] }>(
    `/server/functions/${functionId}/executions`
  );
  return res.data.executions ?? [];
}

export async function getExecution(
  functionId: string,
  executionId: string
): Promise<Execution> {
  const res = await api.get<Execution>(
    `/server/functions/${functionId}/executions/${executionId}`
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
