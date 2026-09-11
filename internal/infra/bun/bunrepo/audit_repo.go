package bunrepo

import (
	"context"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/uptrace/bun"
)

type auditRepo struct {
	db *clients.Database
}

func NewAuditRepository(db *clients.Database) audit.Repository {
	return &auditRepo{db: db}
}

func (r *auditRepo) Insert(ctx context.Context, entry *audit.Entry) error {
	if entry == nil {
		return nil
	}
	id := entry.ID
	if id == "" {
		id = idgen.UUID().String()
	}
	createdAt := entry.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	metadata := entry.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	m := &model.AuditLog{
		ID:         id,
		ProjectID:  entry.ProjectID,
		ActorID:    entry.ActorID,
		ActorKind:  entry.ActorKind,
		Action:     entry.Action,
		ResourceID: entry.ResourceID,
		Status:     entry.Status,
		IP:         entry.IP,
		UserAgent:  entry.UserAgent,
		Metadata:   metadata,
		CreatedAt:  createdAt,
	}
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	return err
}

const auditListMaxLimit = 100

func (r *auditRepo) ListByActor(ctx context.Context, projectID, actorID string, limit int) ([]audit.Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > auditListMaxLimit {
		limit = auditListMaxLimit
	}
	var rows []model.AuditLog
	if err := r.db.NewSelect().Model(&rows).
		Where("al.project_id = ? AND al.actor_id = ?", projectID, actorID).
		Order("al.created_at DESC").
		Limit(limit).
		Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]audit.Entry, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, audit.Entry{
			ID:         row.ID,
			ProjectID:  row.ProjectID,
			ActorID:    row.ActorID,
			ActorKind:  row.ActorKind,
			Action:     row.Action,
			ResourceID: row.ResourceID,
			Status:     row.Status,
			IP:         row.IP,
			UserAgent:  row.UserAgent,
			Metadata:   row.Metadata,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, nil
}

// List 按结构化过滤分页查询（created_at DESC）。项目谓词三种形态：
// AllProjects（无谓词，含平台行）/ ProjectID（单项目）/ ProjectID+
// IncludePlatform（项目行 ∪ 平台行，OR 谓词走不了单边索引，量级可控）。
func (r *auditRepo) List(ctx context.Context, filter audit.ListFilter) ([]audit.Entry, int, error) {
	base := r.db.NewSelect().Model((*model.AuditLog)(nil))
	base = applyAuditListFilter(base, filter)
	total, err := base.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > auditListMaxLimit*10 { // 与 crud 上限（1000）对齐
		pageSize = auditListMaxLimit * 10
	}
	var rows []model.AuditLog
	query := applyAuditListFilter(r.db.NewSelect().Model(&rows), filter).
		Order("al.created_at DESC").
		Limit(pageSize).
		Offset(filter.Offset)
	if err := query.Scan(ctx); err != nil {
		return nil, 0, err
	}
	out := make([]audit.Entry, 0, len(rows))
	for i := range rows {
		row := &rows[i]
		out = append(out, audit.Entry{
			ID:         row.ID,
			ProjectID:  row.ProjectID,
			ActorID:    row.ActorID,
			ActorKind:  row.ActorKind,
			Action:     row.Action,
			ResourceID: row.ResourceID,
			Status:     row.Status,
			IP:         row.IP,
			UserAgent:  row.UserAgent,
			Metadata:   row.Metadata,
			CreatedAt:  row.CreatedAt,
		})
	}
	return out, total, nil
}

func applyAuditListFilter(query *bun.SelectQuery, filter audit.ListFilter) *bun.SelectQuery {
	if !filter.AllProjects {
		if filter.IncludePlatform {
			// 平台级行 = project_id IS NULL（新写入，model nullzero）；
			// 兼容历史空串行。
			query = query.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
				return q.Where("al.project_id = ?", filter.ProjectID).
					WhereOr("al.project_id IS NULL").
					WhereOr("al.project_id = ''")
			})
		} else {
			query = query.Where("al.project_id = ?", filter.ProjectID)
		}
	}
	if filter.ActorID != "" {
		query = query.Where("al.actor_id = ?", filter.ActorID)
	}
	if filter.ActorKind != "" {
		query = query.Where("al.actor_kind = ?", filter.ActorKind)
	}
	if filter.Action != "" {
		query = query.Where("al.action = ?", filter.Action)
	}
	if filter.Status != "" {
		query = query.Where("al.status = ?", filter.Status)
	}
	if filter.ResourceID != "" {
		query = query.Where("al.resource_id = ?", filter.ResourceID)
	}
	if filter.CreatedAfter != nil {
		query = query.Where("al.created_at >= ?", *filter.CreatedAfter)
	}
	if filter.CreatedBefore != nil {
		query = query.Where("al.created_at <= ?", *filter.CreatedBefore)
	}
	return query
}
