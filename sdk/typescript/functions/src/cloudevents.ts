/**
 * v4 统一服务入口（与 Go `sdk/go/functions/cloudevents.go` 的 NewServer 同
 * 构）：可选探针 GET /_tw/health（部署平台用，不走验签）→ Verify 验签 →
 * body 形态探测 → RPC 轴（jsonrpc：invoke / http.request，见 rpc.ts）或事
 * 件轴（specversion：cron.tick → Cron、其余 type → Event）→ Mux 注册的
 * handler。请求体 4MB 上限；handler panic 捕获为 4xx/错误对象，不杀进程。
 * 可直接挂任意 HTTP 服务 / 云平台入口（node:http、k8s Service、serverless
 * function）。
 *
 * 事件轴（docs/design/functions-v4.md §4.2，D10 拍板）：cron / 事件触发以
 * CloudEvents 1.0 structured mode 通知送达（无应用层结果，2xx = 送达），
 * 与 RPC 轴共用统一入口——同一签名方案罩两面。
 */
import type { CronTick, DocumentChange } from "./events.js";
import { parseCronTick, parseDocumentChange } from "./events.js";
import type { Mux } from "./handlers.js";
import { makeContext, serveRpcBody } from "./rpc.js";
import {
  MAX_BODY_BYTES,
  errorMessage,
  readBodyLimited,
  textError,
  verify,
  type HttpHandler,
} from "./verify.js";

/** 事件轴常量（与平台 domain protocol.go 同值）。 */
const CLOUD_EVENTS_SPEC_VERSION = "1.0";
const CLOUD_EVENT_TYPE_CRON_TICK = "cron.tick";

/** 可选探针路径（与 Go healthPath 同值；部署平台探活用，非平台强制）。 */
const HEALTH_PATH = "/_tw/health";

/**
 * newServer 构建函数常驻服务的 v4 统一入口（Go NewServer(mux, secrets…)
 * 同构）。secrets 为函数侧签名密钥集（轮换窗口内新旧并存），至少一个——
 * 空集 fail-closed，构造期即抛。平台 API base（回访 Client 用）每请求读
 * 部署侧注入的 process.env.TW_API_BASE_URL。
 *
 * 用法：
 *
 * ```ts
 * import { Mux, newServer } from "@torchwood/functions";
 *
 * const mux = new Mux();
 * mux.invoke(async (data, ctx) => ({ pong: data }));
 * mux.cron(async (tick, ctx) => { … });
 * mux.event(async (change, ctx) => { … });
 * mux.fetch(async (req, ctx) => new Response("ok"));
 * serveHTTP(newServer(mux, "whsec_…"));
 * ```
 */
export function newServer(mux: Mux, ...secrets: string[]): HttpHandler {
  if (secrets.length === 0) {
    throw new Error("functions: verify middleware requires at least one secret");
  }
  const protocol = verify(...secrets)((req) => dispatch(req, mux));
  return (req) => {
    if (new URL(req.url).pathname === HEALTH_PATH) return serveHealth(req);
    return protocol(req);
  };
}

/**
 * serveHealth 是可选探针：GET → 200 JSON（进程活着即就绪）；其余方法 405。
 * 不走验签——部署平台探活不持签名密钥。
 */
function serveHealth(req: Request): Response {
  if (req.method !== "GET") {
    return new Response(`${JSON.stringify({ ok: false })}\n`, {
      status: 405,
      headers: { "content-type": "application/json", allow: "GET" },
    });
  }
  return new Response(`${JSON.stringify({ ok: true, ready: true })}\n`, {
    status: 200,
    headers: { "content-type": "application/json" },
  });
}

/**
 * dispatch 是统一入口内层：读 body（4MB 上限，413）→ JSON 探测分流——有
 * jsonrpc → RPC 轴、有 specversion → 事件轴、都没有 400 unrecognized
 * protocol（传输层拒绝，无信封可承载错误对象）。
 */
async function dispatch(req: Request, mux: Mux): Promise<Response> {
  let body: Uint8Array;
  try {
    body = await readBodyLimited(req, MAX_BODY_BYTES);
  } catch (err) {
    return textError(400, `functions: read body: ${errorMessage(err)}`);
  }
  if (body.byteLength > MAX_BODY_BYTES) {
    return textError(413, "functions: body too large");
  }
  let probe: unknown;
  try {
    probe = JSON.parse(new TextDecoder().decode(body));
  } catch {
    return textError(400, "functions: malformed json body");
  }
  if (probe === null || typeof probe !== "object" || Array.isArray(probe)) {
    return textError(400, "functions: malformed json body");
  }
  const fields = probe as Record<string, unknown>;
  if (fields["jsonrpc"] !== undefined) return serveRpcBody(req, fields, mux);
  if (fields["specversion"] !== undefined) return serveEventBody(req, fields, mux);
  return textError(400, "functions: unrecognized protocol (expect jsonrpc or specversion)");
}

/**
 * serveEventBody 解析并分发 CloudEvents 通知：cron.tick → data 解析为
 * CronTick → Cron handler；其余 type → data 解析为事件投影 → Event
 * handler。成功 204 No Content（无应用层结果）；解析 / 处理失败 400——
 * 对投递引擎即「本次投递失败」信号（进重试梯度）。
 */
async function serveEventBody(req: Request, fields: Record<string, unknown>, mux: Mux): Promise<Response> {
  // 信封形状校验（Go unmarshal 类型错误同路径 → 400 malformed cloud event）；
  // null 与缺席等价（Go string 字段零值语义）。额外属性一律忽略（协议演进
  // 宪法：twproject/twseq 扩展属性经 envelope 消费，不单独投影）。
  for (const key of ["specversion", "id", "source", "type", "time"]) {
    const v = fields[key];
    if (v !== undefined && v !== null && typeof v !== "string") {
      return textError(400, `functions: malformed cloud event: ${key} must be a string`);
    }
  }
  const specversion = (fields["specversion"] as string | undefined) ?? "";
  if (specversion !== CLOUD_EVENTS_SPEC_VERSION) {
    return textError(400, `functions: unsupported specversion ${specversion}`);
  }

  // 执行身份：token 头 + CloudEvent id（事件轴的执行关联键）+ 部署 env 的
  // API base；source 头缺省回落 "server"。
  const ctx = makeContext(req, (fields["id"] as string | undefined) ?? "");

  if ((fields["type"] as string | undefined ?? "") === CLOUD_EVENT_TYPE_CRON_TICK) {
    const cron = mux.snapshot().cron;
    if (!cron) {
      return textError(400, "functions: no Cron handler registered");
    }
    let tick: CronTick;
    try {
      tick = parseCronTick(fields["data"]);
    } catch (err) {
      return textError(400, `functions: ${errorMessage(err)}`);
    }
    try {
      await cron(tick, ctx);
    } catch (err) {
      return textError(400, `functions: cron handler: ${errorMessage(err)}`);
    }
    return new Response(null, { status: 204 });
  }

  const event = mux.snapshot().event;
  if (!event) {
    return textError(400, "functions: no Event handler registered");
  }
  let change: DocumentChange;
  try {
    change = parseDocumentChange(fields["data"]);
  } catch (err) {
    return textError(400, `functions: ${errorMessage(err)}`);
  }
  try {
    await event(change, ctx);
  } catch (err) {
    return textError(400, `functions: event handler: ${errorMessage(err)}`);
  }
  return new Response(null, { status: 204 });
}
