/**
 * v4 JSON-RPC 2.0 服务面（docs/design/functions-v4.md §4，D10 RPC 轴）：
 * 函数常驻服务的标准入口之一——method 分发（invoke / http.request），复用
 * Mux 注册的 handler。响应恒 200（函数成败在 result / error 对象）；验签
 * 失败 401 在传输层（verify 中间件，见 verify.ts）。
 */
import { type HandlerContext, type Mux, type TriggerEnvelope, buildTriggerRequest, captureFetchResult } from "./handlers.js";
import { errorMessage } from "./verify.js";

/** JSON-RPC method 词表（与平台 domain protocol.go 同值；D10 双轴后 RPC 轴
 * 仅两 method——cron / 事件走事件轴 CloudEvents，见 cloudevents.ts）。 */
export const RPC_METHOD_INVOKE = "invoke";
export const RPC_METHOD_HTTP_REQUEST = "http.request";

/** JSON-RPC 错误码（spec §5.1 + server 区段）。 */
export const RPC_CODE_PARSE_ERROR = -32700;
export const RPC_CODE_INVALID_REQUEST = -32600;
export const RPC_CODE_METHOD_NOT_FOUND = -32601;
export const RPC_CODE_INVALID_PARAMS = -32602;
export const RPC_CODE_INTERNAL_ERROR = -32603;
export const RPC_CODE_SERVER_RANGE = -32000;

/**
 * v4 RPC 入口的 per-request 预算缺省 30s（平台侧按函数超时裁剪；此为函数
 * 侧兜底——signal 到点 abort，防止注册了慢 handler 的服务被单请求长期占用）。
 */
export const RPC_REQUEST_TIMEOUT_MS = 30 * 1000;

/** HttpResult 是 http.request 的 result 形状（与平台 HTTPResult / Go httpResult 同构）。 */
export interface HttpResult {
  status: number;
  headers?: Record<string, string>;
  body_b64?: string;
}

/**
 * makeContext 组装执行上下文（与 Go rpc.go identityContext 同构）：身份六件
 * + 30s 预算信号——token / source / invoking-user / project 四件走身份头，
 * executionId 是载荷关联键（RPC 轴 = JSON-RPC id，调用方白送的 invocation
 * 关联），apiBaseUrl 每请求读部署侧注入的 process.env.TW_API_BASE_URL。
 * X-Tw-Protocol-Version 不匹配时宽容继续（平台升版不立即打断旧 FDK）。
 */
export function makeContext(req: Request, executionId = ""): HandlerContext {
  const header = (name: string): string => req.headers.get(name) ?? "";
  return {
    identity: {
      executionToken: header("x-tw-execution-token"),
      apiBaseUrl: process.env.TW_API_BASE_URL ?? "",
      executionId,
      source: header("x-tw-source") || "server",
      invokingUserId: header("x-tw-invoking-user-id"),
      projectId: header("x-tw-project-id"),
    },
    signal: AbortSignal.timeout(RPC_REQUEST_TIMEOUT_MS),
  };
}

/**
 * serveRpcBody 解析并分发 JSON-RPC 请求（body 已由统一入口 newServer 读取
 * 并探测过形态）。
 */
export function serveRpcBody(req: Request, fields: Record<string, unknown>, mux: Mux): Promise<Response> {
  // Go unmarshal 语义镜像：null → 字段零值（静默）；非字符串非 null → 解析
  // 失败 → -32700（id 置空）。params 为 RawMessage 语义，任意 JSON 值放行。
  const jsonrpc = wireNullableString(fields["jsonrpc"]);
  const method = wireNullableString(fields["method"]);
  const id = wireNullableString(fields["id"]);
  if (jsonrpc === null || method === null || id === null) {
    return Promise.resolve(
      rpcError("", RPC_CODE_PARSE_ERROR, "malformed json-rpc request: jsonrpc/method/id must be strings")
    );
  }
  if (jsonrpc !== "2.0" || method === "") {
    return Promise.resolve(rpcError(id, RPC_CODE_INVALID_REQUEST, "not a json-rpc 2.0 request"));
  }

  const ctx = makeContext(req, id);
  const params = fields["params"];
  switch (method) {
    case RPC_METHOD_INVOKE:
      return serveRpcMain(id, params, ctx, mux);
    case RPC_METHOD_HTTP_REQUEST:
      return serveRpcHttp(id, params, ctx, mux);
    default:
      return Promise.resolve(rpcError(id, RPC_CODE_METHOD_NOT_FOUND, `unknown method ${method}`));
  }
}

/** wireNullableString 镜像 Go 的 string 字段 unmarshal：absent/null → ""，字符串原样，其余 → null（失败）。 */
function wireNullableString(v: unknown): string | null {
  if (v === undefined || v === null) return "";
  return typeof v === "string" ? v : null;
}

/**
 * serveRpcMain 处理 invoke：params 即 TW_DATA 语义直通 handler——缺省补空
 * 对象（对齐 Go twData 空 body → "{}"），其余 JSON 值原样透传。
 */
