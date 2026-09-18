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
