/**
 * @torchwood/functions — Torchwood Functions TypeScript 运行时 SDK
 * （Functions v4）。函数 = 常驻服务：平台经「签名 HTTP + 双轴协议」调用
 * 它（RPC 轴 JSON-RPC 2.0 / 事件轴 CloudEvents 1.0），本包是该协议的参考
 * 实现——统一入口 newServer（验签 + body 探测 + Mux 分发）+ typed handler
 * 契约 + 平台 API 客户端；用户以任意 node:http / 云平台 HTTP 入口托管。
 * Go 侧对称物 `sdk/go/functions` 同构（同一签名方案罩两面，按 body 形态探
 * 测分流）。
 *
 * 自包含零运行时依赖（Go 侧同款约束），仅依赖运行时的 fetch / 全局对象与
 * 部署侧注入的 env（TW_API_BASE_URL）。
 */
export { Client, APIError } from "./client.js";
export type { PlatformDocument, ClientOptions } from "./client.js";
// v4 服务面（D10 双轴：RPC 轴 JSON-RPC 2.0 / 事件轴 CloudEvents 1.0；与 Go
// 的 verify.go / rpc.go / cloudevents.go / handlers.go 同构——同一签名方案
// 罩两面，按 body 形态探测分流）。
export { newServer } from "./cloudevents.js";
export { Mux } from "./handlers.js";
export type {
  InvokeHandler,
  CronHandler,
  EventHandler,
  FetchHandler,
  HandlerContext,
  ExecutionIdentity,
  TriggerEnvelope,
} from "./handlers.js";
export {
  verify,
  computeSignature,
  verifySignatureHeader,
  parseSignatureHeader,
  buildSignatureHeader,
  signaturePayload,
  CANONICAL_SIGNATURE_VECTOR,
  PROTOCOL_VERSION,
} from "./verify.js";
export type { HttpHandler } from "./verify.js";
export { RPC_METHOD_INVOKE, RPC_METHOD_HTTP_REQUEST } from "./rpc.js";
export type { HttpResult } from "./rpc.js";
// 触发器载荷 typed 视图（形状对照平台事件投影；解析在 SDK 侧完成，函数代
// 码不接触 wire 字段名）。
export type { CronTick, DocumentChange, DocumentProjection } from "./events.js";
