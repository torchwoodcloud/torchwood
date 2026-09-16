import { api } from "./client";

// RuntimeVars Console 数据层（docs/design/runtime-vars.md §2.7）。接口形状对齐
// proto/server/v1/runtime_vars.proto（集合/变量/版本三组 13 方法）；Console 不
// import @torchwood/sdk，axios 直连 /server/runtime-var-sets 是现状约定
//（X-Torchwood-Project header 已由 api 实例注入）。int64 归一在本层完成：
// grpc-gateway 以 protojson 输出 int64 字符串（如 "3"），前端统一转 number
//（integer 值域 ±2^53−1 服务端已拦，转换无损）。

/** 集合可见性（proto enum 名；网关 protojson 输入输出均接受名称或数值）。 */
export type VarSetVisibilityInput =
  | "VAR_SET_VISIBILITY_PUBLIC"
  | "VAR_SET_VISIBILITY_PRIVATE";

/** 版本链动作（proto enum 名；展示层做中文映射）。 */
export type RuntimeVarVersionActionName =
  | "RUNTIME_VAR_VERSION_ACTION_CREATE"
  | "RUNTIME_VAR_VERSION_ACTION_UPDATE"
  | "RUNTIME_VAR_VERSION_ACTION_DELETE"
  | "RUNTIME_VAR_VERSION_ACTION_ROLLBACK";

/** 五类型 oneof 的输入形态（恰好携带一个分支；integer 值域 ±2^53−1）。 */
export type RuntimeVarValueInput =
  | { string_value: string }
  | { integer_value: number }
  | { float_value: number }
  | { bool_value: boolean }
  | { json_value: string };

/** 五类型 oneof 的响应形态（服务端保证恰有一个分支被设置）。 */
export interface RuntimeVarValue {
  string_value?: string;
  /** int64：网关 protojson 输出字符串，本层已转 number。 */
  integer_value?: number;
  float_value?: number;
  bool_value?: boolean;
  /** 合法 JSON 文本（写路径已校验嵌套 ≤100），展示时按需自行 JSON.parse。 */
  json_value?: string;
}

/** 变量集合投影（footprint：var_count/total_bytes 供 D13 限额展示）。 */
export interface VarSet {
  /** = var_set_id（列表骨架 DataTable 以 id 为主键，展示仍用 var_set_id）。 */
  id: string;
  var_set_id: string;
  /** enum 名：VAR_SET_VISIBILITY_PUBLIC / VAR_SET_VISIBILITY_PRIVATE。 */
  visibility: string;
  description: string;
  /** 不透明 token "{epoch}:{revision}"（D2）：透传勿解析。 */
  etag: string;
  /** int64：网关输出字符串，本层已转 number。 */
  revision: number;
  var_count: number;
  /** int64：网关输出字符串，本层已转 number。 */
  total_bytes: number;
  created_at?: string;
  updated_at?: string;
}

/** 单个类型化变量（key 创建后不可改；类型变更走显式 Update，D3/D5）。 */
export interface RuntimeVar {
  var_set_id: string;
  key: string;
  value?: RuntimeVarValue;
  description: string;
  created_at?: string;
  updated_at?: string;
}

/** 版本链元数据（D10：仅"值快照序列"，全文走 getVersion）。 */
export interface RuntimeVarVersion {
  /** int64：网关输出字符串，本层已转 number。 */
  revision: number;
  /** enum 名：RUNTIME_VAR_VERSION_ACTION_CREATE / _UPDATE / _DELETE / _ROLLBACK。 */
  action: string;
  summary: string;
  /** admin:<id> / apikey:<id> / function:<id>。 */
  actor: string;
  created_at?: string;
}

/** 版本快照全文的单键条目（{key:{t,v,d,c}} 的展开，D11 全列四元组）。 */
export interface RuntimeVarVersionEntry {
  key: string;
  value?: RuntimeVarValue;
  description: string;
  created_at?: string;
}

export interface GetRuntimeVarVersionResponse {
  version?: RuntimeVarVersion;
  /** 该版本的全量快照（key ASC）。 */
  entries: RuntimeVarVersionEntry[];
}

// ---- int64 归一（grpc-gateway protojson 输出字符串，如 "3"）----

function toNumber(v: number | string | undefined): number | undefined {
  if (v === undefined) return undefined;
  return typeof v === "string" ? Number(v) : v;
}

function normalizeValue(v: RuntimeVarValue | undefined): RuntimeVarValue | undefined {
  if (!v) return v;
  return { ...v, integer_value: toNumber(v.integer_value) };
}

function normalizeVarSet(v: VarSet): VarSet {
  return {
    ...v,
    id: v.var_set_id,
    revision: toNumber(v.revision) ?? 0,
    total_bytes: toNumber(v.total_bytes) ?? 0,
  };
}

function normalizeVar(v: RuntimeVar): RuntimeVar {
  return { ...v, value: normalizeValue(v.value) };
}

function normalizeVersion(v: RuntimeVarVersion): RuntimeVarVersion {
  return { ...v, revision: toNumber(v.revision) ?? 0 };
}

function normalizeEntries(
  entries: RuntimeVarVersionEntry[] | undefined
): RuntimeVarVersionEntry[] {
  return (entries ?? []).map((e) => ({ ...e, value: normalizeValue(e.value) }));
}

