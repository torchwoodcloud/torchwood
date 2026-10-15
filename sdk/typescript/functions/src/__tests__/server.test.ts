import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { newServer } from "../cloudevents.js";
import { Mux, type HandlerContext } from "../handlers.js";
import {
  RPC_CODE_INVALID_PARAMS,
  RPC_CODE_METHOD_NOT_FOUND,
  RPC_CODE_PARSE_ERROR,
  RPC_CODE_SERVER_RANGE,
  RPC_METHOD_HTTP_REQUEST,
  RPC_METHOD_INVOKE,
} from "../rpc.js";
import { buildSignatureHeader, computeSignature, MAX_BODY_BYTES, type HttpHandler } from "../verify.js";
import * as pkg from "../index.js";

const SECRET = "whsec_test";

const utf8 = (s: string): Uint8Array<ArrayBuffer> => new Uint8Array(new TextEncoder().encode(s));
const b64 = (s: string): string => btoa(s);

// signedRequest 组装一个经协议签名的请求（联调/测试辅助口径，对标 Go
// rpc_test.go 的 signedRPCRequest）。
function signedRequest(body: string, secret: string, headers?: Record<string, string>): Request {
  const ts = Math.floor(Date.now() / 1000);
  const h = new Headers(headers);
  h.set("x-tw-signature", buildSignatureHeader(ts, [["v1", computeSignature(secret, ts, utf8(body))]]));
  return new Request("http://function/", { method: "POST", headers: h, body: utf8(body) });
}

const identityHeaders = (): Record<string, string> => ({
  "x-tw-execution-token": "twx_tok",
  "x-tw-source": "client",
  "x-tw-invoking-user-id": "usr_1",
  "x-tw-project-id": "prj_1",
});

// signedRpcRequest 组装经签名的 JSON-RPC invoke / http.request 请求。
function signedRpcRequest(
  method: string,
  params: unknown,
  id: string,
  secret: string,
  headers?: Record<string, string>
): Request {
  const body = JSON.stringify(params === undefined ? { jsonrpc: "2.0", method, id } : { jsonrpc: "2.0", method, params, id });
  return signedRequest(body, secret, headers);
}

// signedEventRequest 组装一个经协议签名的 CloudEvents 通知请求（事件轴）。
function signedEventRequest(
  eventType: string,
  data: unknown,
  secret: string,
  specversion = "1.0",
  headers?: Record<string, string>
): Request {
  const body = JSON.stringify({
    specversion,
    id: "evt_1",
    source: "projects/prj_1/triggers/trg_1",
    type: eventType,
    time: "2026-10-15T00:00:00Z",
    data,
  });
  return signedRequest(body, secret, headers);
}

// RpcPayload 是 JSON-RPC 响应 wire 形状（result 与 error 互斥）。
interface RpcPayload {
  jsonrpc: string;
  id: string;
  result?: unknown;
  error?: { code: number; message: string };
}

async function rpcPayload(res: Response): Promise<RpcPayload> {
  return (await res.json()) as RpcPayload;
}

// ---- 导出面 ----

describe("导出面（index.ts）", () => {
  it("v4 服务面导出齐备：newServer / Mux / verify / 签名工具 / method 词表", () => {
    assert.equal(typeof pkg.newServer, "function");
    assert.equal(typeof pkg.Mux, "function");
    assert.equal(typeof pkg.verify, "function");
    assert.equal(typeof pkg.computeSignature, "function");
    assert.equal(typeof pkg.verifySignatureHeader, "function");
    assert.equal(typeof pkg.parseSignatureHeader, "function");
    assert.equal(typeof pkg.buildSignatureHeader, "function");
    assert.equal(typeof pkg.signaturePayload, "function");
    assert.equal(pkg.RPC_METHOD_INVOKE, "invoke");
    assert.equal(pkg.RPC_METHOD_HTTP_REQUEST, "http.request");
  });
});

