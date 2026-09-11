import { describe, it, afterEach } from "node:test";
import assert from "node:assert/strict";

import { AnalyticsEventBuffer } from "../client/analytics.js";
import { TorchwoodError } from "../errors.js";
import type { ClientAnalyticsEvent, IngestEventsResult } from "../client/analytics.js";

// ---- AnalyticsEventBuffer 批量缓冲器（执行计划 PR6 验收）----
// 覆盖：flush 触发（条数/时间/页面隐藏）、失败静默、有界退避重试的
// 不丢批语义（at-least-once，D14）。

type SendCall = ClientAnalyticsEvent[];

/** drain 让出一轮宏任务，等待微任务级联（拒绝传播/退避续发）完成。 */
const drain = (): Promise<void> => new Promise((resolve) => setTimeout(resolve, 0));

/** 假时钟：记录 setTimeout 调度，测试手动按序触发（clearTimeout 同步生效）。 */
function fakeTimers() {
  interface Entry {
    fn: () => void;
    ms: number;
    cleared: boolean;
  }
  const scheduled: Entry[] = [];
  return {
    scheduled,
    timers: {
      setTimeout(fn: () => void, ms: number): unknown {
        const entry: Entry = { fn, ms, cleared: false };
        scheduled.push(entry);
        return entry;
      },
      clearTimeout(handle: unknown): void {
        (handle as Entry).cleared = true;
      },
    },
    /** run 触发全部未清除且 ms <= until 的回调（按调度顺序）。 */
    run(until = Number.MAX_SAFE_INTEGER): void {
      for (const e of [...scheduled]) {
        if (!e.cleared && e.ms <= until) {
          e.cleared = true;
          e.fn();
        }
      }
    },
    /** delays 返回全部已调度回调的等待毫秒序列（退避节奏断言用）。 */
    delays(): number[] {
      return scheduled.map((e) => e.ms);
    },
  };
}

/** 记录型 send：每次调用入列并返回可手动放行的 Deferred。 */
function deferredSend() {
  const calls: SendCall[] = [];
  const pending: {
    resolve: (res: IngestEventsResult) => void;
    reject: (err: unknown) => void;
  }[] = [];
  const send = (events: ClientAnalyticsEvent[]): Promise<IngestEventsResult> => {
    calls.push(events);
    return new Promise((resolve, reject) => pending.push({ resolve, reject }));
  };
  return {
    send,
    calls,
    pending,
    /** resolveAll 放行全部在途请求（部分接收响应：accepted/skipped 不抛错）。 */
    resolveAll(): void {
      while (pending.length > 0) {
        pending.shift()!.resolve({ accepted: 1, skipped: 0 });
      }
    },
    /** rejectAll 以给定错误拒绝全部在途请求。 */
    rejectAll(err: unknown): void {
      while (pending.length > 0) {
        pending.shift()!.reject(err);
      }
    },
  };
}

function ev(name: string): ClientAnalyticsEvent {
  return { name };
}

afterEach(() => {
  delete (globalThis as { document?: unknown }).document;
  delete (globalThis as { window?: unknown }).window;
});

