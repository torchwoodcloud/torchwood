/**
 * Ctx 是 main 风格函数的执行上下文（functions 五期 5c）。
 *
 * 与平台 runner 注入的 ctx 逐字段同源（`internal/infra/functions/runner/
 * runner.js` 组装处）：身份六件经分发 header 传入，per-request 经
 * AsyncLocalStorage 隔离——并发函数**必须读 ctx**，不要读
 * `process.env.TW_EXECUTION_TOKEN`（含 await 的函数恢复执行后 env 可能已被
 * 后续请求覆盖，凭证串号窗口）。Go 侧对称物为 `sdk/go/functions` 的
 * `functions.Identity`。
 */
export interface Ctx {
  /** 本次执行的短期凭证（Bearer twx_… 形态，供平台 API 鉴权，执行结束即失效）。 */
  executionToken: string;
  /** 平台 API base（env TW_API_BASE_URL），如 https://api.example.com。 */
  apiBaseUrl: string;
  /** 平台执行 ID（日志关联）。 */
  executionId: string;
  /** 调用来源（恒非空）：server | client | http:{id} | cron:{id} | event:{id}。 */
  source: string;
  /** 触发执行的端用户 ID；空串 = 非用户触发（系统语义）。 */
  invokingUserId: string;
  /** 执行所属项目 ID。 */
  projectId: string;
}

/**
 * CTX_FIELDS 是 ctx 的规范字段清单（assertCtx 的校验面 = 错误文案的依据）。
 * runner 侧新增字段时旧版 SDK 保持可用（协议演进宪法：只读已知字段），
 * 故校验只要求「已知字段存在且为 string」，不拒绝额外字段。
 */
const CTX_FIELDS = [
  "executionToken",
  "apiBaseUrl",
  "executionId",
  "source",
  "invokingUserId",
  "projectId",
] as const;

/**
 * assertCtx 运行时校验 ctx 形状（六字段齐备且均为 string），缺字段/类型
 * 不符抛出带字段名的清晰错误——函数签名错误在**调用时**即暴露，而不是把
 * undefined 一路带进业务逻辑（设计动机：runner v5 之前平台可能注入旧形状
 * ctx，typed 契约的运行时半边）。校验通过返回原对象（透传，不拷贝）。
 */
export function assertCtx(value: unknown, origin: string): Ctx {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(
      `${origin}: ctx must be an object with fields {${CTX_FIELDS.join(", ")}}, got ${describe(value)}`
    );
  }
  const rec = value as Record<string, unknown>;
  for (const field of CTX_FIELDS) {
    const v = rec[field];
    if (typeof v !== "string") {
      throw new Error(
        `${origin}: ctx.${field} must be a string, got ${describe(v)} — runner injects ctx as {${CTX_FIELDS.join(", ")}}`
      );
    }
  }
  return value as Ctx;
}

function describe(v: unknown): string {
  if (v === undefined) return "undefined";
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}
