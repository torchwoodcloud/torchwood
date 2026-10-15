/**
 * 触发器载荷的 typed 视图（functions 五期 5c：先随 SDK 提供类型面，typed
 * 入口变体在后续阶段接入——形状与 Go SDK `sdk/go/functions` 的 CronTick /
 * DocumentChange 逐字段对称）。
 *
 * 字段为 camelCase 的 typed 视图：平台 wire 形状（TW_DATA JSON）是
 * snake_case，解析在 SDK 侧完成，函数代码不接触 wire 字段名。
 */

/**
 * CronTick 是 cron 触发的一次心跳（wire：TW_DATA
 * `{type:"cron", trigger_id, scheduled_for}` 的投影；`scheduled_for` 为计划
 * 时刻 RFC3339 字符串，函数内可作幂等键）。
 */
export interface CronTick {
  /** 触发本次执行的 cron 触发器 ID。 */
  triggerId: string;
  /** 本次计划时刻（RFC3339；补跑时为原到期值，非实际投递时刻）。 */
  scheduledFor: string;
}

/**
 * DocumentProjection 是事件载荷内嵌的文档投影（REST Document 的
 * {id, data} 子集；投影只带「ID + 摘要」，全量靠 Client.getDocument 回读）。
 */
export interface DocumentProjection {
  /** 文档 ID（即 DocumentChange.documentId）。 */
  id: string;
  /** 文档数据（delete 事件与超限截断时缺省）。 */
  data?: Record<string, unknown>;
}

/**
 * DocumentChange 是数据库事件触发的一次变更（wire：平台 EventInvocationData
 * 投影 `internal/domain/functions/eventdata.go` 的 typed 视图）。
 */
export interface DocumentChange {
  /** 事件名（如 databases.documents.create | update | delete）。 */
  event: string;
  /** 变更文档定位三件（回读全量的键）。 */
  databaseId: string;
  collectionId: string;
  documentId: string;
  /** 文档 OCC 版本（事件时刻）。 */
  version: number;
  /**
   * 文档投影（wire `data` 字段；delete 事件与超限截断时缺省——按
   * documentId 经 Client.getDocument 回读判断）。
   */
  document?: DocumentProjection;
}

// ---- wire 解析（v4 事件轴；对标 Go events.go 的 parseCronTick /
// parseDocumentChange：snake_case wire → camelCase typed 视图，形状不匹配
// 抛错 → 服务面映射 400 投递失败）。

/**
 * parseCronTick 解析 cron 通知的 data（wire：TW_DATA
 * {trigger_id, scheduled_for}）；缺 trigger_id / 非法 RFC3339 抛错。
 */
export function parseCronTick(data: unknown): CronTick {
  const wire = wireObject(data, "cron");
  const triggerId = wireString(wire, "trigger_id", "cron");
  if (triggerId === "") throw new Error("cron TW_DATA: missing trigger_id");
  const scheduledFor = wireString(wire, "scheduled_for", "cron");
  if (!isRFC3339(scheduledFor)) {
    throw new Error(`cron TW_DATA: invalid scheduled_for ${JSON.stringify(scheduledFor)}`);
  }
  return { triggerId, scheduledFor };
}

/**
 * parseDocumentChange 解析事件通知的 data（wire：平台 EventInvocationData
 * 投影——SDK 只取 typed 视图所需字段；data 缺省（delete/截断）→ document
 * 缺省）。
 */
export function parseDocumentChange(data: unknown): DocumentChange {
  const wire = wireObject(data, "event");
  const event = wireString(wire, "event", "event");
  const databaseId = wireString(wire, "database_id", "event");
  const collectionId = wireString(wire, "collection_id", "event");
  const documentId = wireString(wire, "document_id", "event");
  const version = wireInteger(wire, "version", "event");
  if (event === "") throw new Error("event TW_DATA: missing event");
  if (databaseId === "") throw new Error("event TW_DATA: missing database_id");
  if (collectionId === "") throw new Error("event TW_DATA: missing collection_id");
  if (documentId === "") throw new Error("event TW_DATA: missing document_id");

  const wireData = wire["data"];
  let document: DocumentProjection | undefined;
  if (wireData !== undefined && wireData !== null) {
    if (typeof wireData !== "object" || Array.isArray(wireData)) {
      throw new Error("event TW_DATA shape mismatch: cannot unmarshal non-object into field data");
    }
    const proj = wireData as Record<string, unknown>;
    const id = proj["id"];
    if (id !== undefined && id !== null && typeof id !== "string") {
      throw new Error("event TW_DATA shape mismatch: cannot unmarshal non-string into field data.id");
    }
    const projData = proj["data"];
    if (projData !== undefined && projData !== null && (typeof projData !== "object" || Array.isArray(projData))) {
      throw new Error("event TW_DATA shape mismatch: cannot unmarshal non-object into field data.data");
    }
    document = { id: typeof id === "string" ? id : "" };
    if (projData !== undefined && projData !== null) {
      document.data = projData as Record<string, unknown>;
    }
  }

  const change: DocumentChange = { event, databaseId, collectionId, documentId, version };
  if (document !== undefined) change.document = document;
  return change;
}

/** wireObject 取 wire 载荷对象；absent → "unexpected end of JSON input"（Go nil RawMessage 同款），null → 空对象（Go JSON null → 零值结构体同款）。 */
function wireObject(data: unknown, kind: "cron" | "event"): Record<string, unknown> {
  if (data === undefined) {
    throw new Error(`${kind} TW_DATA shape mismatch: unexpected end of JSON input`);
  }
  if (data === null) return {};
  if (typeof data !== "object" || Array.isArray(data)) {
    throw new Error(`${kind} TW_DATA shape mismatch: cannot unmarshal ${describeJson(data)} into object`);
  }
  return data as Record<string, unknown>;
}

/** wireString 取 string 字段：absent/null → ""（Go 零值），非字符串 → shape mismatch。 */
function wireString(wire: Record<string, unknown>, key: string, kind: "cron" | "event"): string {
  const v = wire[key];
  if (v === undefined || v === null) return "";
  if (typeof v !== "string") {
    throw new Error(`${kind} TW_DATA shape mismatch: cannot unmarshal ${describeJson(v)} into string field ${key}`);
  }
  return v;
}

/** wireInteger 取整数字段：absent/null → 0（Go 零值），非整数 → shape mismatch（int64 语义）。 */
function wireInteger(wire: Record<string, unknown>, key: string, kind: "cron" | "event"): number {
  const v = wire[key];
  if (v === undefined || v === null) return 0;
  if (typeof v !== "number" || !Number.isInteger(v)) {
    throw new Error(`${kind} TW_DATA shape mismatch: cannot unmarshal ${describeJson(v)} into integer field ${key}`);
  }
  return v;
}

/** isRFC3339 严格校验 RFC3339 时间串（Go time.Parse(time.RFC3339) 同口径：T 分隔 + 时区必带）。 */
function isRFC3339(v: string): boolean {
  if (!/^\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:\d{2}(\.\d+)?([Zz]|[+-]\d{2}:\d{2})$/.test(v)) return false;
  return !Number.isNaN(Date.parse(v.replace(/t/, "T").replace(/z$/, "Z")));
}

function describeJson(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}