describe("AnalyticsEventBuffer flush 触发", () => {
  it("size 阈值：缓冲达到 maxBatchSize 立即 flush 整批", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, { maxBatchSize: 5, timers: t.timers });

    for (let i = 0; i < 4; i++) buf.track(ev(`e${i}`));
    assert.equal(d.calls.length, 0, "未达 size 阈值不发送");
    assert.equal(buf.pending, 4);

    buf.track(ev("e4"));
    d.resolveAll();
    await buf.flush(); // 共用在途 Promise；此时整批应已发出

    assert.equal(d.calls.length, 1);
    assert.deepEqual(d.calls[0].map((e) => e.name), ["e0", "e1", "e2", "e3", "e4"]);
    assert.equal(buf.pending, 0);
    assert.equal(t.scheduled.filter((e) => !e.cleared).length, 0, "flush 后定时器已清");
    buf.dispose();
  });

  it("time 阈值：事件等待满 flushIntervalMs 到点 flush", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, {
      maxBatchSize: 100,
      flushIntervalMs: 10_000,
      timers: t.timers,
    });

    buf.track(ev("a"));
    buf.track(ev("b"));
    assert.equal(d.calls.length, 0);
    assert.equal(t.scheduled.length, 1, "首个事件入缓冲即排定时器");
    assert.equal(t.scheduled[0].ms, 10_000);

    t.run();
    d.resolveAll();
    await buf.flush();

    assert.equal(d.calls.length, 1);
    assert.deepEqual(d.calls[0].map((e) => e.name), ["a", "b"]);
    buf.dispose();
  });

  it("页面隐藏（visibilitychange hidden / beforeunload）尽力 flush", async () => {
    const listeners: Record<string, (() => void)[]> = {};
    const makeTarget = () => ({
      addEventListener: (type: string, fn: () => void) => {
        (listeners[type] ??= []).push(fn);
      },
      removeEventListener: (type: string, fn: () => void) => {
        listeners[type] = (listeners[type] ?? []).filter((f) => f !== fn);
      },
    });
    const doc = { ...makeTarget(), visibilityState: "visible", defaultView: undefined };
    (globalThis as { document?: unknown }).document = doc;
    (globalThis as { window?: unknown }).window = makeTarget();

    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, { maxBatchSize: 100, timers: t.timers });

    buf.track(ev("x"));
    assert.equal(d.calls.length, 0);

    // visibilitychange：visible 不触发，hidden 触发。
    for (const fn of listeners["visibilitychange"] ?? []) fn();
    assert.equal(d.calls.length, 0, "visibilityState=visible 不触发");
    doc.visibilityState = "hidden";
    for (const fn of listeners["visibilitychange"] ?? []) fn();
    assert.equal(d.calls.length, 1, "hidden 尽力 flush");
    d.resolveAll();
    await buf.flush();
    assert.deepEqual(d.calls[0].map((e) => e.name), ["x"]);

    // beforeunload 触发 flush；dispose 注销钩子后不再触发。
    buf.track(ev("y"));
    for (const fn of listeners["beforeunload"] ?? []) fn();
    d.resolveAll();
    await buf.flush();
    assert.equal(d.calls.length, 2);

    buf.dispose();
    buf.track(ev("z"));
    for (const fn of listeners["beforeunload"] ?? []) fn();
    for (const fn of listeners["visibilitychange"] ?? []) fn();
    assert.equal(d.calls.length, 2, "dispose 后钩子已注销");
    assert.equal(buf.pending, 1, "dispose 后缓冲保留");
    assert.equal(t.scheduled.filter((e) => !e.cleared).length, 0, "dispose 清定时器");
  });
});

