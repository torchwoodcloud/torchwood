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