// ---- RPC 轴（JSON-RPC 2.0）----

describe("newServer · RPC 轴", () => {
  it("invoke 分发全链：验签过 → Mux.Invoke 收到 params 原文与身份 → result 回包 + id 回显（HTTP 恒 200）", async () => {
    const mux = new Mux();
    const captured: Array<{ data: unknown; ctx: HandlerContext }> = [];
    mux.invoke(async (data, ctx) => {
      captured.push({ data, ctx });
      return { answer: 42 };
    });

    const res = await newServer(mux, SECRET)(
      signedRpcRequest(RPC_METHOD_INVOKE, { q: "hi" }, "exec_9", SECRET, identityHeaders())
    );
    assert.equal(res.status, 200, "函数成败在 body，HTTP 恒 200");
    assert.match(res.headers.get("content-type") ?? "", /application\/json/);
    const payload = await rpcPayload(res);
    assert.equal(payload.jsonrpc, "2.0");
    assert.equal(payload.id, "exec_9", "id 回显");
    assert.deepEqual(payload.result, { answer: 42 });
    assert.equal(payload.error, undefined);
    assert.equal(captured.length, 1);
    assert.deepEqual(captured[0].data, { q: "hi" });
    // 身份四件（身份头注入 ctx）。
    assert.deepEqual(captured[0].ctx.identity, {
      executionToken: "twx_tok",
      source: "client",
      invokingUserId: "usr_1",
      projectId: "prj_1",
    });
    assert.ok(captured[0].ctx.signal instanceof AbortSignal, "per-request 预算信号已注入");
  });

  it("x-tw-source 头缺省回落 \"server\"", async () => {
    const mux = new Mux();
    let gotSource = "";
    mux.invoke((_data, ctx) => {
      gotSource = ctx.identity.source;
      return null;
    });
    const headers = identityHeaders();
    delete headers["x-tw-source"];
    await newServer(mux, SECRET)(signedRpcRequest(RPC_METHOD_INVOKE, {}, "x", SECRET, headers));
    assert.equal(gotSource, "server");
  });

  it("invoke：params 缺省 → handler 收空对象（Go twData 空 body → \"{}\" 同款）", async () => {
    const mux = new Mux();
    let gotData: unknown = "sentinel";
    mux.invoke((data) => {
      gotData = data;
      return "ok";
    });
    const res = await newServer(mux, SECRET)(signedRpcRequest(RPC_METHOD_INVOKE, undefined, "exec_p", SECRET));
    assert.equal(res.status, 200);
    assert.deepEqual(gotData, {});
    assert.deepEqual((await rpcPayload(res)).result, "ok");
  });

  it("函数 handler 报错 → error 对象（-32000 区段），HTTP 仍 200", async () => {
    const mux = new Mux();
    mux.invoke(async () => {
      throw new Error("boom: deadline");
    });
    const res = await newServer(mux, SECRET)(signedRpcRequest(RPC_METHOD_INVOKE, {}, "exec_err", SECRET));
    assert.equal(res.status, 200);
    const payload = await rpcPayload(res);
    assert.equal(payload.error?.code, RPC_CODE_SERVER_RANGE);
    assert.match(payload.error?.message ?? "", /boom: deadline/);
    assert.equal(payload.result, undefined);
  });

  it("http.request 分发：envelope+body 复原真 Request → Fetch handler → httpResult（status/headers/body_b64）", async () => {
    const mux = new Mux();
    const seen: Array<{ url: string; method: string; contentType: string; body: string; source: string }> = [];
    mux.fetch(async (req, ctx) => {
      seen.push({
        url: req.url,
        method: req.method,
        contentType: req.headers.get("content-type") ?? "",
        body: new TextDecoder().decode(await req.arrayBuffer()),
        source: ctx.identity.source,
      });
      return new Response("created", { status: 201, headers: { "x-custom": "1" } });
    });
    const res = await newServer(mux, SECRET)(
      signedRpcRequest(
        RPC_METHOD_HTTP_REQUEST,
        {
          envelope: { method: "POST", path: "/cb", raw_query: "", headers: { "content-type": ["application/xml"] } },
          body_b64: b64("<xml>ok</xml>"),
        },
        "exec_h",
        SECRET,
        identityHeaders()
      )
    );
    assert.equal(res.status, 200);
    const payload = await rpcPayload(res);
    assert.equal(payload.error, undefined);
    const result = payload.result as { status: number; headers: Record<string, string>; body_b64: string };
    assert.equal(result.status, 201, "函数自己的 status 在 result.status");
    assert.equal(result.headers["x-custom"], "1");
    assert.equal(new TextDecoder().decode(Uint8Array.from(atob(result.body_b64), (c) => c.charCodeAt(0))), "created");
    // 触发器封套还原：url = http://trigger{path}?{raw_query}，headers 原样，body 解码。
    assert.equal(seen.length, 1);
    assert.equal(seen[0].url, "http://trigger/cb");
    assert.equal(seen[0].method, "POST");
    assert.equal(seen[0].contentType, "application/xml");
    assert.equal(seen[0].body, "<xml>ok</xml>");
    assert.equal(seen[0].source, "client", "fetch handler 亦收身份 ctx");
  });

  it("http.request：空响应 → headers/body_b64 按省略语义不出现（Go omitempty 同款）", async () => {
    const mux = new Mux();
    mux.fetch(() => new Response(null, { status: 204 }));
    const res = await newServer(mux, SECRET)(
      signedRpcRequest(RPC_METHOD_HTTP_REQUEST, { envelope: { method: "GET", path: "/" } }, "exec_e", SECRET)
    );
    const payload = await rpcPayload(res);
    assert.deepEqual(payload.result, { status: 204 });
  });

  it("http.request：缺 envelope → -32602；未注册 Fetch → -32601", async () => {
    const bare = new Mux();
    const resNoFetch = await newServer(bare, SECRET)(
      signedRpcRequest(RPC_METHOD_HTTP_REQUEST, { envelope: { method: "GET", path: "/" } }, "x", SECRET)
    );
    assert.equal(resNoFetch.status, 200);
    assert.equal((await rpcPayload(resNoFetch)).error?.code, RPC_CODE_METHOD_NOT_FOUND);

    const mux = new Mux();
    mux.fetch(() => new Response("ok"));
    const resNoEnvelope = await newServer(mux, SECRET)(
      signedRpcRequest(RPC_METHOD_HTTP_REQUEST, { body_b64: b64("x") }, "x", SECRET)
    );
    const noEnvelopePayload = await rpcPayload(resNoEnvelope);
    assert.equal(noEnvelopePayload.error?.code, RPC_CODE_INVALID_PARAMS);
    assert.match(noEnvelopePayload.error?.message ?? "", /missing envelope/);
  });

  it("invoke 未注册 handler → -32601（HTTP 仍 200）", async () => {
    const res = await newServer(new Mux(), SECRET)(signedRpcRequest(RPC_METHOD_INVOKE, {}, "x", SECRET));
    assert.equal(res.status, 200);
    const payload = await rpcPayload(res);
    assert.equal(payload.error?.code, RPC_CODE_METHOD_NOT_FOUND);
    assert.match(payload.error?.message ?? "", /no Invoke handler registered/);
  });

  it("未知 method → -32601；非 JSON-RPC 2.0 形态 → -32600；字段类型不匹配 → -32700（id 置空）", async () => {
    const server = newServer(new Mux(), SECRET);

    const unknown = await server(signedRpcRequest("wat.method", undefined, "x", SECRET));
    assert.equal((await rpcPayload(unknown)).error?.code, RPC_CODE_METHOD_NOT_FOUND);

    const notV2 = await server(signedRequest(JSON.stringify({ jsonrpc: "1.1", method: "invoke", id: "x" }), SECRET));
    assert.equal((await rpcPayload(notV2)).error?.code, -32600);

    const noMethod = await server(signedRequest(JSON.stringify({ jsonrpc: "2.0", id: "x" }), SECRET));
    assert.equal((await rpcPayload(noMethod)).error?.code, -32600);

    const badId = await server(signedRequest(JSON.stringify({ jsonrpc: "2.0", method: "invoke", id: 123 }), SECRET));
    assert.equal(badId.status, 200, "解析层错误也是应用层回包");
    const payload = await rpcPayload(badId);
    assert.equal(payload.id, "", "-32700 时 id 置空");
    assert.equal(payload.error?.code, RPC_CODE_PARSE_ERROR);
  });

  it("轮换窗口：头带 v1 新钥 / v2 旧钥签名，函数侧持旧钥可验（200）", async () => {
    const mux = new Mux();
    let gotData: unknown = "sentinel";
    mux.invoke((data) => {
      gotData = data;
      return "ok";
    });
    const body = JSON.stringify({ jsonrpc: "2.0", method: RPC_METHOD_INVOKE, id: "exec_r" });
    const ts = Math.floor(Date.now() / 1000);
    const req = new Request("http://function/", {
      method: "POST",
      headers: {
        "x-tw-signature": buildSignatureHeader(ts, [
          ["v1", computeSignature("whsec_new", ts, utf8(body))],
          ["v2", computeSignature("whsec_old", ts, utf8(body))],
        ]),
      },
      body: utf8(body),
    });
    const res = await newServer(mux, "whsec_old")(req);
    assert.equal(res.status, 200, "旧钥轮换窗口可验");
    assert.deepEqual((await rpcPayload(res)).result, "ok");
    assert.deepEqual(gotData, {});
  });
});