// ---- 集合 ----

export async function listVarSets(): Promise<VarSet[]> {
  const res = await api.get<{ var_sets?: VarSet[] }>("/server/runtime-var-sets");
  return (res.data.var_sets ?? []).map(normalizeVarSet);
}

export async function getVarSet(varSetId: string): Promise<VarSet> {
  const res = await api.get<VarSet>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}`
  );
  return normalizeVarSet(res.data);
}

/** 创建集合（var_set_id 创建后不可改，D9；visibility 缺省 public）。 */
export async function createVarSet(input: {
  var_set_id: string;
  visibility?: VarSetVisibilityInput;
  description?: string;
}): Promise<VarSet> {
  const res = await api.post<VarSet>("/server/runtime-var-sets", {
    var_set_id: input.var_set_id,
    visibility: input.visibility ?? "VAR_SET_VISIBILITY_PUBLIC",
    description: input.description ?? "",
  });
  return normalizeVarSet(res.data);
}

// proto3 optional 对齐：undefined 不被 JSON 序列化 = 不修改（PATCH 语义）。
export interface UpdateVarSetInput {
  visibility?: VarSetVisibilityInput;
  description?: string;
}

/** 只改集合元数据；元数据变更不入版本链（D10），visibility 双向切换为破坏性操作（D15）。 */
export async function updateVarSet(
  varSetId: string,
  input: UpdateVarSetInput
): Promise<VarSet> {
  const res = await api.patch<VarSet>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}`,
    { var_set_id: varSetId, visibility: input.visibility, description: input.description }
  );
  return normalizeVarSet(res.data);
}

/** 删除集合：vars/heads/versions 经 FK CASCADE 连带销毁，全部历史不可恢复。 */
export async function deleteVarSet(varSetId: string): Promise<void> {
  await api.delete(`/server/runtime-var-sets/${encodeURIComponent(varSetId)}`);
}

// ---- 变量 ----

export async function listVars(varSetId: string): Promise<RuntimeVar[]> {
  const res = await api.get<{ vars?: RuntimeVar[] }>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars`,
    // 限额 500 keys/集合（D13），一页取全（page_size ≤1000）。
    { params: { page_size: 1000 } }
  );
  return (res.data.vars ?? []).map(normalizeVar);
}

export async function getVar(varSetId: string, key: string): Promise<RuntimeVar> {
  const res = await api.get<RuntimeVar>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars/${encodeURIComponent(key)}`
  );
  return normalizeVar(res.data);
}

/** 创建类型化变量（类型创建时锁定；value 恰携带一个 oneof 分支）。 */
export async function createVar(
  varSetId: string,
  input: { key: string; value: RuntimeVarValueInput; description?: string }
): Promise<RuntimeVar> {
  const res = await api.post<RuntimeVar>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars`,
    {
      var_set_id: varSetId,
      key: input.key,
      value: input.value,
      description: input.description ?? "",
    }
  );
  return normalizeVar(res.data);
}

/** 携带完整新值（必填，无 Upsert，D5）；类型可随本次变更；description 为全量表单值。 */
export async function updateVar(
  varSetId: string,
  key: string,
  input: { value: RuntimeVarValueInput; description?: string }
): Promise<RuntimeVar> {
  const res = await api.patch<RuntimeVar>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars/${encodeURIComponent(key)}`,
    { var_set_id: varSetId, key, value: input.value, description: input.description }
  );
  return normalizeVar(res.data);
}

export async function deleteVar(varSetId: string, key: string): Promise<void> {
  await api.delete(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars/${encodeURIComponent(key)}`
  );
}

// ---- 版本链 ----

/** 列出版本链元数据（保留最近 50 版/集合，D11；page_size ≤200 一页取全）。 */
export async function listVersions(varSetId: string): Promise<RuntimeVarVersion[]> {
  const res = await api.get<{ versions?: RuntimeVarVersion[] }>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/versions`,
    { params: { page_size: 200 } }
  );
  return (res.data.versions ?? []).map(normalizeVersion);
}

/** 获取某版本的全量快照（revision 1 起；版本内容不可变，可长缓存）。 */
export async function getVersion(
  varSetId: string,
  revision: number
): Promise<GetRuntimeVarVersionResponse> {
  const res = await api.get<GetRuntimeVarVersionResponse>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/versions/${revision}`
  );
  return {
    version: res.data.version ? normalizeVersion(res.data.version) : undefined,
    entries: normalizeEntries(res.data.entries),
  };
}

/**
 * 回滚到目标版本（custom verb：POST .../versions:rollback，body 携带
 * target_revision，不是 REST 子资源）。目标等于当前版本 → InvalidArgument
 *（D12）；已被 50 版窗口淘汰 → NotFound "target revision pruned"。
 */
export async function rollback(
  varSetId: string,
  targetRevision: number
): Promise<RuntimeVarVersion> {
  const res = await api.post<RuntimeVarVersion>(
    `/server/runtime-var-sets/${encodeURIComponent(varSetId)}/versions:rollback`,
    { var_set_id: varSetId, target_revision: targetRevision }
  );
  return normalizeVersion(res.data);
}
