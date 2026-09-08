package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
)

type inviteCodeRepo struct {
	db *clients.Database
}

func NewInviteCodeRepository(db *clients.Database) projects.InviteCodeRepository {
	return &inviteCodeRepo{db: db}
}

func (r *inviteCodeRepo) CreateInviteCode(ctx context.Context, c *projects.InviteCode) error {
	m := &model.InviteCode{
		ID:        c.ID,
		ProjectID: c.ProjectID,
		Code:      c.Code,
		MaxUses:   c.MaxUses,
		UsedCount: c.UsedCount,
		ExpireAt:  c.ExpireAt,
		RevokedAt: c.RevokedAt,
		CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt,
	}
	_, err := r.db.Conn(ctx).NewInsert().Model(m).Exec(ctx)
	return err
}

func (r *inviteCodeRepo) ListInviteCodes(ctx context.Context, projectID string, limit int) ([]projects.InviteCode, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	var ms []model.InviteCode
	err := r.db.NewSelect().Model(&ms).
		Where("project_id = ?", projectID).
		Order("created_at DESC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]projects.InviteCode, len(ms))
	for i := range ms {
		out[i] = mapInviteCodeToDomain(&ms[i])
	}
	return out, nil
}

func (r *inviteCodeRepo) GetInviteCode(ctx context.Context, projectID, id string) (*projects.InviteCode, error) {
	m := new(model.InviteCode)
	err := r.db.Conn(ctx).NewSelect().Model(m).Where("project_id = ? AND id = ?", projectID, id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	d := mapInviteCodeToDomain(m)
	return &d, nil
}

func (r *inviteCodeRepo) RevokeInviteCode(ctx context.Context, projectID, id string) (bool, error) {
	res, err := r.db.Conn(ctx).NewUpdate().
		Model((*model.InviteCode)(nil)).
		Set("revoked_at = ?", time.Now()).
		Where("project_id = ? AND id = ? AND revoked_at IS NULL", projectID, id).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ConsumeInviteCode 原子消费（T-03 验收：并发同码只有一个成功）：有效性
// 判定与 used_count 递增在单条 UPDATE 内完成，行锁天然串行化并发。
// 无码/错码/过期/已耗尽/已吊销统一返回 false（不区分原因，防探测）。
// 占位符用 `?`（bun 统一重写为驱动占位，见 clients/tx.go raw-SQL 先例）。
func (r *inviteCodeRepo) ConsumeInviteCode(ctx context.Context, projectID, code string) (bool, error) {
	res, err := r.db.Conn(ctx).ExecContext(ctx, `
		UPDATE invite_codes
		SET used_count = used_count + 1
		WHERE project_id = ?
		  AND code = ?
		  AND revoked_at IS NULL
		  AND (expire_at IS NULL OR expire_at > now())
		  AND used_count < max_uses
	`, projectID, code)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func mapInviteCodeToDomain(m *model.InviteCode) projects.InviteCode {
	return projects.InviteCode{
		ID:        m.ID,
		ProjectID: m.ProjectID,
		Code:      m.Code,
		MaxUses:   m.MaxUses,
		UsedCount: m.UsedCount,
		ExpireAt:  m.ExpireAt,
		RevokedAt: m.RevokedAt,
		CreatedBy: m.CreatedBy,
		CreatedAt: m.CreatedAt,
	}
}
