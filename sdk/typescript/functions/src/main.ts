import { assertCtx, type Ctx } from "./identity.js";

/**
 * MainHandler 是用户业务函数：data 为 TW_DATA 反序列化后的 JSON 值
 * （unknown——schema 由函数自定义，平台不约束）；ctx 为强类型执行上下文。
 * 同步返回值与 Promise 均可（runner 侧统一 Promise 化）。
 */
export type MainHandler = (data: unknown, ctx: Ctx) => unknown | Promise<unknown>;

/**
 * MainEntrypoint 是 defineMain 的产物：导出为 index.js 的 `main` 后即满足
 * runner 契约（`mod.main` 为 function → main 风格，见 runner.js 入口探测）。
 * 返回值恒为 Promise；拒绝（throw/reject）由 runner 捕获为 500 封套
 * `{ok:false,error}`。
 */
export type MainEntrypoint = (data: unknown, ctx: Ctx) => Promise<unknown>;

/**
 * defineMain 声明 main 风格函数入口（一期语义 = 类型标识 + 运行时校验）：
 *
 * ```ts
 * import { defineMain, Client } from "@torchwood/functions";
 *
 * export const main = defineMain(async (data, ctx) => {
 *   const client = new Client(ctx);
 *   const doc = await client.createDocument("app", "logs", { at: Date.now() });
 *   return { created: doc.id };
 * });
 * ```
 *
 * 运行时行为：每次调用先校验 ctx 形状（六字段齐备且为 string，缺字段抛
 * 带字段名的清晰错误——错误经 runner 落 500 封套），然后透传 data/ctx 进
 * handler 并透传返回值。后续变体（fetch/cron/event 的 typed 入口）将以
 * 同款 `define*` 命名扩展，不改动本函数。
 */
export function defineMain(fn: MainHandler): MainEntrypoint {
  if (typeof fn !== "function") {
    throw new Error(`defineMain: handler must be a function, got ${fn === null ? "null" : typeof fn}`);
  }
  return async (data: unknown, ctx: Ctx): Promise<unknown> => {
    assertCtx(ctx, "defineMain");
    return await fn(data, ctx);
  };
}
