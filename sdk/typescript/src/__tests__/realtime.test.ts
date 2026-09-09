import { describe, it } from "node:test";
import assert from "node:assert/strict";

import { Torchwood } from "../torchwood.js";
import type { RealtimeWebSocket } from "../client/realtime.js";

// MockWebSocket 模拟服务端一侧：记录客户端发出的帧，测试可手动触发
// open / 服务端帧 / close（如 token_expired）。
class MockWebSocket implements RealtimeWebSocket {
  static instances: MockWebSocket[] = [];

  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: { code: number; reason: string }) => void) | null = null;
  onerror: (() => void) | null = null;

  readonly sent: string[] = [];
  closed = false;

  constructor(readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.closed = true;
  }

  frames(): Record<string, unknown>[] {
    return this.sent.map((s) => JSON.parse(s) as Record<string, unknown>);
  }

  serverOpen(): void {
    this.onopen?.();
  }

  serverSend(frame: Record<string, unknown>): void {
    this.onmessage?.({ data: JSON.stringify(frame) });
  }

  serverClose(code: number, reason: string): void {
    this.onclose?.({ code, reason });
  }
}

function factory(): (url: string) => RealtimeWebSocket {
  MockWebSocket.instances = [];
  return (url) => new MockWebSocket(url);
}

const tick = () => new Promise((r) => setTimeout(r, 0));
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

describe("realtime", () => {
  it("hello 握手 + 订阅/事件/ping 帧序列", async () => {
    const client = Torchwood.withAccessToken("http://localhost:9080", "proj-1", "t1");
    const rt = client.realtime.connect({ webSocketFactory: factory() });
    await tick();
    const ws = MockWebSocket.instances[0];
    assert.equal(ws.url, "ws://localhost:9080/v1/realtime");

    ws.serverOpen();
    assert.deepEqual(ws.frames(), [
      { type: "hello", project_id: "proj-1", access_token: "t1" },
    ]);

    ws.serverSend({ type: "hello_ok", connection_id: "conn-1" });
    const events: unknown[] = [];
    const sub = rt.subscribe("databases.app.collections.posts", (ev) => events.push(ev));
    assert.deepEqual(ws.frames()[1], {
      type: "subscribe",
      id: "c1",
      channel: "databases.app.collections.posts",
    });

    ws.serverSend({ type: "subscribed", id: "c1", channel: "databases.app.collections.posts" });
    ws.serverSend({
      type: "event",
      channel: "databases.app.collections.posts",
      payload: { event_id: "e1", document_id: "d1" },
    });
    assert.deepEqual(events, [
      { channel: "databases.app.collections.posts", payload: { event_id: "e1", document_id: "d1" } },
    ]);

    // 服务端 30s ping → 客户端回 pong。
    ws.serverSend({ type: "ping" });
    assert.deepEqual(ws.frames().at(-1), { type: "pong" });

    // 退订：发送 unsubscribe 帧。
    sub.unsubscribe();
    assert.deepEqual(ws.frames().at(-1), {
      type: "unsubscribe",
      id: "c1",
      channel: "databases.app.collections.posts",
    });

    rt.close();
    assert.equal(rt.status, "closed");
  });

  it("Console cookie 场景：getAccessToken 省略时 hello 不带 access_token", async () => {
    const client = new Torchwood({ endpoint: "http://localhost:9080", projectId: "proj-1" });
    client.realtime.connect({ webSocketFactory: factory() });
    await tick();
    const ws = MockWebSocket.instances[0];
    ws.serverOpen();
    assert.deepEqual(ws.frames(), [{ type: "hello", project_id: "proj-1" }]);
  });

  it("token_expired 断线：刷新 token → 重新 hello → 重订全部频道（不补历史）", async () => {
    const client = Torchwood.withAccessToken("http://localhost:9080", "proj-1", "t1");
    const tokens = ["t2", "t3"];
    let refreshes = 0;
    const rt = client.realtime.connect({
      webSocketFactory: factory(),
      reconnectInitialDelayMs: 1,
      reconnectMaxDelayMs: 5,
      getAccessToken: () => (refreshes === 0 ? "t1" : tokens[Math.min(refreshes - 1, 1)]),
    });
    // 包一层计数（首次 connect 也会调 getAccessToken）。
    await tick();
    const ws1 = MockWebSocket.instances[0];
    ws1.serverOpen();
    ws1.serverSend({ type: "hello_ok", connection_id: "conn-1" });
    const events: unknown[] = [];
    rt.subscribe("databases.app.collections.posts", (ev) => events.push(ev));
    rt.subscribe("databases.app.collections.comments", () => {});
    assert.equal(ws1.frames().length, 3); // hello + 2 subscribe

    // JWT 到期：服务端以 StatusPolicyViolation + token_expired 关连接。
    refreshes += 1;
    ws1.serverClose(1008, "token_expired");
    assert.equal(rt.status, "reconnecting");
    await sleep(20);

    assert.equal(MockWebSocket.instances.length, 2);
    const ws2 = MockWebSocket.instances[1];
    ws2.serverOpen();
    // 重连 hello 带刷新后的 token。
    assert.deepEqual(ws2.frames(), [
      { type: "hello", project_id: "proj-1", access_token: "t2" },
    ]);
    ws2.serverSend({ type: "hello_ok", connection_id: "conn-2" });
    // 重订全部频道（复用原订阅 id），无历史补放。
    assert.deepEqual(ws2.frames().slice(1), [
      { type: "subscribe", id: "c1", channel: "databases.app.collections.posts" },
      { type: "subscribe", id: "c2", channel: "databases.app.collections.comments" },
    ]);
    assert.equal(rt.status, "connected");

    // 旧连接的迟到帧不再分发。
    ws1.serverSend({ type: "event", channel: "databases.app.collections.posts", payload: {} });
    ws2.serverSend({
      type: "event",
      channel: "databases.app.collections.posts",
      payload: { event_id: "e2" },
    });
    assert.deepEqual(events, [
      { channel: "databases.app.collections.posts", payload: { event_id: "e2" } },
    ]);

    rt.close();
  });

  it("指数退避：连续失败时延迟翻倍且封顶", async () => {
    const client = Torchwood.withAccessToken("http://localhost:9080", "proj-1", "t1");
    const rt = client.realtime.connect({
      webSocketFactory: factory(),
      reconnectInitialDelayMs: 5,
      reconnectMaxDelayMs: 20,
    });
    await tick();
    MockWebSocket.instances[0].serverOpen();
    MockWebSocket.instances[0].serverSend({ type: "hello_ok", connection_id: "c1" });

    // 第 1 次断线 → 5ms 后重连；第 2 次断线 → 10ms；第 3 次 → 20ms（封顶）。
    MockWebSocket.instances[0].serverClose(1006, "");
    await sleep(30);
    assert.equal(MockWebSocket.instances.length, 2);
    MockWebSocket.instances[1].serverClose(1006, "");
    await sleep(40);
    assert.equal(MockWebSocket.instances.length, 3);
    rt.close();

    // close 后不再重连。
    MockWebSocket.instances[2].serverClose(1006, "");
    await sleep(60);
    assert.equal(MockWebSocket.instances.length, 3);
    assert.equal(rt.status, "closed");
  });
});
