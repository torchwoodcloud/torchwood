package bunrepo

import (
	"context"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
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
