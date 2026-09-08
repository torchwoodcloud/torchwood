package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
)

type projectRepo struct {
	db *clients.Database
}

func NewProjectRepository(db *clients.Database) projects.Repository {
	return &projectRepo{db: db}
}

// DeleteProjectControlPlaneRows 清理 public 控制面中该项目的派生行；感知
// 调用方事务（项目删除事务内执行）。错误消息与既有删除流程保持一致。
//
// 审计留存不变量（M5 C7）：audit_logs 不随项目删除清理——审计是系统级
// 追责/取证记录，项目生命周期终结不追溯抹除历史轨迹（项目删除本身已记
// 审计，删除审计行等于销毁证据）。audit_logs 无项目 FK，行随时间自然保留；
// retention/归档策略归运维面，不归业务删除路径。
func (r *projectRepo) DeleteProjectControlPlaneRows(ctx context.Context, projectID string) error {
	conn := r.db.Conn(ctx)
	if _, err := conn.NewDelete().Model((*model.DocumentEventsOutbox)(nil)).Where("project_id = ?", projectID).Exec(ctx); err != nil {
		return fmt.Errorf("delete outbox: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM document_events_outbox_dead WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("delete outbox dead: %w", err)
	}
	if _, err := conn.NewDelete().Model((*model.APIKey)(nil)).Where("project_id = ?", projectID).Exec(ctx); err != nil {
		return fmt.Errorf("delete api_keys: %w", err)
	}
	if _, err := conn.NewDelete().Model((*model.AdminProject)(nil)).Where("project_id = ?", projectID).Exec(ctx); err != nil {
		return fmt.Errorf("delete admin_projects: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM provider_resource_index WHERE project_id = ?`, projectID); err != nil {
		return fmt.Errorf("delete provider_resource_index: %w", err)
	}
	return nil
}

func (r *projectRepo) CreateProject(ctx context.Context, p *projects.Project) error {
	m := mapProjectToModel(p)
	_, err := r.db.Conn(ctx).NewInsert().Model(m).Exec(ctx)
	if err == nil {
		p.InternalID = m.InternalID
	}
	return err
}

func (r *projectRepo) GetProject(ctx context.Context, id string) (*projects.Project, error) {
	m := new(model.Project)
	err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapProjectToDomain(m), nil
}

func (r *projectRepo) GetProjectByName(ctx context.Context, name string) (*projects.Project, error) {
	m := new(model.Project)
	err := r.db.NewSelect().Model(m).Where("name = ?", name).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapProjectToDomain(m), nil
}

func (r *projectRepo) ListProjects(ctx context.Context) ([]projects.Project, error) {
	var ms []model.Project
	err := r.db.NewSelect().Model(&ms).Order("created_at DESC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]projects.Project, len(ms))
	for i := range ms {
		out[i] = *mapProjectToDomain(&ms[i])
	}
	return out, nil
}

func (r *projectRepo) UpdateProject(ctx context.Context, p *projects.Project) error {
	m := mapProjectToModel(p)
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	return err
}

func (r *projectRepo) DeleteProject(ctx context.Context, id string) error {
	_, err := r.db.Conn(ctx).NewDelete().Model((*model.Project)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func mapProjectToModel(p *projects.Project) *model.Project {
	return &model.Project{
		ID:                 p.ID,
		Name:               p.Name,
		Description:        p.Description,
		Status:             p.Status,
		Settings:           p.Settings,
		RegistrationPolicy: p.RegistrationPolicy,
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
}

func mapProjectToDomain(m *model.Project) *projects.Project {
	return &projects.Project{
		ID:                 m.ID,
		InternalID:         m.InternalID,
		Name:               m.Name,
		Description:        m.Description,
		Status:             m.Status,
		Settings:           m.Settings,
		RegistrationPolicy: m.RegistrationPolicy,
		CreatedAt:          m.CreatedAt,
		UpdatedAt:          m.UpdatedAt,
	}
}
