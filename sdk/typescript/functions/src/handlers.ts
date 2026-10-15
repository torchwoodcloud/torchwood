/**
 * 四类 handler 契约 + 多源组合器 Mux（对标 Go `sdk/go/functions` 的
 * handlers.go）：invoke / cron / event / fetch 四类触发源显式注册（重复
 * 注册后者覆盖），由统一服务入口 newServer 按请求形态分发。
 */
import type { CronTick, DocumentChange } from "./events.js";

/** fetch 响应 body 截断上限（平台 maxOutputBytes；与 Go serve.go 同值）。 */
export const MAX_OUTPUT_BYTES = 64 * 1024;

/**
 * ExecutionIdentity 是 v4 服务面的执行身份四件（身份头投影；与 Go
 * identityContext 注入的 Identity 字段一一对应——v4 下 X-Tw-Execution-Id
 * 头退役、id 走 JSON-RPC body，身份头仅剩 token/source/invoking-user/project）。
 */
export interface ExecutionIdentity {
  /** 本次执行的短期凭证（x-tw-execution-token；Bearer twx_… 形态，供平台 API 鉴权，执行结束即失效）。 */
  executionToken: string;
  /** 调用来源（x-tw-source；恒非空——header 缺省回落 "server"）。 */
  source: string;
  /** 触发执行的端用户（x-tw-invoking-user-id；空串 = 非用户触发，系统语义）。 */
  invokingUserId: string;
  /** 执行所属项目（x-tw-project-id）。 */
  projectId: string;
}

/**
 * HandlerContext 是 handler 的执行上下文（Go context 携带 Identity +
 * deadline 的 TS 对称物）：identity 为身份四件；signal 为 per-request
 * 预算（30s 兜底，到点 abort——Go rpcRequestTimeout 同源）。
 */
export interface HandlerContext {
  identity: ExecutionIdentity;
  signal: AbortSignal;
}

/** InvokeHandler 处理 invoke 分发：data 为 TW_DATA JSON 值（JSON-RPC params 原文）。 */
export type InvokeHandler = (data: unknown, ctx: HandlerContext) => unknown | Promise<unknown>;

/** CronHandler 处理 cron.tick 通知；抛错 = 本次投递失败（进重试梯度）。 */
export type CronHandler = (tick: CronTick, ctx: HandlerContext) => void | Promise<void>;

/** EventHandler 处理文档 / 系统事件通知；抛错 = 本次投递失败。 */
export type EventHandler = (change: DocumentChange, ctx: HandlerContext) => void | Promise<void>;

/**
 * FetchHandler 处理 http.request 分发：收到按触发器封套还原的真 Request
 * （url = http://trigger{path}?{raw_query}，headers 原样），响应
 * status/headers/body 经封套捕获回传。执行身份经 ctx 显式传入（Go 侧走
 * r.Context() 注入；fetch Request 无上下文概念，TS 侧以参数承载）。
 */
export type FetchHandler = (req: Request, ctx: HandlerContext) => Response | Promise<Response>;

/**
 * TriggerEnvelope 是触发器封套元数据（http.request params.envelope 的
 * wire 形状，snake_case；与 Go triggerEnvelope / 平台 TriggerEnvelope
 * 同构）。额外字段一律忽略（协议演进宪法：只读已知字段）。
 */
export interface TriggerEnvelope {
  method?: string;
  path?: string;
  raw_query?: string;
  headers?: Record<string, string[]>;
}

/**
 * 响应头过滤清单（对照 runner.js RESPONSE_HEADER_BLOCKLIST）：hop-by-hop
 * + 平台头。content-length 由分发链路按实际 body 重算；date/server/host
 * 不得由用户代码冒充；其余头（含 content-type）原样透传。
 */
const RESPONSE_HEADER_BLOCKLIST = new Set([
  "connection",
  "keep-alive",
  "proxy-connection",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
  "content-length",
  "host",
  "date",
  "server",
]);

/** MuxSnapshot 是 Mux 的分发面快照（Go RLock 读取的 TS 对称物）。 */
export interface MuxSnapshot {
  invoke?: InvokeHandler;
  cron?: CronHandler;
  event?: EventHandler;
  fetch?: FetchHandler;
}

