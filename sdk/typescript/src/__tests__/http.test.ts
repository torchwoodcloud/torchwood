import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { HttpTransport, listQuery, DEFAULT_TIMEOUT_MS } from "../http.js";
import { TorchwoodError } from "../errors.js";

type FetchCall = { url: URL; headers: Record<string, string>; body: string };

function captureFetch(): { calls: FetchCall[]; fetch: typeof fetch } {
  const calls: FetchCall[] = [];
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers: Record<string, string> = {};
    if (init?.headers) new Headers(init.headers).forEach((v, k) => (headers[k] = v));
    calls.push({ url: new URL(String(input)), headers, body: String(init?.body ?? "") });
    return new Response(JSON.stringify({ ok: true }), { status: 200 });
  };
  return { calls, fetch: fetchImpl };
}

describe("HttpTransport.query", () => {
  it("数组参数展开为重复 query 参数（query 展开）", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      projectId: "default",
      apiKey: "k",
      fetch,
    });
    await http.request("GET", "/v1/server/users", {
      auth: "apiKey",
      query: listQuery({ queries: ['equal("a","1")', 'orderDesc("b")'], page_size: 10 }),
    });
    assert.equal(calls.length, 1);
    assert.deepEqual(calls[0].url.searchParams.getAll("queries"), [
      'equal("a","1")',
      'orderDesc("b")',
    ]);
    assert.equal(calls[0].url.searchParams.get("page_size"), "10");
    assert.equal(calls[0].headers["x-api-key"], "k");
  });

  it("undefined 查询参数被跳过", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({ endpoint: "http://localhost:9080", projectId: "p", fetch });
    await http.request("GET", "/v1/account/me", {
      query: { project_id: undefined as unknown as string },
    });
    assert.equal(calls[0].url.searchParams.has("project_id"), false);
  });

  it("请求体 JSON 序列化", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({ endpoint: "http://localhost:9080", projectId: "p", fetch });
    await http.request("PATCH", "/v1/account", { body: { name: "New" } });
    assert.equal(calls[0].headers["content-type"], "application/json");
    assert.deepEqual(JSON.parse(calls[0].body), { name: "New" });
  });
});

describe("HttpTransport.timeout", () => {
  /** 模拟规范 fetch 的超时中止行为：signal 触发后以 TimeoutError 拒绝。 */
  function hangUntilTimeoutFetch(captured: { signal?: AbortSignal }[]): typeof fetch {
    return (_input, init) =>
      new Promise<Response>((_resolve, reject) => {
        captured.push({ signal: init?.signal ?? undefined });
        init?.signal?.addEventListener("abort", () => {
          const err = new Error("The operation was aborted due to timeout");
          err.name = "TimeoutError";
          reject(err);
        });
      });
  }

  /** 立即成功返回、只捕获本次请求收到的 signal（信号注入断言用）。 */
  function captureSignalFetch(captured: { signal?: AbortSignal }[]): typeof fetch {
    return (_input, init) => {
      captured.push({ signal: init?.signal ?? undefined });
      return Promise.resolve(new Response(JSON.stringify({ ok: true }), { status: 200 }));
    };
  }

  it("缺省配置注入超时信号（timeoutMs 缺省 = 30000）", async () => {
    const captured: { signal?: AbortSignal }[] = [];
    const http = new HttpTransport({ endpoint: "http://localhost:9080", fetch: captureSignalFetch(captured) });
    await http.request("GET", "/v1/account/me");
    assert.equal(captured.length, 1);
    assert.ok(captured[0].signal, "缺省 timeoutMs 必须传超时 signal");
  });

  it("timeoutMs: 0 显式禁用（fetch 不带 signal）", async () => {
    const captured: { signal?: AbortSignal }[] = [];
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      timeoutMs: 0,
      fetch: captureSignalFetch(captured),
    });
    await http.request("GET", "/v1/account/me");
    assert.equal(captured[0].signal, undefined);
  });

  it("超时抛出可辨识的 TorchwoodError（code=timeout，消息含 timed out）", async () => {
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      timeoutMs: 20,
      fetch: hangUntilTimeoutFetch([]),
    });
    await assert.rejects(
      () => http.request("GET", "/v1/server/users"),
      (err: unknown) => {
        assert.ok(err instanceof TorchwoodError);
        assert.equal((err as TorchwoodError).code, "timeout");
        assert.equal((err as TorchwoodError).status, 0);
        assert.ok((err as TorchwoodError).message.includes("timed out after 20ms"));
        return true;
      },
    );
  });

  it("DEFAULT_TIMEOUT_MS 为 30000（对齐 Go SDK 30s 兜底）", () => {
    assert.equal(DEFAULT_TIMEOUT_MS, 30000);
  });
});

describe("HttpTransport.requestForm 空体", () => {
  it("204 空响应体返回 undefined（不抛 JSON 解析错）", async () => {
    const fetch204: typeof fetch = async () => new Response(null, { status: 204 });
    const http = new HttpTransport({ endpoint: "http://localhost:9080", projectId: "p", apiKey: "k", fetch: fetch204 });
    const out = await http.requestForm("POST", "/v1/server/functions/f/deploy", new FormData());
    assert.equal(out, undefined);
  });

  it("200 但空文本响应体返回 undefined", async () => {
    const fetchEmpty: typeof fetch = async () => new Response("", { status: 200 });
    const http = new HttpTransport({ endpoint: "http://localhost:9080", projectId: "p", apiKey: "k", fetch: fetchEmpty });
    const out = await http.requestForm("POST", "/v1/server/functions/f/deploy", new FormData());
    assert.equal(out, undefined);
  });
});
