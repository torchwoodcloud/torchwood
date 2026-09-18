import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  defineMain,
  Client,
  APIError,
  type Ctx,
  type MainHandler,
  type MainEntrypoint,
  type PlatformDocument,
  type ClientOptions,
  type CronTick,
  type DocumentChange,
  type DocumentProjection,
} from "../index.js";

// ---- 导出面（运行时值） ----

describe("导出面", () => {
  it("值导出齐备：defineMain / Client / APIError", () => {
    assert.equal(typeof defineMain, "function");
    assert.equal(typeof Client, "function");
    assert.equal(typeof APIError, "function");
    assert.equal(new APIError(404, "Not Found", "{}").name, "APIError");
  });

  it("defineMain 收非函数 handler → 同步抛错（运行时校验兜底）", () => {
    const anyFn = 42 as unknown as MainHandler;
    assert.throws(() => defineMain(anyFn), /handler must be a function/);
  });
});

// ---- 类型形状（编译期断言；tsc -p 通过即验证） ----

const ctx: Ctx = {
  executionToken: "twx_x",
  apiBaseUrl: "http://localhost:9080",
  executionId: "e1",
  source: "server",
  invokingUserId: "",
  projectId: "p1",
};

// Ctx 六件与 runner.js 注入逐字段同源；缺字段应编译失败。
// @ts-expect-error Ctx 缺 projectId 不允许
const badCtx: Ctx = {
  executionToken: "twx_x",
  apiBaseUrl: "http://localhost:9080",
  executionId: "e1",
  source: "server",
  invokingUserId: "",
};
void badCtx;

// defineMain 签名：MainHandler 可赋给 MainEntrypoint 的入参面（协变一致）。
const handler: MainHandler = async (data, c) => {
  void data;
  return c.source;
};
const entry: MainEntrypoint = defineMain(handler);
void entry;

// CronTick：wire {type:"cron",trigger_id,scheduled_for} 的 camelCase 视图。
const tick: CronTick = { triggerId: "cron-1", scheduledFor: "2026-09-18T00:00:00Z" };
void tick;
// @ts-expect-error CronTick 缺 scheduledFor 不允许
const badTick: CronTick = { triggerId: "cron-1" };
void badTick;

// DocumentChange：EventInvocationData 投影的 typed 视图；document 可选
// （delete/截断缺省）。
const change: DocumentChange = {
  event: "databases.documents.create",
  databaseId: "app",
  collectionId: "logs",
  documentId: "doc-1",
  version: 7,
  document: { id: "doc-1", data: { k: "v" } },
};
const deletedChange: DocumentChange = {
  event: "databases.documents.delete",
  databaseId: "app",
  collectionId: "logs",
  documentId: "doc-1",
  version: 8,
};
void change;
void deletedChange;
// @ts-expect-error DocumentChange 缺 documentId 不允许
const badChange: DocumentChange = { event: "e", databaseId: "d", collectionId: "c", version: 1 };
void badChange;

// DocumentProjection：REST Document 的 {id,data} 子集。
const proj: DocumentProjection = { id: "doc-1", data: { a: 1 } };
const bareProj: DocumentProjection = { id: "doc-1" };
void proj;
void bareProj;

// PlatformDocument：getDocument 返回形状（version string|number 双形态）。
const rec: PlatformDocument = { id: "d1", data: { a: 1 }, version: "3" };
const recNum: PlatformDocument = { id: "d1", data: null, version: 3 };
void rec;
void recNum;
// @ts-expect-error version 缺失不允许
const badRec: PlatformDocument = { id: "d1", data: {}, };
void badRec;

// ClientOptions：fetch 覆写通道。
const opts: ClientOptions = { fetch: globalThis.fetch };
void opts;

describe("事件类型形状（运行时冒烟）", () => {
  it("DocumentChange 定位三件 + document 投影可回读 Client.getDocument", () => {
    // 语义链：change → client.getDocument(change.databaseId, …, change.documentId)。
    const c: DocumentChange = {
      event: "databases.documents.update",
      databaseId: "app",
      collectionId: "logs",
      documentId: "doc-9",
      version: 12,
    };
    assert.equal(c.databaseId, "app");
    assert.equal(c.collectionId, "logs");
    assert.equal(c.documentId, "doc-9");
    assert.equal(c.document, undefined);
    const t: CronTick = { triggerId: "t", scheduledFor: "2026-09-18T01:02:03Z" };
    assert.match(t.scheduledFor, /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
  });
});