/** Mux 是多触发源显式组合器（单服务多源；复用 ServeMux/chi 心智）。 */
export class Mux {
  private invokeHandler?: InvokeHandler;
  private cronHandler?: CronHandler;
  private eventHandler?: EventHandler;
  private fetchHandler?: FetchHandler;

  /** invoke 注册 invoke 源（server/client 面调用）。 */
  invoke(fn: InvokeHandler): void {
    this.invokeHandler = fn;
  }

  /** cron 注册 cron 触发源（任意 trigger id 命中）。 */
  cron(fn: CronHandler): void {
    this.cronHandler = fn;
  }

  /** event 注册事件触发源（任意 trigger id 命中）。 */
  event(fn: EventHandler): void {
    this.eventHandler = fn;
  }

  /** fetch 注册 HTTP 触发源（任意 trigger id 命中）。 */
  fetch(fn: FetchHandler): void {
    this.fetchHandler = fn;
  }

  /** @internal 分发面快照（对齐 Go 侧 RLock 读取；JS 单线程无需锁）。 */
  snapshot(): MuxSnapshot {
    return {
      invoke: this.invokeHandler,
      cron: this.cronHandler,
      event: this.eventHandler,
      fetch: this.fetchHandler,
    };
  }
}

/**
 * buildTriggerRequest 还原 fetch 风格请求（对照 runner.js buildRequest /
 * Go buildTriggerRequest）：url = http://trigger{path}?{raw_query}、
 * headers 原样、body = 分发 body（GET/HEAD 不带 body）；method 大写化、
 * 缺省 GET。非法 method（非 HTTP token）抛 TypeError → 上层映射
 * "http request failed"。
 */
export function buildTriggerRequest(env: TriggerEnvelope, rawBody: Uint8Array): Request {
  const method = (env.method ?? "").toUpperCase() || "GET";
  let url = `http://trigger${env.path || "/"}`;
  if (env.raw_query) {
    url += `?${env.raw_query}`;
  }
  const headers = new Headers();
  for (const [name, values] of Object.entries(env.headers ?? {})) {
    if (!Array.isArray(values)) continue; // 形状已在 RPC 轴校验；防御性跳过
    for (const v of values) {
      headers.append(name, v);
    }
  }
  const init: RequestInit = { method, headers };
  if (method !== "GET" && method !== "HEAD" && rawBody.byteLength > 0) {
    // cast：TS ≥5.7 的 ArrayBufferView 泛型摩擦（BodyInit 钉 ArrayBuffer 后端），
    // 运行时 undici 接受任意 ArrayBufferView。
    init.body = rawBody as unknown as BodyInit;
  }
  return new Request(url, init);
}

/** CapturedFetch 是 fetch 响应的封套捕获（Go fetchResult 同款：64KB 截断 + 头过滤 + 小写合并头）。 */
export interface CapturedFetch {
  /** 函数自己的 HTTP status（封套 status 字段；传输层恒 200）。 */
  status: number;
  /** 函数设置的响应头（小写名 + 合并值；过滤清单外原样透传）。 */
  headers: Record<string, string>;
  /** 响应 body 字节（超 MAX_OUTPUT_BYTES 截断）。 */
  body: Uint8Array;
  /** body 是否被截断。 */
  truncated: boolean;
}

/** captureFetchResult 捕获 fetch 响应为封套（headers 迭代天然给出小写名与合并值——fetch Headers combined 语义）。 */
export async function captureFetchResult(res: Response): Promise<CapturedFetch> {
  let body = new Uint8Array(await res.arrayBuffer());
  let truncated = false;
  if (body.byteLength > MAX_OUTPUT_BYTES) {
    body = body.slice(0, MAX_OUTPUT_BYTES);
    truncated = true;
  }
  const headers: Record<string, string> = {};
  res.headers.forEach((value, name) => {
    if (RESPONSE_HEADER_BLOCKLIST.has(name)) return;
    headers[name] = value;
  });
  return { status: res.status, headers, body, truncated };
}
