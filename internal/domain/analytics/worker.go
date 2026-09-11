// 本文件是 worker 面（PR5）端口与领域类型：rollup 幂等重算（D7）、
// maintenance 分区治理与保留期裁剪（D5）、注销 tombstone 队列（D10）。
// 红线（执行计划）：rollup/清洗只读写项目 schema 内 analytics_* 静态表，
// 不进 outbox、不发 realtime、不触发函数（D1）。
package analytics

import (
	"context"
	"time"
)

// rollup/maintenance 的日界与批量常数。
const (
	// DefTotalsWindow 是字典 total_30d 的统计窗（设计 §5：近 30 天 count）。
	DefTotalsWindow = 30 * 24 * time.Hour
	// MaxDeleteBatch 是 tombstone 清洗单批删除行数上限（D10：分批有界，
	// (user_id, occurred_at) 索引点删）。
	MaxDeleteBatch = 5000
	// MaxCleanupUserBatches 是单轮清洗单用户的最大批次数（达到上限即停，
	// done_at 不标记、下一轮续删——限制单用户单轮的事务时长）。
	MaxCleanupUserBatches = 50
	// MaxCleanupUsers 是单轮清洗处理的 tombstone 用户数上限。
	MaxCleanupUsers = 200
	// MaxPruneBatches 是保留期裁剪单轮 user_days 修剪的最大批次数。
	MaxPruneBatches = 200
)

// NormalizeRetentionDays 把 analytics.retention_days 配置值归一到有效口径：
// 未配置（0）= 默认 90；越界值钳制进可配域 [RetentionDaysMin, RetentionDaysMax]
// （配置错误不致行为翻转——保留期裁剪与查询 raw 回退窗口护栏共用本归一）。
func NormalizeRetentionDays(days int32) int {
	switch {
	case days <= 0:
		return DefaultRetentionDays
	case days < RetentionDaysMin:
		return RetentionDaysMin
	case days > RetentionDaysMax:
		return RetentionDaysMax
	default:
		return int(days)
	}
}

// RollupRepository 是 rollup worker 的存储端口（D7 幂等重算式：同日重跑
// 覆盖不累加——重跑不翻倍；全部聚合可从 raw 重算 = 容灾重建路径）。
type RollupRepository interface {
	// RollupDay 幂等重算单日（day = UTC 零点；窗口 [day, day+24h)），同事务内：
	//   - analytics_daily 覆盖 upsert（INSERT ... ON CONFLICT (day,name)
	//     DO UPDATE SET total/unique_users/updated_at = EXCLUDED...）；
	//   - analytics_user_days 覆盖 upsert（events 覆盖语义，仅 user_id <> ''
	//     的归属事件——空归属不进用户粒度表）；
	//   - analytics_user_first_seen 极值 upsert（first_day = LEAST、
	//     last_day = GREATEST——首末见是天然幂等的极值聚合）。
	RollupDay(ctx context.Context, projectID string, day time.Time) error
	// RefreshDefinitionTotals30d 覆盖式刷新字典 total_30d：窗口（since 起）
	// 有事件的名字取 count，无事件的名字归 0（近 30 天排序键，设计 §5）。
	RefreshDefinitionTotals30d(ctx context.Context, projectID string, since time.Time) error
}

// MaintenanceRepository 是 maintenance worker 的存储端口（D5 月分区治理 +
// D10 合规清洗；DDL 与分区名由实现侧生成并经 ident 校验/形状正则约束）。
type MaintenanceRepository interface {
	// EnsureMonthlyPartitions 预建自 now 所在月起 months 个月的月分区
	//（CREATE TABLE IF NOT EXISTS ... PARTITION OF，幂等）；目标月与 DEFAULT
	// 分区数据重叠时先在事务内把 DEFAULT 中该月行搬运进新分区（INSERT..SELECT
	// 后 DELETE，父表 ACCESS EXCLUSIVE 锁内完成——搬运后新事件落正确分区）。
	EnsureMonthlyPartitions(ctx context.Context, projectID string, now time.Time, months int) error
	// ListExpiredMonthlyPartitions 返回整月范围早于 cutoff 的
	// analytics_events 分区名（整月粒度：分区上界 <= cutoff 才可裁——不足
	// 整月的部分留待下月）。
	ListExpiredMonthlyPartitions(ctx context.Context, projectID string, cutoff time.Time) ([]string, error)
	// DropPartition DROP 指定月分区（O(1)，D5 保留期裁剪的执行动词；分区名
	// 必须匹配 analytics_events_YYYY_MM 形状，其余拒绝）。
	DropPartition(ctx context.Context, projectID, partition string) error
	// PruneUserDaysBefore 批删早于 cutoff 的 user_days 行（与 raw 同窗口修剪，
	// 设计 §5；单批 <= limit，返回本批删除行数，0 = 修剪完成）。
	PruneUserDaysBefore(ctx context.Context, projectID string, cutoff time.Time, limit int64) (int64, error)
}

// TombstoneCleaner 是 maintenance worker 消费合规清洗队列的端口（D10）。
// 幂等：重删安全（行已删 = 0 行受影响），done_at 只在三表清理完成后标记。
type TombstoneCleaner interface {
	// ListPendingDeletionUsers 返回待清洗的 tombstone 用户（done_at IS NULL，
	// 按 enqueued_at 先进先出，上限 limit）。
	ListPendingDeletionUsers(ctx context.Context, projectID string, limit int) ([]string, error)
	// DeleteEventsByUserBatch 按身份点删该用户 raw 事件一批（(user_id,
	// occurred_at) 索引 + PK (id, occurred_at) 精确行定位；<= limit 行，
	// 返回本批删除行数，< limit = 该用户 raw 已清空）。
	DeleteEventsByUserBatch(ctx context.Context, projectID, userID string, limit int64) (int64, error)
	// DeleteUserDaysByUser 删除该用户全部 user_days 行（PK 前缀，行数有界
	// = 活跃天数 <= 保留期天数）。返回删除行数。
	DeleteUserDaysByUser(ctx context.Context, projectID, userID string) (int64, error)
	// DeleteFirstSeenByUser 删除该用户 first_seen 行。返回删除行数。
	DeleteFirstSeenByUser(ctx context.Context, projectID, userID string) (int64, error)
	// MarkDeletionDone 标记 tombstone 已清洗（幂等覆盖）。
	MarkDeletionDone(ctx context.Context, projectID, userID string, doneAt time.Time) error
}

// DeletionQueueRepository 是注销钩子的 tombstone 写入端口（D10：注销事务内
// 插入，worker 异步消费；重删幂等）。
type DeletionQueueRepository interface {
	// EnqueueUserDeletion 写入 tombstone（ON CONFLICT (user_id) DO NOTHING：
	// 同用户重复注销不报错、不覆盖首次 enqueued_at）。ctx 携带调用方事务时
	// 加入该事务（uow.Runner 的 WithTx 通道），否则独立执行。
	EnqueueUserDeletion(ctx context.Context, projectID, userID string, enqueuedAt time.Time) error
}
