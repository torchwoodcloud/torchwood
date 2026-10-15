/**
 * @torchwood/functions — Torchwood Functions TypeScript 运行时 SDK（函数
 * 五期 5c）。owner 裁决（2026-09-18）：node/TS 函数的用户侧 API 升级为
 * SDK——typed defineMain + 平台 API 客户端；平台注入 runner.js 的 serve
 * 机制保留不变，本包是用户函数 package.json 的依赖（走代装链路自动安装）。
 *
 * 自包含零运行时依赖（Go 侧对称物 `sdk/go/functions` 同款约束），仅依赖
 * 运行时的 fetch / 全局对象。
 */
export { defineMain } from "./main.js";
export type { MainHandler, MainEntrypoint } from "./main.js";
export type { Ctx } from "./identity.js";
export { Client, APIError } from "./client.js";
export type { PlatformDocument, ClientOptions } from "./client.js";
// 触发器载荷 typed 视图（typed 入口变体在后续阶段接入，类型面先行——
// 形状对照 internal/domain/functions/eventdata.go 与 cronEnvelope）。
export type { CronTick, DocumentChange, DocumentProjection } from "./events.js";
// v4 常驻服务面（D10 双轴：RPC 轴 JSON-RPC 2.0 / 事件轴 CloudEvents 1.0；
// 与 Go `sdk/go/functions` 的 verify.go / rpc.go / cloudevents.go / handlers.go
// 同构——同一签名方案罩两面，按 body 形态探测分流）。
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
