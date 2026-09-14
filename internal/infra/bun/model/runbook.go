package model

import (
	"time"

	"github.com/uptrace/bun"
)

// RunbookStep 迁移状态行（docs/design/runbook.md §2.2，迁移 000009）。
// 全量 INSERT/SELECT/DELETE、无 UPDATE——迁移历史不可变（改历史 = 环境
// 分叉之源）；id 为 identity 列，skipupdate 防 bun UPDATE 事故面（本表
// 恒不更新，防线冗余登记）。
type RunbookStep struct {
	bun.BaseModel `bun:"table:runbook_steps,alias:rs"`

	ID        int64     `bun:"id,autoincrement,skipupdate"`
	ProjectID string    `bun:"project_id,notnull"`
	Runbook   string    `bun:"runbook,notnull"`
	Version   int64     `bun:"version,notnull"`
	Name      string    `bun:"name,notnull"`
	Checksum  string    `bun:"checksum,notnull"`
	AppliedAt time.Time `bun:"applied_at,notnull,default:now()"`
}
