package audit

import (
	"context"
	"time"
)

type Entry struct {
	ID         string
	ProjectID  string
	ActorID    string
	ActorKind  string
	Action     string
	ResourceID string
	Status     string
	IP         string
	UserAgent  string
	Metadata   map[string]any
	CreatedAt  time.Time
}

// SetMetadata 惰性初始化并写入一个 metadata 键（拦截器/app 回填共用入口；
// 保留键：client、request、changes、resource_name、denied、reason）。
func (e *Entry) SetMetadata(key string, value any) {
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	e.Metadata[key] = value
}

type Repository interface {
	Insert(ctx context.Context, entry *Entry) error
	// ListByActor 返回某项目下指定 actor 的日志（created_at DESC，limit ≤ 100）。
	ListByActor(ctx context.Context, projectID, actorID string, limit int) ([]Entry, error)
	// List 按结构化过滤分页查询审计日志（created_at DESC），返回当页行与
	// 过滤条件下的总数（供 offset 分页 meta 用）。
	List(ctx context.Context, filter ListFilter) ([]Entry, int, error)
}

// ListFilter 是 List 的查询形状（与 server.v1.ListAuditLogsRequest 同构，
// 项目作用域语义由 app 用例层决定：ProjectID 空串 + AllProjects=false 时
// 由调用方先行拒绝，repo 不做兜底）。
type ListFilter struct {
	ProjectID       string
	AllProjects     bool // true = 不加项目谓词（平台 admin 全量视图，含平台行）
	IncludePlatform bool // true = 并入 project_id IS NULL 的平台级行
	ActorID         string
	ActorKind       string
	Action          string
	Status          string
	ResourceID      string
	CreatedAfter    *time.Time
	CreatedBefore   *time.Time
	Offset          int
	PageSize        int
}
