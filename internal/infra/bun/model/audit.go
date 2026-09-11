package model

import (
	"time"

	"github.com/uptrace/bun"
)

type AuditLog struct {
	bun.BaseModel `bun:"table:audit_logs,alias:al"`

	// nullzero：平台级行（admin 无项目上下文）/匿名 actor/无资源标注时
	// 写 NULL 而非空串，对齐 000001 的可空列设计（List 的 IncludePlatform
	// 匹配 IS NULL；查询侧同时容忍历史空串行）。
	ID         string         `bun:"id,pk"`
	ProjectID  string         `bun:"project_id,nullzero"`
	ActorID    string         `bun:"actor_id,nullzero"`
	ActorKind  string         `bun:"actor_kind,notnull"`
	Action     string         `bun:"action,notnull"`
	ResourceID string         `bun:"resource_id,nullzero"`
	Status     string         `bun:"status,notnull"`
	IP         string         `bun:"ip"`
	UserAgent  string         `bun:"user_agent"`
	Metadata   map[string]any `bun:"metadata,type:jsonb"`
	CreatedAt  time.Time      `bun:"created_at,notnull"`
}

type AdminProject struct {
	bun.BaseModel `bun:"table:admin_projects,alias:cap"`

	AdminID   string    `bun:"admin_id,pk"`
	ProjectID string    `bun:"project_id,pk"`
	CreatedAt time.Time `bun:"created_at,notnull"`
}
