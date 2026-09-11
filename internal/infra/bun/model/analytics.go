package model

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"
)

// 本文件是项目 schema 内 analytics 六表的 bun 模型（projectschema 迁移
// 000019，docs/design/analytics.md §5）。写路径纪律：INSERT 一律显式
// .Column(...) 白名单；AnalyticsEvent.ID 为 BIGSERIAL 调试序，白名单必须
// 排除本列（bun 无 serial 特判，见 DocumentEventsOutbox.Seq 同款约定）。

// AnalyticsEvent 是原始事件行（analytics_events，月 RANGE 分区父表；
// PK (id, occurred_at) 含分区键；无唯一约束——接受重复 D14）。
// 表名由 ModelTableExpr 限定（Scoped 模式，schema 每请求不同）。
// 列不带 default 标签：仓储映射恒显式赋值（含零值串），避免 bun 对
// default 标记列渲染 DEFAULT 占位 + 附加 RETURNING（摄入热路径）。
type AnalyticsEvent struct {
	bun.BaseModel `bun:"alias:ae"`

	ID         int64           `bun:"id,pk"`
	Name       string          `bun:"name,notnull"`
	UserID     string          `bun:"user_id,notnull"`
	SessionID  string          `bun:"session_id,notnull"`
	Source     string          `bun:"source,notnull"`
	Platform   string          `bun:"platform,notnull"`
	AppVersion string          `bun:"app_version,notnull"`
	OccurredAt time.Time       `bun:"occurred_at,notnull"`
	IngestedAt time.Time       `bun:"ingested_at,notnull"`
	Props      json.RawMessage `bun:"props,type:jsonb,notnull"`
}

// AnalyticsEventDefinition 是事件字典行（analytics_event_definitions；
// name 主键；first_seen 首见不回退，last_seen 摄取 upsert 取 GREATEST）。
type AnalyticsEventDefinition struct {
	bun.BaseModel `bun:"alias:aed"`

	Name      string    `bun:"name,pk"`
	FirstSeen time.Time `bun:"first_seen,notnull"`
	LastSeen  time.Time `bun:"last_seen,notnull"`
	Total30d  int64     `bun:"total_30d,notnull,default:0"` // rollup 维护（PR5）
}

// AnalyticsDaily 是事件×日聚合行（analytics_daily；rollup worker 幂等重算，
// PR5——本 PR 仅建模型供查询面/worker 后续消费）。
type AnalyticsDaily struct {
	bun.BaseModel `bun:"alias:ad"`

	Day         time.Time `bun:"day,pk,notnull"` // DATE 列
	Name        string    `bun:"name,pk,notnull"`
	Total       int64     `bun:"total,notnull"`
	UniqueUsers int64     `bun:"unique_users,notnull"`
	UpdatedAt   time.Time `bun:"updated_at,notnull"`
}

// AnalyticsUserDay 是用户×日活跃集行（analytics_user_days；UV/留存共同基座）。
type AnalyticsUserDay struct {
	bun.BaseModel `bun:"alias:aud"`

	UserID string    `bun:"user_id,pk,notnull"`
	Day    time.Time `bun:"day,pk,notnull"` // DATE 列
	Events int64     `bun:"events,notnull,default:0"`
}

// AnalyticsUserFirstSeen 是用户首见/末见行（analytics_user_first_seen；
// cohort 锚点 + 下钻档案）。
type AnalyticsUserFirstSeen struct {
	bun.BaseModel `bun:"alias:ufs"`

	UserID    string    `bun:"user_id,pk,notnull"`
	FirstDay  time.Time `bun:"first_day,notnull"` // DATE 列
	LastDay   time.Time `bun:"last_day,notnull"`  // DATE 列
	UpdatedAt time.Time `bun:"updated_at,notnull"`
}

// AnalyticsUserDeletion 是合规清洗队列行（analytics_user_deletions；
// 注销钩子写入 tombstone，worker 硬删后标记 done_at——PR5 接线）。
type AnalyticsUserDeletion struct {
	bun.BaseModel `bun:"alias:audl"`

	UserID     string     `bun:"user_id,pk,notnull"`
	EnqueuedAt time.Time  `bun:"enqueued_at,notnull"`
	DoneAt     *time.Time `bun:"done_at"`
}