describe("AnalyticsEventBuffer 失败静默与重试语义", () => {
  it("5xx 失败按指数退避重试，重试期间不丢批（at-least-once）；恢复后整批送达", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, {
      maxBatchSize: 3,
      flushIntervalMs: 10_000,
      maxRetries: 2,
      retryBackoffMs: 500,
      timers: t.timers,
    });

    buf.track(ev("a"));
    buf.track(ev("b"));
    buf.track(ev("c")); // 达 size 阈值 → 首次请求同步发出
    assert.equal(d.calls.length, 1);

    // 首次 500 → 退避 500ms → 重试 1 → 再 500 折算 1000ms → 重试 2 → 成功。
    d.rejectAll(new TorchwoodError("boom", 500));
    await drain();
    t.run(500);
    await drain();
    assert.equal(d.calls.length, 2, "同批重试 1");
    d.rejectAll(new TorchwoodError("boom", 500));
    await drain();
    t.run(1000);
    await drain();
    assert.equal(d.calls.length, 3, "同批重试 2");
    d.resolveAll();
    await buf.flush();

    assert.deepEqual(d.calls[0].map((e) => e.name), ["a", "b", "c"]);
    assert.deepEqual(d.calls[1].map((e) => e.name), ["a", "b", "c"], "重试复用同批（不丢批）");
    assert.deepEqual(d.calls[2].map((e) => e.name), ["a", "b", "c"]);
    // 退避节奏 500 → 1000（滤掉 time 阈值的 10s 定时）。
    assert.deepEqual(t.delays().filter((ms) => ms > 0 && ms !== 10_000), [500, 1000]);
    assert.equal(buf.pending, 0);
    buf.dispose();
  });

  it("重试耗尽后该批静默丢弃：flush 不 reject、缓冲清空", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, {
      maxBatchSize: 2,
      maxRetries: 1,
      retryBackoffMs: 100,
      timers: t.timers,
    });

    buf.track(ev("a"));
    buf.track(ev("b"));
    assert.equal(d.calls.length, 1);

    d.rejectAll(new TorchwoodError("down", 503));
    await drain();
    t.run();
    await drain();
    assert.equal(d.calls.length, 2, "1 次首发 + 1 次重试");
    d.rejectAll(new TorchwoodError("down", 503));
    await drain();
    // 耗尽：flush promise 应正常 resolve（失败静默，不 reject）。
    await buf.flush();

    assert.equal(buf.pending, 0, "耗尽后该批静默丢弃");
    buf.dispose();
  });

  it("track/flush 永不抛错：网络异常耗尽重试后静默丢弃", async () => {
    const t = fakeTimers();
    const send = (): Promise<IngestEventsResult> => Promise.reject(new TypeError("fetch failed"));
    const buf = new AnalyticsEventBuffer(send, {
      maxBatchSize: 1,
      maxRetries: 2,
      retryBackoffMs: 10,
      timers: t.timers,
    });

    buf.track(ev("a")); // size 阈值触发 flush（异步）
    await drain();
    t.run();
    await drain();
    t.run();
    await drain();
    await buf.flush(); // 不 reject 即通过

    assert.equal(buf.pending, 0);
    buf.dispose();
  });

  it("4xx 语义性失败不重试：直接静默丢弃", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, {
      maxBatchSize: 1,
      maxRetries: 3,
      retryBackoffMs: 100,
      timers: t.timers,
    });

    buf.track(ev("bad"));
    assert.equal(d.calls.length, 1);
    d.rejectAll(new TorchwoodError("invalid", 400));
    await drain();
    await buf.flush();

    assert.equal(d.calls.length, 1, "4xx 不重试");
    assert.equal(buf.pending, 0);
    assert.equal(t.scheduled.filter((e) => e.ms > 0 && !e.cleared).length, 0, "无遗留退避定时");
    buf.dispose();
  });

  it("429 限流可重试；accepted/skipped 响应不抛错", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, {
      maxBatchSize: 1,
      maxRetries: 1,
      retryBackoffMs: 100,
      timers: t.timers,
    });

    buf.track(ev("a"));
    d.rejectAll(new TorchwoodError("rate limited", 429));
    await drain();
    t.run();
    await drain();
    assert.equal(d.calls.length, 2, "429 触发一次重试");
    d.resolveAll(); // 部分接收响应（accepted/skipped）
    await buf.flush();

    assert.equal(buf.pending, 0);
    buf.dispose();
  });
});

describe("AnalyticsEventBuffer 批量分片", () => {
  it("超设 maxBatchSize 按服务端单批 100 上限分批", async () => {
    const t = fakeTimers();
    const d = deferredSend();
    const buf = new AnalyticsEventBuffer(d.send, { maxBatchSize: 500, timers: t.timers });

    for (let i = 0; i < 150; i++) buf.track(ev(`e${i}`));
    assert.equal(d.calls.length, 1, "首批 100 条同步发出");
    d.resolveAll();
    await drain(); // 循环继续 → 第二批发出
    assert.equal(d.calls.length, 2);
    assert.equal(d.calls[0].length, 100);
    d.resolveAll();
    await buf.flush();

    assert.equal(d.calls[1].length, 50);
    assert.equal(buf.pending, 0);
    buf.dispose();
  });
});
