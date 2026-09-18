import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { defineMain } from "../main.js";
import type { Ctx } from "../identity.js";

const fullCtx: Ctx = {
  executionToken: "twx_test_token",
  apiBaseUrl: "http://localhost:9080",
  executionId: "exec-1",
  source: "client",
  invokingUserId: "user-1",
  projectId: "proj-1",
};

function ctxWith(patch: Partial<Record<keyof Ctx, unknown>>): unknown {
  return { ...fullCtx, ...patch };
}

describe("defineMain", () => {
  it("透传 data/ctx 进 handler 并透传返回值（异步）", async () => {
    let seen: { data: unknown; ctx: Ctx } | null = null;
    const main = defineMain(async (data, ctx) => {
      seen = { data, ctx };
      return { pong: (data as { ping: string }).ping };
    });
    const result = await main({ ping: "hi" }, fullCtx);
    assert.deepEqual(result, { pong: "hi" });
    assert.deepEqual(seen, { data: { ping: "hi" }, ctx: fullCtx });
  });

  it("同步返回值 Promise 化", async () => {
    const main = defineMain((data) => ({ echo: data }));
    const result = await main("x", fullCtx);
    assert.deepEqual(result, { echo: "x" });
  });

  it("handler 抛错 → 包装函数 reject（runner 捕获落 500 封套）", async () => {
    const main = defineMain(() => {
      throw new Error("boom");
    });
    await assert.rejects(() => main({}, fullCtx), /boom/);
  });

  it("ctx 缺字段 → 带字段名的清晰错误", async () => {
    const { projectId: _omitted, ...rest } = fullCtx;
    const main = defineMain(() => null);
    await assert.rejects(
      () => main({}, rest as unknown as Ctx),
      /ctx\.projectId must be a string, got undefined/
    );
  });

  it("ctx 字段非 string → 报实际类型", async () => {
    const main = defineMain(() => null);
    await assert.rejects(
      () => main({}, ctxWith({ executionId: 42 }) as unknown as Ctx),
      /ctx\.executionId must be a string, got number/
    );
  });

  it("ctx 非 object → 清晰错误", async () => {
    const main = defineMain(() => null);
    await assert.rejects(() => main({}, undefined as unknown as Ctx), /ctx must be an object/);
    await assert.rejects(() => main({}, null as unknown as Ctx), /ctx must be an object/);
  });

  it("校验发生在 handler 之前（坏 ctx 不触达业务代码）", async () => {
    let called = false;
    const main = defineMain(() => {
      called = true;
      return null;
    });
    await assert.rejects(() => main({}, { source: "server" } as unknown as Ctx));
    assert.equal(called, false);
  });

  it("defineMain 收非函数 handler → 同步抛错", () => {
    assert.throws(() => defineMain(null as unknown as Parameters<typeof defineMain>[0]), /handler must be a function/);
    assert.throws(() => defineMain("nope" as unknown as Parameters<typeof defineMain>[0]), /handler must be a function/);
  });

  it("允许 runner 新增的额外 ctx 字段（协议演进：只读已知字段）", async () => {
    const main = defineMain((_data, ctx) => (ctx as Ctx & { extra: string }).extra);
    const result = await main({}, Object.assign({}, fullCtx, { extra: "future-field" }));
    assert.equal(result, "future-field");
  });
});
