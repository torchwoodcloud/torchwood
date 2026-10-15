/**
 * v4 调用协议的验签面（docs/design/functions-v4.md §4）。SDK 是独立包、
 * 不能 import 平台 internal/——本文件自持签名方案实现，口径与 Go FDK
 * `sdk/go/functions/verify.go`、平台 `internal/domain/functions/protocol.go`
 * **逐字节一致**：载荷串 `<t>.<body>`、HMAC-SHA256 hex、头格式 t=/v1=/v2=、
 * 时间窗 ±5 分钟、多密钥任一命中、常数时间比较。三面漂移由共享基准向量
 * 测试钉死（CANONICAL_SIGNATURE_VECTOR）。
 */

import { createHmac } from "node:crypto";

/** 协议头（与 Go serve.go 头族同处声明；v4 新增）。 */
export const HDR_SIGNATURE = "X-Tw-Signature";
export const HDR_PROTOCOL_VERSION = "X-Tw-Protocol-Version";

/** 扩展协议版本（与平台 domain protocol.go 同值）。 */
export const PROTOCOL_VERSION = "1";

/** 签名时间窗（防重放）：±5 分钟。 */
export const SIGNATURE_TIMESTAMP_TOLERANCE_MS = 5 * 60 * 1000;

/** 请求体上限 4MB（触发器封套双编码口径；与 Go maxBodyBytes 同值）。 */
export const MAX_BODY_BYTES = 4 << 20;

/**
 * 跨仓基准向量：HMAC-SHA256("whsec_vector", "1759000000.abc") 的 hex。
 * 平台侧 protocol_test.go、Go/TS 两侧 FDK 测试三方共用同一向量，任一侧
 * 口径漂移即红。
 */
export const CANONICAL_SIGNATURE_VECTOR =
  "e91f630384f34686e4d97568d65a25531151d944aabd0569f4cfbe39282f29c6";

/**
 * HttpHandler 是 fetch 风格服务面形态（Go http.Handler 的 TS 对称物：
 * 收真 Request、回真 Response，可直接挂 node:http 裸服务 / 云平台 HTTP
 * 入口 / 测试直调）。
 */
export type HttpHandler = (req: Request) => Response | Promise<Response>;

/**
 * errorMessage 归一错误文本（对齐 Go err.Error()：Error 取 message，
 * 其余值 String 化——handler 可 throw 任意值）。
 */
export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

/**
 * signaturePayload 返回参与签名的载荷字节 `<unix秒>.<body>`（口径：单一
 * 函数，禁止手拼）。Go 侧返回 string（保字节），TS 侧等价物为字节串。
 */
export function signaturePayload(timestamp: number, body: Uint8Array): Uint8Array {
  const prefix = new TextEncoder().encode(`${timestamp}.`);
  const out = new Uint8Array(prefix.length + body.byteLength);
  out.set(prefix, 0);
  out.set(body, prefix.length);
  return out;
}

/** computeSignature 返回 HMAC-SHA256(secret, payload) 的 hex 编码。 */
export function computeSignature(secret: string, timestamp: number, body: Uint8Array): string {
  return createHmac("sha256", secret).update(signaturePayload(timestamp, body)).digest("hex");
}

/** SignatureEntry 是签名头条目（v1=当前钥，v2=轮换旧钥）。 */
export type SignatureEntry = readonly [version: string, hex: string];

/** buildSignatureHeader 组装签名头（t= 前缀 + 条目逗号连接；本地开发联调 / 自签测试用）。 */
export function buildSignatureHeader(timestamp: number, entries: readonly SignatureEntry[]): string {
  const parts = [`t=${timestamp}`];
  for (const [version, hex] of entries) {
    parts.push(`${version}=${hex}`);
  }
  return parts.join(",");
}

/** ParsedSignatureHeader 是 parseSignatureHeader 的产物。 */
export interface ParsedSignatureHeader {
  /** 签名时刻（unix 秒）。 */
  timestamp: number;
  /** 版本 → hex 签名（同名后者覆盖，与 Go map 语义一致）。 */
  signatures: Map<string, string>;
}

/**
 * parseSignatureHeader 解析签名头（缺 t / 非法时间戳 / 空签名 fail-closed
 * 抛错）。无 "=" / 空 key / 空 value 的片段跳过（Go strings.Cut 同款）。
 */
export function parseSignatureHeader(header: string): ParsedSignatureHeader {
  const signatures = new Map<string, string>();
  let timestamp = 0;
  for (const rawPart of header.split(",")) {
    const part = rawPart.trim();
    const eq = part.indexOf("=");
    if (eq <= 0) continue; // 无 "=" 或空 key → 跳过
    const key = part.slice(0, eq);
    const value = part.slice(eq + 1);
    if (value === "") continue;
    if (key === "t") {
      const parsed = parseUnixSeconds(value);
      if (parsed === null) {
        throw new Error(`signature header: invalid timestamp ${JSON.stringify(value)}`);
      }
      timestamp = parsed;
      continue;
    }
    signatures.set(key, value);
  }
  if (timestamp === 0) throw new Error("signature header: missing t");
  if (signatures.size === 0) throw new Error("signature header: no signature entries");
  return { timestamp, signatures };
}

