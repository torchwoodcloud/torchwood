package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

type adminProjectRepo struct {
	db *clients.Database
}

func NewAdminProjectRepository(db *clients.Database) projects.AdminProjectRepository {
	return &adminProjectRepo{db: db}
}

func (r *adminProjectRepo) HasProjectAccess(ctx context.Context, adminID, projectID string) (bool, error) {
	return r.db.NewSelect().Model((*model.AdminProject)(nil)).
		Where("admin_id = ? AND project_id = ?", adminID, projectID).
		Exists(ctx)
}

func (r *adminProjectRepo) GrantProjectAccess(ctx context.Context, adminID, projectID string) error {
	m := &model.AdminProject{
		AdminID:   adminID,
		ProjectID: projectID,
		CreatedAt: time.Now(),
	}
	_, err := r.db.NewInsert().Model(m).
		On("CONFLICT (admin_id, project_id) DO NOTHING").
		Exec(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func (r *adminProjectRepo) ListProjectIDs(ctx context.Context, adminID string) ([]string, error) {
	var rows []model.AdminProject
	if err := r.db.NewSelect().Model(&rows).Where("admin_id = ?", adminID).Scan(ctx); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ProjectID)
	}
	return ids, nil
}