// ---- 事件轴（CloudEvents 1.0 structured mode）----

describe("newServer · 事件轴", () => {
  it("cron.tick / documents.* 以 CloudEvents 通知送达：204 = 送达（无应用层结果），同一 NewServer 与 RPC 轴共存", async () => {
    const mux = new Mux();
    let cronTick: { triggerId: string; scheduledFor: string } | null = null;
    let docChange: Record<string, unknown> | null = null;
    let cronSource = "";
    mux.cron(async (tick, ctx) => {
      cronTick = { triggerId: tick.triggerId, scheduledFor: tick.scheduledFor };
      cronSource = ctx.identity.source;
    });
    mux.event(async (ch) => {
      docChange = ch as unknown as Record<string, unknown>;
    });
    const server = newServer(mux, SECRET);

    const cronRes = await server(
      signedEventRequest("cron.tick", { trigger_id: "trg_1", scheduled_for: "2026-10-15T00:00:00Z" }, SECRET, "1.0", {
        "x-tw-source": "cron:trg_1",
      })
    );
    assert.equal(cronRes.status, 204);
    assert.deepEqual(cronTick, { triggerId: "trg_1", scheduledFor: "2026-10-15T00:00:00Z" });
    assert.equal(cronSource, "cron:trg_1");

    const eventRes = await server(
      signedEventRequest(
        "documents.created",
        {
          event: "documents.created",
          database_id: "db",
          collection_id: "col",
          document_id: "doc_1",
          version: 3,
          data: { id: "doc_1", data: { k: "v" } },
        },
        SECRET
      )
    );
    assert.equal(eventRes.status, 204);
    assert.deepEqual(docChange, {
      event: "documents.created",
      databaseId: "db",
      collectionId: "col",
      documentId: "doc_1",
      version: 3,
      document: { id: "doc_1", data: { k: "v" } },
    });
  });

  it("handler 抛错 → 400（= 投递失败信号，进重试梯度）", async () => {
    const failing = new Mux();
    failing.cron(async () => {
      throw new Error("boom");
    });
    const res = await newServer(failing, SECRET)(
      signedEventRequest("cron.tick", { trigger_id: "trg_1", scheduled_for: "2026-10-15T00:00:00Z" }, SECRET)
    );
    assert.equal(res.status, 400);
  });

  it("对应 handler 未注册 → 400", async () => {
    const bare = new Mux();
    const server = newServer(bare, SECRET);
    const cronRes = await server(
      signedEventRequest("cron.tick", { trigger_id: "trg_1", scheduled_for: "2026-10-15T00:00:00Z" }, SECRET)
    );
    assert.equal(cronRes.status, 400);
    const eventRes = await server(
      signedEventRequest("documents.created", { event: "documents.created", database_id: "db", collection_id: "col", document_id: "doc_1", version: 1 }, SECRET)
    );
    assert.equal(eventRes.status, 400);
  });

  it("载荷形状不匹配 / 非法 scheduled_for → 400", async () => {
    const mux = new Mux();
    mux.cron(async () => {});
    const server = newServer(mux, SECRET);
    const missing = await server(signedEventRequest("cron.tick", {}, SECRET));
    assert.equal(missing.status, 400);
    const badTime = await server(signedEventRequest("cron.tick", { trigger_id: "t", scheduled_for: "not-a-time" }, SECRET));
    assert.equal(badTime.status, 400);
  });

  it("非 1.0 specversion → 400", async () => {
    const mux = new Mux();
    mux.event(async () => {});
    const res = await newServer(mux, SECRET)(
      signedEventRequest("documents.created", { event: "documents.created", database_id: "db", collection_id: "col", document_id: "d", version: 1 }, SECRET, "0.3")
    );
    assert.equal(res.status, 400);
  });
});