/**
 * verifySignatureHeader 是完整校验：时间窗内 + 任一 (secret, 签名) 命中
 * （常数时间比较）。secrets 是函数侧持有的密钥集（轮换窗口内新旧并存）；
 * 任一环节失败抛错，通过则静默返回（Go 返回 error / nil 的 TS 对称物）。
 */
export function verifySignatureHeader(
  header: string,
  body: Uint8Array,
  secrets: readonly string[],
  now: Date = new Date()
): void {
  const { timestamp, signatures } = parseSignatureHeader(header);
  const delta = now.getTime() - timestamp * 1000;
  if (delta < -SIGNATURE_TIMESTAMP_TOLERANCE_MS || delta > SIGNATURE_TIMESTAMP_TOLERANCE_MS) {
    throw new Error(`signature header: timestamp ${timestamp} outside tolerance`);
  }
  for (const secret of secrets) {
    const want = computeSignature(secret, timestamp, body);
    for (const got of signatures.values()) {
      if (constantTimeEqual(want, got)) return;
    }
  }
  throw new Error("signature header: no signature match");
}

/**
 * constantTimeEqual 常数时间比较 hex 串（长度不等即假——与 Go hmac.Equal
 * 同口径：长度差异本身可泄漏，比较体恒长）。
 */
function constantTimeEqual(a: string, b: string): boolean {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) {
    diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  }
  return diff === 0;
}

/** parseUnixSeconds 严格十进制整数解析（Go strconv.ParseInt 的 TS 口径；越界/含尾随字符 → null）。 */
function parseUnixSeconds(v: string): number | null {
  if (!/^[+-]?\d+$/.test(v)) return null;
  const n = Number(v);
  return Number.isSafeInteger(n) ? n : null;
}

/**
 * readBodyLimited 读取请求体，累计超过 limit 即停（Go
 * io.LimitReader(maxBodyBytes+1) 同款——读够判 413 即止，不吞无界 body）。
 * 读取失败向上抛（调用方回 400）。
 */
export async function readBodyLimited(req: Request, limit: number): Promise<Uint8Array> {
  if (req.body === null) return new Uint8Array(0);
  const reader = req.body.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      chunks.push(value);
      total += value.byteLength;
      if (total > limit) break;
    }
  } finally {
    void reader.cancel().catch(() => {}); // 读毕 / 超限都释放底层流
  }
  const out = new Uint8Array(Math.min(total, limit + 1));
  let offset = 0;
  for (const chunk of chunks) {
    if (offset >= out.length) break;
    const take = Math.min(chunk.byteLength, out.length - offset);
    out.set(chunk.subarray(0, take), offset);
    offset += take;
  }
  return out;
}

/** textError 镜像 Go http.Error：text/plain + 尾换行。 */
export function textError(status: number, message: string): Response {
  return new Response(`${message}\n`, {
    status,
    headers: { "content-type": "text/plain; charset=utf-8" },
  });
}

/**
 * verify 是验签中间件：校验 X-Tw-Signature（对原始 body 逐字节验签），
 * 失败 401；成功后 body 原样续传给下一 handler。secrets 至少一个（空集
 * fail-closed——无密钥等于不设防；TS 侧在构造期即抛，比 Go 的请求期 500
 * 更早暴露配置错误）。
 */
export function verify(...secrets: string[]): (next: HttpHandler) => HttpHandler {
  if (secrets.length === 0) {
    throw new Error("functions: verify middleware requires at least one secret");
  }
  return (next: HttpHandler): HttpHandler => {
    return async (req: Request): Promise<Response> => {
      let body: Uint8Array;
      try {
        body = await readBodyLimited(req, MAX_BODY_BYTES);
      } catch (err) {
        return textError(400, `functions: read body: ${errorMessage(err)}`);
      }
      if (body.byteLength > MAX_BODY_BYTES) {
        return textError(413, "functions: body too large");
      }
      try {
        verifySignatureHeader(req.headers.get(HDR_SIGNATURE) ?? "", body, secrets);
      } catch (err) {
        return textError(401, `functions: ${errorMessage(err)}`);
      }
      // 原流已被消费：重建携带缓冲 body 的请求（Go r.Body 重挂同款）。
      return next(rebuildRequest(req, body));
    };
  };
}

/** rebuildRequest 复制 method/headers/url 并挂回已读 body（GET/HEAD 不带 body）。 */
function rebuildRequest(req: Request, body: Uint8Array): Request {
  const init: RequestInit = { method: req.method, headers: req.headers };
  if (body.byteLength > 0 && req.method !== "GET" && req.method !== "HEAD") {
    // cast：TS ≥5.7 的 ArrayBufferView 泛型摩擦（BodyInit 钉 ArrayBuffer 后端），
    // 运行时 undici 接受任意 ArrayBufferView。
    init.body = body as unknown as BodyInit;
  }
  return new Request(req.url, init);
}
