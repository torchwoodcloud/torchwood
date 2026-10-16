import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  Client,
  APIError,
  type ExecutionIdentity,
  type HandlerContext,
  type InvokeHandler,
  type CronHandler,
  type EventHandler,
  type FetchHandler,
  type PlatformDocument,
  type ClientOptions,
  type CronTick,
  type DocumentChange,
  type DocumentProjection,
} from "../index.js";

// ---- 导出面（运行时值） ----

describe("导出面", () => {
  it("值导出齐备：Client / APIError（v4 服务面值导出在 server.test 覆盖）", () => {
    assert.equal(typeof Client, "function");
    assert.equal(typeof APIError, "function");
    assert.equal(new APIError(404, "Not Found", "{}").name, "APIError");
  });
});

// ---- 类型形状（编译期断言；tsc -p 通过即验证） ----

// ExecutionIdentity 六件（与 Go identity.go 的 Identity 字段一一对应）；
// 缺字段应编译失败。
const identity: ExecutionIdentity = {
  executionToken: "twx_x",
  apiBaseUrl: "http://localhost:9080",
  executionId: "e1",
  source: "server",
  invokingUserId: "",
  projectId: "p1",
};
void identity;
// @ts-expect-error ExecutionIdentity 缺 projectId 不允许
const badIdentity: ExecutionIdentity = {
  executionToken: "twx_x",
  apiBaseUrl: "http://localhost:9080",
  executionId: "e1",
  source: "server",
  invokingUserId: "",
};
void badIdentity;

// HandlerContext：identity 六件 + per-request 预算信号。
const handlerCtx: HandlerContext = { identity, signal: new AbortController().signal };
void handlerCtx;

// 四类 handler 签名（v4 分发维度 = 协议轴；仅编译期形状断言，不执行）。
const invoke: InvokeHandler = async (data, ctx) => ({ pong: data, source: ctx.identity.source });
void invoke;
const cron: CronHandler = async (tick, ctx) => {
  void tick;
  void ctx.identity.executionId;
};
void cron;
const event: EventHandler = async (change) => {
  void change.documentId;
};
void event;
const fetchHandler: FetchHandler = async (req, ctx) => {
  void req;
  void ctx.identity.projectId;
  return new Response("ok");
};
void fetchHandler;

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
