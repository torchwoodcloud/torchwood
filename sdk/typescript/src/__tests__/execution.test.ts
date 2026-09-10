import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";

import { HttpTransport } from "../http.js";
import { Torchwood, TorchwoodError } from "../torchwood.js";
import { FunctionsService } from "../server/functions.js";

type FetchCall = { url: URL; headers: Record<string, string> };

function captureFetch(): { calls: FetchCall[]; fetch: typeof fetch } {
  const calls: FetchCall[] = [];
  const fetchImpl = async (input: RequestInfo | URL, init?: RequestInit) => {
    const headers: Record<string, string> = {};
    if (init?.headers) new Headers(init.headers).forEach((v, k) => (headers[k] = v));
    calls.push({ url: new URL(String(input)), headers });
    return new Response(JSON.stringify({ ok: true }), { status: 200 });
  };
  return { calls, fetch: fetchImpl };
}

// 函数执行身份（docs/design/functions-v3.md §5.1）：HttpTransport 的
// auth:"execution" 模式 + fromExecution 工厂（TW_EXECUTION_TOKEN 回退）。
describe("HttpTransport execution auth", () => {
  it("auth:\"execution\" 发送 Bearer <executionToken> 且不带 X-Api-Key", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      executionToken: "exec-tok",
      fetch,
    });
    await http.request("GET", "/v1/server/functions", { auth: "execution" });
    assert.equal(calls[0].headers.authorization, "Bearer exec-tok");
    assert.equal(calls[0].headers["x-api-key"], undefined);
    assert.equal(calls[0].headers["x-torchwood-project"], undefined);
  });

  it("server 服务类的 auth:\"apiKey\" 在 execution transport 下切换为执行 Bearer", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      executionToken: "exec-tok",
      fetch,
    });
    const functions = new FunctionsService(http);
    await functions.list();
    assert.equal(calls[0].headers.authorization, "Bearer exec-tok");
    assert.equal(calls[0].headers["x-api-key"], undefined);
  });

  it("execution transport 无 apiKey 时普通 apiKey 模式回退执行凭证（服务类复用）", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({
      endpoint: "http://localhost:9080",
      executionToken: "exec-tok",
      fetch,
    });
    await http.request("GET", "/v1/server/users", { auth: "apiKey" });
    assert.equal(calls[0].headers.authorization, "Bearer exec-tok");
  });

  it("execution 模式缺凭证抛错（fail-closed）", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({ endpoint: "http://localhost:9080", fetch });
    await assert.rejects(
      () => http.request("GET", "/v1/server/functions", { auth: "execution" }),
      TorchwoodError
    );
    assert.equal(calls.length, 0);
  });

  it("setExecutionToken 后续注入生效", async () => {
    const { calls, fetch } = captureFetch();
    const http = new HttpTransport({ endpoint: "http://localhost:9080", fetch });
    http.setExecutionToken("late-tok");
    assert.equal(http.getExecutionToken(), "late-tok");
    await http.request("GET", "/v1/server/functions", { auth: "execution" });
    assert.equal(calls[0].headers.authorization, "Bearer late-tok");
  });
});

describe("Torchwood.fromExecution", () => {
  const ENV_KEY = "TW_EXECUTION_TOKEN";
  let saved: string | undefined;

  beforeEach(() => {
    saved = process.env[ENV_KEY];
    delete process.env[ENV_KEY];
  });

  afterEach(() => {
    if (saved === undefined) {
      delete process.env[ENV_KEY];
    } else {
      process.env[ENV_KEY] = saved;
    }
  });

  it("显式参数优先，返回的 client server 服务类可用", () => {
    const client = Torchwood.fromExecution("http://localhost:9080", {
      executionToken: "explicit-tok",
    });
    assert.equal(client.getExecutionToken(), "explicit-tok");
    assert.equal(client.getProjectId(), "");
    // server 服务类全量可用（execution principal 走 Server API）。
    assert.ok(client.server.functions);
    assert.ok(client.server.databases);
    assert.ok(client.server.assets);
    assert.ok(client.server.users);
  });

  it("无显式参数时回退 TW_EXECUTION_TOKEN env", () => {
    process.env[ENV_KEY] = "env-tok";
    const client = Torchwood.fromExecution("http://localhost:9080");
    assert.equal(client.getExecutionToken(), "env-tok");
  });

  it("显式参数覆盖 env", () => {
    process.env[ENV_KEY] = "env-tok";
    const client = Torchwood.fromExecution("http://localhost:9080", {
      executionToken: "explicit-tok",
    });
    assert.equal(client.getExecutionToken(), "explicit-tok");
  });

  it("两者皆缺抛错（fail-closed）", () => {
    assert.throws(() => Torchwood.fromExecution("http://localhost:9080"), TorchwoodError);
  });

  it("fromExecution client 发送执行 Bearer 头（端到端经 mock fetch）", async () => {
    const { calls, fetch } = captureFetch();
    const client = Torchwood.fromExecution("http://localhost:9080", {
      executionToken: "wire-tok",
      fetch,
    });
    await client.server.functions.list();
    assert.equal(calls[0].headers.authorization, "Bearer wire-tok");
    assert.equal(calls[0].headers["x-api-key"], undefined);
  });
});
