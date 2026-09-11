package analytics

import "context"

// IngestRepository 是事件摄入的存储端口（D2：同步直写 PG，不经任何缓冲
// 队列；项目 schema 内的 analytics_* 静态表，bunrepo 提供 Scoped 实现）。
// 不进 outbox / 不发 realtime / 不触发函数（D1 红线）。
type IngestRepository interface {
	// InsertEvents 批量多行单语句 INSERT 追加只写事件（at-least-once，
	// 不做去重 D14；id 为 BIGSERIAL 调试序由库分配）。
	InsertEvents(ctx context.Context, projectID string, events []Event) error
	// UpsertEventDefinitions 字典 upsert：新名插入 first/last_seen；存量名
	// 仅推进 last_seen（GREATEST），first_seen 不回退。批内同名调用方已
	// 去重（实现侧防御性去重兜底，防同语句二次命中冲突行）。
	UpsertEventDefinitions(ctx context.Context, projectID string, defs []EventDefinition) error
	// CountEventDefinitions 项目内事件名总数（软上限 MaxEventNames 的
	// 判定量；并发摄取轻微超扣可容忍，D12）。
	CountEventDefinitions(ctx context.Context, projectID string) (int64, error)
}
