import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { Client, APIError } from "../client.js";
import type { Ctx } from "../identity.js";

const ctx: Ctx = {
  executionToken: "twx_token_abc",
  apiBaseUrl: "http://gateway:9080/",
  executionId: "exec-1",
  source: "event:t1",
  invokingUserId: "",
  projectId: "proj-1",
};

type FetchCall = { url: string; method: string; headers: Record<string, string>; body: string | undefined };

function captureFetch(status: number, payload: unknown, contentType = "application/json") {
  const calls: FetchCall[] = [];
  const fetchImpl = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers: Record<string, string> = {};
    if (init?.headers) new Headers(init.headers).forEach((v, k) => (headers[k] = v));
    calls.push({
      url: String(input),
      method: init?.method ?? "GET",
      headers,
      body: typeof init?.body === "string" ? init.body : undefined,
    });
    const body = payload === undefined ? "" : JSON.stringify(payload);
    return new Response(body, { status, headers: { "Content-Type": contentType } });
  }) as typeof fetch;
  return { calls, fetchImpl };
}

describe("Client.createDocument", () => {
  it("POST 文档路径 + Bearer 鉴权 + JSON body {data}，返回 {id}", async () => {
    const { calls, fetchImpl } = captureFetch(200, { id: "doc-1", data: { k: "v" }, version: "1" });
    const client = new Client(ctx, { fetch: fetchImpl });
    const doc = await client.createDocument("app", "logs", { k: "v" });

    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, "http://gateway:9080/v1/server/databases/app/collections/logs/documents");
    assert.equal(calls[0].method, "POST");
    assert.equal(calls[0].headers["authorization"], "Bearer twx_token_abc");
    assert.equal(calls[0].headers["content-type"], "application/json");
    assert.deepEqual(JSON.parse(calls[0].body ?? "{}"), { data: { k: "v" } });
    assert.deepEqual(doc, { id: "doc-1", data: { k: "v" }, version: "1" });
  });

  it("apiBaseUrl 尾斜杠归一 + 路径段 encodeURIComponent", async () => {
    const { calls, fetchImpl } = captureFetch(200, { id: "d", data: {}, version: 2 });
    const client = new Client({ ...ctx, apiBaseUrl: "https://api.example.com///" }, { fetch: fetchImpl });
    await client.createDocument("my db", "col lect", { a: 1 });
    assert.equal(
      calls[0].url,
      "https://api.example.com/v1/server/databases/my%20db/collections/col%20lect/documents"
    );
  });

  it("data 为 undefined → 清晰错误且不发请求", async () => {
    const { calls, fetchImpl } = captureFetch(200, {});
    const client = new Client(ctx, { fetch: fetchImpl });
    await assert.rejects(
      () => client.createDocument("app", "logs", undefined),
      /createDocument requires data/
    );
    assert.equal(calls.length, 0);
  });
});

describe("Client.getDocument", () => {
  it("GET 文档路径（含 documentId）+ Bearer 鉴权，返回 id/data/version", async () => {
    const { calls, fetchImpl } = captureFetch(200, {
      id: "doc-9",
      data: { n: 1 },
      version: "42",
      permissions: ["perm:read"],
      created_at: "2026-09-18T00:00:00Z",
    });
    const client = new Client(ctx, { fetch: fetchImpl });
    const doc = await client.getDocument("app", "logs", "doc-9");

    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, "http://gateway:9080/v1/server/databases/app/collections/logs/documents/doc-9");
    assert.equal(calls[0].method, "GET");
    assert.equal(calls[0].headers["authorization"], "Bearer twx_token_abc");
    assert.equal(calls[0].body, undefined);
    assert.equal(doc.id, "doc-9");
    assert.deepEqual(doc.data, { n: 1 });
    assert.equal(doc.version, "42");
  });
});

describe("Client 错误映射", () => {
  it("4xx → APIError（status + error.message 提取 + body 原文）", async () => {
    const { fetchImpl } = captureFetch(403, { error: { code: "PERMISSION_DENIED", message: "scope databases:write missing" } });
    const client = new Client(ctx, { fetch: fetchImpl });
    await assert.rejects(
      () => client.createDocument("app", "logs", {}),
      (e: unknown) => {
        assert.ok(e instanceof APIError, `expected APIError, got ${String(e)}`);
        const err = e as APIError;
        assert.equal(err.status, 403);
        assert.equal(err.statusText, "");
        assert.match(err.message, /403/);
        assert.match(err.message, /scope databases:write missing/);
        assert.match(err.body, /PERMISSION_DENIED/);
        return true;
      }
    );
  });

  it("5xx 非 JSON body → 回退 statusText/body 片段", async () => {
    const fetchImpl = (async () => {
      return new Response("upstream exploded", { status: 502 });
    }) as typeof fetch;
    const client = new Client(ctx, { fetch: fetchImpl });
    await assert.rejects(
      () => client.getDocument("app", "logs", "d1"),
      (e: unknown) => {
        assert.ok(e instanceof APIError, `expected APIError, got ${String(e)}`);
        const err = e as APIError;
        assert.equal(err.status, 502);
        assert.match(err.message, /upstream exploded/);
        return true;
      }
    );
  });
});

describe("Client fail-closed", () => {
  it("executionToken 为空 → 不发请求直接抛错", async () => {
    const { calls, fetchImpl } = captureFetch(200, {});
    const client = new Client({ ...ctx, executionToken: "" }, { fetch: fetchImpl });
    await assert.rejects(() => client.getDocument("app", "logs", "d"), /executionToken is empty/);
    assert.equal(calls.length, 0);
  });

  it("apiBaseUrl 为空 → 不发请求直接抛错", async () => {
    const { calls, fetchImpl } = captureFetch(200, {});
    const client = new Client({ ...ctx, apiBaseUrl: "" }, { fetch: fetchImpl });
    await assert.rejects(() => client.createDocument("app", "logs", {}), /apiBaseUrl is empty/);
    assert.equal(calls.length, 0);
  });

  it("ctx 形状非法 → 构造期即抛错", () => {
    assert.throws(() => new Client({ ...ctx, projectId: 1 } as unknown as Ctx), /ctx\.projectId must be a string/);
    assert.throws(() => new Client(null as unknown as Ctx), /ctx must be an object/);
  });
});