async function serveRpcMain(id: string, params: unknown, ctx: HandlerContext, mux: Mux): Promise<Response> {
  const invoke = mux.snapshot().invoke;
  if (!invoke) {
    return rpcError(id, RPC_CODE_METHOD_NOT_FOUND, "no Invoke handler registered");
  }
  let result: unknown;
  try {
    result = await invoke(params === undefined ? {} : params, ctx);
  } catch (err) {
    return rpcError(id, RPC_CODE_SERVER_RANGE, errorMessage(err));
  }
  if (result === undefined) result = null; // Go nil → JSON null
  try {
    JSON.stringify(result);
  } catch (err) {
    return rpcError(id, RPC_CODE_SERVER_RANGE, `handler result is not JSON-serializable: ${errorMessage(err)}`);
  }
  return rpcResult(id, result);
}

/**
 * serveRpcHttp 处理 http.request：params 组触发器封套 + body，还原真
 * Request 交 Fetch handler，响应捕获为 HttpResult（status/headers/body_b64）。
 */
async function serveRpcHttp(id: string, params: unknown, ctx: HandlerContext, mux: Mux): Promise<Response> {
  // params 形状校验（对齐 Go unmarshal 错误路径 → -32602）。
  if (params !== undefined && params !== null && (typeof params !== "object" || Array.isArray(params))) {
    return rpcError(id, RPC_CODE_INVALID_PARAMS, "http.request params: params must be an object");
  }
  const p = (params ?? {}) as { envelope?: unknown; body_b64?: unknown };
  if (p.body_b64 !== undefined && p.body_b64 !== null && typeof p.body_b64 !== "string") {
    return rpcError(id, RPC_CODE_INVALID_PARAMS, "http.request params: body_b64 must be a string");
  }
  if (p.envelope === undefined || p.envelope === null) {
    return rpcError(id, RPC_CODE_INVALID_PARAMS, "http.request params: missing envelope");
  }
  const env = validateEnvelope(p.envelope);
  if (env === null) {
    return rpcError(id, RPC_CODE_INVALID_PARAMS, "http.request params: invalid envelope");
  }
  const fetch = mux.snapshot().fetch;
  if (!fetch) {
    return rpcError(id, RPC_CODE_METHOD_NOT_FOUND, "no Fetch handler registered");
  }

  try {
    const triggerReq = buildTriggerRequest(env, decodeBodyB64(String(p.body_b64 ?? "")));
    const captured = await captureFetchResult(await fetch(triggerReq, ctx));
    const result: HttpResult = { status: captured.status };
    if (Object.keys(captured.headers).length > 0) result.headers = captured.headers;
    if (captured.body.byteLength > 0) result.body_b64 = encodeBase64(captured.body);
    return rpcResult(id, result);
  } catch {
    // 还原请求 / handler 执行 / 响应捕获任一失败 → server 区段（Go
    // env.OK false → "http request failed" 同款）。
    return rpcError(id, RPC_CODE_SERVER_RANGE, "http request failed");
  }
}

/**
 * validateEnvelope 校验封套形状（Go unmarshal 类型错误同路径 → -32602）；
 * 额外字段一律忽略（协议演进宪法）。
 */
function validateEnvelope(raw: unknown): TriggerEnvelope | null {
  if (raw === null || typeof raw !== "object" || Array.isArray(raw)) return null;
  const env = raw as { method?: unknown; path?: unknown; raw_query?: unknown; headers?: unknown };
  for (const key of ["method", "path", "raw_query"] as const) {
    const v = env[key];
    if (v !== undefined && v !== null && typeof v !== "string") return null;
  }
  if (env.headers !== undefined && env.headers !== null) {
    if (typeof env.headers !== "object" || Array.isArray(env.headers)) return null;
    for (const values of Object.values(env.headers)) {
      if (!Array.isArray(values)) return null;
      for (const v of values) {
        if (typeof v !== "string") return null;
      }
    }
  }
  return env as TriggerEnvelope;
}

/**
 * decodeBodyB64 解码 body_b64：严格 StdEncoding 口径（4 字节对齐 + 合法
 * 字母表 + 规范 padding）；非法 base64 不报错、回落为字面字符串 body
 * （Go DecodeString 失败 → raw = params.BodyB64 同款容错）。
 */
const STD_BASE64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{3}=|[A-Za-z0-9+/]{2}==)?$/;

function decodeBodyB64(b64: string): Uint8Array {
  if (!STD_BASE64.test(b64)) return new TextEncoder().encode(b64);
  const bin = atob(b64);
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) {
    out[i] = bin.charCodeAt(i);
  }
  return out;
}

/** encodeBase64 无损编码响应 body（btoa 走 latin1 二进制串中转）。 */
function encodeBase64(bytes: Uint8Array): string {
  let bin = "";
  for (let i = 0; i < bytes.byteLength; i++) {
    bin += String.fromCharCode(bytes[i]);
  }
  return btoa(bin);
}

/**
 * rpcResult / rpcError 恒写 200（送达成功；函数成败在 body）：result 与
 * error 互斥出现，id 回显，jsonrpc 恒 "2.0"。
 */
function rpcResult(id: string, result: unknown): Response {
  return rpcResponse(id, { result });
}

function rpcError(id: string, code: number, message: string): Response {
  return rpcResponse(id, { error: { code, message } });
}

function rpcResponse(id: string, body: Record<string, unknown>): Response {
  return new Response(`${JSON.stringify({ jsonrpc: "2.0", id, ...body })}\n`, {
    status: 200,
    headers: { "content-type": "application/json" },
  });
}
