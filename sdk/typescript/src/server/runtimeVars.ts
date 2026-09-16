import { listQuery, type HttpTransport } from "../http.js";
import type { ListMeta, ListParams } from "../types.js";

// RuntimeVarsService 是 Server API 运行时变量管理面（docs/design/runtime-vars.md
// §2.3）：集合 CRUD + 变量 CRUD + 版本链查看/回滚，三组 13 方法。项目上下文
// 来自 API Key 凭证。SDK 不内置 TTL 缓存（设计 §7），轮询口径见 client 面
// getRuntimeVars 的 JSDoc。

/** 集合可见性（proto enum 名；网关 protojson 输入输出均接受名称或数值）。 */
export type VarSetVisibility =
  | "VAR_SET_VISIBILITY_PUBLIC"
  | "VAR_SET_VISIBILITY_PRIVATE";

/** 集合可见性输入：enum 名或数值（1=public，2=private；缺省由服务端归一为 public）。 */
export type VarSetVisibilityInput = VarSetVisibility | 1 | 2;

/** 版本链动作（proto enum 名）。 */
export type RuntimeVarVersionAction =
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
  /**
   * int64：grpc-gateway 以 protojson 输出 JSON 字符串（E2E 实测
   * `"integer_value":"3"`），本层已安全转换为 number（`typeof x === "string"
   * ? Number(x) : x`；值域 ±2^53−1 服务端已拦，转换无损）。
   */
  integer_value?: number;
  float_value?: number;
  bool_value?: boolean;
  /** 合法 JSON 文本（写路径已校验嵌套 ≤100），调用方按需自行 JSON.parse。 */
  json_value?: string;
}

/** 变量集合投影（footprint：var_count/total_bytes 供 D13 限额展示）。 */
export interface VarSet {
  var_set_id: string;
  /** enum 名：VAR_SET_VISIBILITY_PUBLIC / VAR_SET_VISIBILITY_PRIVATE。 */
  visibility: string;
  description: string;
  /** 不透明 token "{epoch}:{revision}"（D2）：轮询时原样透传，不要解析。 */
  etag: string;
  /** int64：网关输出字符串，本层已转 number。 */
  revision: number;
  var_count: number;
  /** int64：网关输出字符串，本层已转 number。 */
  total_bytes: number;
  created_at?: string;
  updated_at?: string;
}

/** 单个类型化变量（类型创建时锁定、变更走显式 Update，D3）。 */
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

/** rollback 响应即回滚后生成的版本链新条目（与 proto 返回 RuntimeVarVersion 一致）。 */
export type RollbackResponse = RuntimeVarVersion;

// ---- int64 归一（grpc-gateway protojson 输出字符串，如 "3"）----

function toNumber(v: number | string | undefined): number | undefined {
  if (v === undefined) return undefined;
  return typeof v === "string" ? Number(v) : v;
}

function normalizeValue(v: RuntimeVarValue | undefined): RuntimeVarValue | undefined {
  if (!v) return v;
  const out: RuntimeVarValue = { ...v };
  out.integer_value = toNumber(out.integer_value);
  return out;
}

function normalizeVarSet(v: VarSet | undefined): VarSet | undefined {
  if (!v) return v;
  return { ...v, revision: toNumber(v.revision) ?? 0, total_bytes: toNumber(v.total_bytes) ?? 0 };
}

function normalizeVar(v: RuntimeVar | undefined): RuntimeVar | undefined {
  if (!v) return v;
  return { ...v, value: normalizeValue(v.value) };
}

function normalizeVersion(v: RuntimeVarVersion | undefined): RuntimeVarVersion | undefined {
  if (!v) return v;
  return { ...v, revision: toNumber(v.revision) ?? 0 };
}

function normalizeEntries(entries: RuntimeVarVersionEntry[] | undefined): RuntimeVarVersionEntry[] {
  return (entries ?? []).map((e) => ({ ...e, value: normalizeValue(e.value) }));
}

/** 分页参数（ListRuntimeVars page_size ≤1000；ListRuntimeVarVersions ≤200）。 */
export interface RuntimeVarsPageParams {
  page_size?: number;
  page_token?: string;
}

export class RuntimeVarsService {
  constructor(private readonly http: HttpTransport) {}

  // ---- 集合 ----

  /** 创建集合（var_set_id 创建后不可改，D9；visibility 缺省 public）。 */
  async createVarSet(input: {
    var_set_id: string;
    visibility?: VarSetVisibilityInput;
    description?: string;
  }): Promise<VarSet> {
    const raw = await this.http.request<VarSet>("POST", "/v1/server/runtime-var-sets", {
      auth: "apiKey",
      body: {
        var_set_id: input.var_set_id,
        visibility: input.visibility,
        description: input.description ?? "",
      },
    });
    return normalizeVarSet(raw)!;
  }

  /** 列出集合（返回裸数组；需要分页 meta 用 listVarSetsWithMeta）。 */
  async listVarSets(params?: ListParams): Promise<VarSet[]> {
    const res = await this.listVarSetsWithMeta(params);
    return res.var_sets;
  }

  async listVarSetsWithMeta(params?: ListParams): Promise<{ var_sets: VarSet[]; meta?: ListMeta }> {
    const res = await this.http.request<{ var_sets?: VarSet[]; meta?: ListMeta }>(
      "GET",
      "/v1/server/runtime-var-sets",
      { auth: "apiKey", query: listQuery(params) }
    );
    return { var_sets: (res.var_sets ?? []).map((v) => normalizeVarSet(v)!), meta: res.meta };
  }