// ---- 统一入口守卫（验签 / 体量 / 协议探测）----

describe("newServer · 统一入口守卫", () => {
  const mux = new Mux();
  const server: HttpHandler = newServer(mux, SECRET);

  it("坏签名 → 401（传输层拒绝）", async () => {
    const body = JSON.stringify({ jsonrpc: "2.0", method: "invoke", id: "x" });
    const ts = Math.floor(Date.now() / 1000);
    const req = new Request("http://function/", {
      method: "POST",
      headers: { "x-tw-signature": `t=${ts},v1=deadbeef` },
      body: utf8(body),
    });
    const res = await server(req);
    assert.equal(res.status, 401);
  });

  it("无签名 → 401（fail-closed）", async () => {
    const req = new Request("http://function/", {
      method: "POST",
      body: utf8(JSON.stringify({ jsonrpc: "2.0", method: "invoke", id: "x" })),
    });
    const res = await server(req);
    assert.equal(res.status, 401);
  });

  it("签名时间窗外 → 401", async () => {
    const body = JSON.stringify({ jsonrpc: "2.0", method: "invoke", id: "x" });
    const stale = Math.floor(Date.now() / 1000) - 6 * 60;
    const req = new Request("http://function/", {
      method: "POST",
      headers: { "x-tw-signature": buildSignatureHeader(stale, [["v1", computeSignature(SECRET, stale, utf8(body))]]) },
      body: utf8(body),
    });
    assert.equal((await server(req)).status, 401);
  });

  it("body 超 4MB → 413", async () => {
    const big = "a".repeat(MAX_BODY_BYTES + 1);
    const res = await server(signedRequest(big, SECRET));
    assert.equal(res.status, 413);
  });

  it("非法 JSON body → 400", async () => {
    const res = await server(signedRequest("{nope", SECRET));
    assert.equal(res.status, 400);
  });

  it("无 jsonrpc 也无 specversion → 400 unrecognized protocol", async () => {
    const res = await server(signedRequest(JSON.stringify({ method: "invoke" }), SECRET));
    assert.equal(res.status, 400, "传输层 400（无 RPC 信封可承载错误对象）");
  });

  it("空密钥集 fail-closed：newServer / verify 构造期即抛", () => {
    assert.throws(() => newServer(new Mux()), /requires at least one secret/);
  });
});
