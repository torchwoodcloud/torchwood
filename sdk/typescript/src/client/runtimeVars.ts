import type { HttpTransport } from "../http.js";

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

/** getRuntimeVars 结果：vars 为按 key 索引的全量快照。 */
export interface GetRuntimeVarsResult {
  /** 全量快照；unchanged=true 时为空对象（沿用调用方本地缓存）。 */
  vars: Record<string, RuntimeVarValue>;
  /** 不透明 token "{epoch}:{revision}"（D2）：下次轮询经 options.etag 透传，不要解析。 */
  etag: string;
  /** true = etag 命中短路（vars 为空），数据未变化。 */
  unchanged: boolean;
}

/**
 * RuntimeVarsService 是 Client API 的运行时变量拉取面（docs/design/
 * runtime-vars.md §2.3）：单一 getRuntimeVars 按集合全量快照，无单 key 读/
 * 分页/跨集合聚合（D4/D14）。public 集匿名可读；private 集需登录态。
 *
 * SDK 不内置 TTL 缓存（设计 §7 明确不做），轮询口径：
 * - 建议间隔 TTL ≥ 60s（runtime-vars 是读多写少的客户端行为参数）；
 * - 透传上次响应的 etag，命中则 unchanged=true 且 vars 为空（常态轮询仅几十字节）；
 * - 收到 404 保留 last-known-good 缓存（visibility 翻转 private 或集合删除，
 *   D15——App 回退内置默认值，不要清空生效配置）。
 */
export class ClientRuntimeVarsService {
  constructor(private readonly http: HttpTransport) {}

  /**
   * 拉取指定集合的全量快照。options.projectId 是匿名唯一可靠寻址（为空时
   * 回落 X-Torchwood-Project 头 → 登录 Principal）；options.etag 透传上次
   * 响应值以命中 unchanged 短路。
   */
  async getRuntimeVars(
    varSetId: string,
    options?: { projectId?: string; etag?: string }
  ): Promise<GetRuntimeVarsResult> {
    const query: Record<string, string | undefined> = {
      project_id: options?.projectId,
      etag: options?.etag,
    };
    for (const k of Object.keys(query)) {
      if (query[k] === undefined || query[k] === "") delete query[k];
    }
    const res = await this.http.request<{
      vars?: Record<string, RuntimeVarValue>;
      etag?: string;
      unchanged?: boolean;
    }>("GET", `/v1/runtime-vars/${encodeURIComponent(varSetId)}`, { query });
    return {
      vars: normalizeVars(res.vars),
      etag: res.etag ?? "",
      unchanged: res.unchanged ?? false,
    };
  }
}

/** int64 归一：grpc-gateway protojson 可能输出字符串（"3"），安全转 number。 */
function normalizeValue(v: RuntimeVarValue | undefined): RuntimeVarValue {
  if (!v) return {};
  const out: RuntimeVarValue = { ...v };
  if (typeof out.integer_value === "string") {
    out.integer_value = Number(out.integer_value);
  }
  return out;
}

function normalizeVars(vars: Record<string, RuntimeVarValue> | undefined): Record<string, RuntimeVarValue> {
  const out: Record<string, RuntimeVarValue> = {};
  for (const [key, value] of Object.entries(vars ?? {})) {
    out[key] = normalizeValue(value);
  }
  return out;
}