  async getVarSet(varSetId: string): Promise<VarSet> {
    const raw = await this.http.request<VarSet>(
      "GET",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}`,
      { auth: "apiKey" }
    );
    return normalizeVarSet(raw)!;
  }

  /** 只改集合元数据（未设置 = 不修改）；元数据变更不入版本链（D10）。 */
  async updateVarSet(
    varSetId: string,
    input: { visibility?: VarSetVisibilityInput; description?: string }
  ): Promise<VarSet> {
    const raw = await this.http.request<VarSet>(
      "PATCH",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}`,
      {
        auth: "apiKey",
        // proto3 optional：undefined 不会被 JSON 序列化，语义即"不修改"。
        body: { var_set_id: varSetId, visibility: input.visibility, description: input.description },
      }
    );
    return normalizeVarSet(raw)!;
  }

  /** 删除集合：vars/heads/versions 经 FK CASCADE 连带销毁，全部历史不可恢复。 */
  async deleteVarSet(varSetId: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}`,
      { auth: "apiKey" }
    );
  }

  // ---- 变量 ----

  /** 创建类型化变量（类型创建时锁定；value 恰携带一个分支）。 */
  async createVar(
    varSetId: string,
    input: { key: string; value: RuntimeVarValueInput; description?: string }
  ): Promise<RuntimeVar> {
    const raw = await this.http.request<RuntimeVar>(
      "POST",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars`,
      {
        auth: "apiKey",
        body: { var_set_id: varSetId, key: input.key, value: input.value, description: input.description ?? "" },
      }
    );
    return normalizeVar(raw)!;
  }

  async listVars(varSetId: string, params?: RuntimeVarsPageParams): Promise<RuntimeVar[]> {
    const res = await this.listVarsWithMeta(varSetId, params);
    return res.vars;
  }

  async listVarsWithMeta(
    varSetId: string,
    params?: RuntimeVarsPageParams
  ): Promise<{ vars: RuntimeVar[]; meta?: ListMeta }> {
    const res = await this.http.request<{ vars?: RuntimeVar[]; meta?: ListMeta }>(
      "GET",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars`,
      {
        auth: "apiKey",
        query: { page_size: params?.page_size, page_token: params?.page_token },
      }
    );
    return { vars: (res.vars ?? []).map((v) => normalizeVar(v)!), meta: res.meta };
  }

  async getVar(varSetId: string, key: string): Promise<RuntimeVar> {
    const raw = await this.http.request<RuntimeVar>(
      "GET",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars/${encodeURIComponent(key)}`,
      { auth: "apiKey" }
    );
    return normalizeVar(raw)!;
  }

  /** 携带完整新值（必填，无 Upsert，D5）；类型可随本次变更。 */
  async updateVar(
    varSetId: string,
    key: string,
    input: { value: RuntimeVarValueInput; description?: string }
  ): Promise<RuntimeVar> {
    const raw = await this.http.request<RuntimeVar>(
      "PATCH",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars/${encodeURIComponent(key)}`,
      {
        auth: "apiKey",
        body: { var_set_id: varSetId, key, value: input.value, description: input.description },
      }
    );
    return normalizeVar(raw)!;
  }

  async deleteVar(varSetId: string, key: string): Promise<void> {
    await this.http.request<void>(
      "DELETE",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/vars/${encodeURIComponent(key)}`,
      { auth: "apiKey" }
    );
  }

  // ---- 版本链 ----

  /** 列出版本链元数据（保留最近 50 版/集合，D11；page_size ≤200）。 */
  async listVersions(varSetId: string, params?: RuntimeVarsPageParams): Promise<RuntimeVarVersion[]> {
    const res = await this.listVersionsWithMeta(varSetId, params);
    return res.versions;
  }

  async listVersionsWithMeta(
    varSetId: string,
    params?: RuntimeVarsPageParams
  ): Promise<{ versions: RuntimeVarVersion[]; meta?: ListMeta }> {
    const res = await this.http.request<{ versions?: RuntimeVarVersion[]; meta?: ListMeta }>(
      "GET",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/versions`,
      {
        auth: "apiKey",
        query: { page_size: params?.page_size, page_token: params?.page_token },
      }
    );
    return { versions: (res.versions ?? []).map((v) => normalizeVersion(v)!), meta: res.meta };
  }

  /** 获取某版本的全量快照（revision 1 起）。 */
  async getVersion(varSetId: string, revision: number | string): Promise<GetRuntimeVarVersionResponse> {
    const res = await this.http.request<GetRuntimeVarVersionResponse>(
      "GET",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/versions/${revision}`,
      { auth: "apiKey" }
    );
    return { version: normalizeVersion(res.version), entries: normalizeEntries(res.entries) };
  }

  /**
   * 回滚到目标版本（custom verb，POST .../versions:rollback）。目标等于当前
   * 版本 → InvalidArgument；已被窗口淘汰 → NotFound "target revision pruned"。
   */
  async rollback(varSetId: string, targetRevision: number | string): Promise<RollbackResponse> {
    const raw = await this.http.request<RuntimeVarVersion>(
      "POST",
      `/v1/server/runtime-var-sets/${encodeURIComponent(varSetId)}/versions:rollback`,
      { auth: "apiKey", body: { var_set_id: varSetId, target_revision: targetRevision } }
    );
    return normalizeVersion(raw)!;
  }
}
