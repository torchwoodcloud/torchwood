package bunrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/lib/pq"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

type adminRepo struct {
	db *clients.Database
}

func NewAdminRepository(db *clients.Database) projects.AdminRepository {
	return &adminRepo{db: db}
}

// WithBootstrapLock 在事务内持 pg_advisory_xact_lock(key) 执行 fn；
// 事务经 clients.WithTx 注入 ctx，本 repo 的查询自动使用同一连接，
// 锁随事务提交/回滚释放。
func (r *adminRepo) WithBootstrapLock(ctx context.Context, key int64, fn func(ctx context.Context) error) error {
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := r.db.Conn(txCtx).ExecContext(txCtx, "SELECT pg_advisory_xact_lock(?)", key); err != nil {
			return err
		}
		return fn(txCtx)
	})
}

// conn 返回当前事务（WithBootstrapLock 注入时）或默认连接。
func (r *adminRepo) conn(ctx context.Context) bun.IDB {
	return r.db.Conn(ctx)
}

func (r *adminRepo) GetAdmin(ctx context.Context, id string) (*projects.Admin, error) {
	m := new(model.Admin)
	err := r.conn(ctx).NewSelect().Model(m).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return adminToDomain(m), nil
}

func (r *adminRepo) GetAdminByEmail(ctx context.Context, email string) (*projects.Admin, error) {
	m := new(model.Admin)
	err := r.conn(ctx).NewSelect().Model(m).Where("email = ?", email).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return adminToDomain(m), nil
}

func (r *adminRepo) ListAdmins(ctx context.Context) ([]projects.Admin, error) {
	var ms []model.Admin
	if err := r.conn(ctx).NewSelect().Model(&ms).Order("created_at ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]projects.Admin, len(ms))
	for i := range ms {
		out[i] = *adminToDomain(&ms[i])
	}
	return out, nil
}

// adminToDomain 映射 bun 行到领域结构（revoked_at NULL → 零值=未撤销；
// metadata 逐键拷贝，domain 侧只读不与 model 共享 map）。
func adminToDomain(m *model.Admin) *projects.Admin {
	metadata := make(map[string]string, len(m.Metadata))
	for k, v := range m.Metadata {
		metadata[k] = v
	}
	return &projects.Admin{
		ID:           m.ID,
		Email:        m.Email,
		PasswordHash: m.PasswordHash,
		Role:         m.Role,
		RevokedAt:    m.RevokedAt,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
		Metadata:     metadata,
	}
}

func (r *adminRepo) CreateAdmin(ctx context.Context, admin *projects.Admin) error {
	metadata := admin.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	m := &model.Admin{
		ID:           admin.ID,
		Email:        admin.Email,
		PasswordHash: admin.PasswordHash,
		Role:         admin.Role,
		CreatedAt:    admin.CreatedAt,
		UpdatedAt:    admin.UpdatedAt,
		Metadata:     metadata,
	}
	_, err := r.conn(ctx).NewInsert().Model(m).Exec(ctx)
	return err
}

// UpdateAdminMetadata 单语句原子合并/删键偏好 JSONB：`|| $set` 合并写入、
// `- $keys` 删除，不读-改-写，不覆盖并发写方的其他键。
func (r *adminRepo) UpdateAdminMetadata(ctx context.Context, adminID string, set map[string]string, removeKeys []string, updatedAt time.Time) error {
	setJSON, err := json.Marshal(set)
	if err != nil {
		return err
	}
	removeArr := make([]string, 0, len(removeKeys))
	removeArr = append(removeArr, removeKeys...)
	_, err = r.conn(ctx).NewUpdate().Model((*model.Admin)(nil)).
		Set("updated_at = ?", updatedAt).
		Set("metadata = (COALESCE(metadata, '{}'::jsonb) || ?::jsonb) - ?::text[]", string(setJSON), pq.StringArray(removeArr)).
		Where("id = ?", adminID).
		Exec(ctx)
	return err
}

func (r *adminRepo) UpdateAdmin(ctx context.Context, admin *projects.Admin) error {
	m := &model.Admin{
		ID:           admin.ID,
		Email:        admin.Email,
		PasswordHash: admin.PasswordHash,
		Role:         admin.Role,
		UpdatedAt:    admin.UpdatedAt,
	}
	_, err := r.conn(ctx).NewUpdate().Model(m).Column("password_hash", "role", "updated_at").Where("id = ?", admin.ID).Exec(ctx)
	return err
}

// RevokeCredentials 写凭证撤销时间戳（M5 C1）：只前推不回退（已有更晚的
// 撤销时间保持不变），避免并发路径上的旧时间戳覆盖新撤销。感知调用方事务。
func (r *adminRepo) RevokeCredentials(ctx context.Context, adminID string, revokedAt time.Time) error {
	if adminID == "" {
		return nil
	}
	res, err := r.conn(ctx).NewUpdate().Model((*model.Admin)(nil)).
		Set("revoked_at = ?", revokedAt).
		Where("id = ? AND (revoked_at IS NULL OR revoked_at < ?)", adminID, revokedAt).
		Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// admin 不存在（删除路径的行已被同事务删除）时静默成功：
		// 凭证失效语义已由"行不存在 → 拒绝"兜底。
		return nil
	}
	return nil
}

func (r *adminRepo) DeleteAdmin(ctx context.Context, id string) error {
	_, err := r.conn(ctx).NewDelete().Model((*model.Admin)(nil)).Where("id = ?", id).Exec(ctx)
	return err
}

func (r *adminRepo) CountAdminsByRole(ctx context.Context, role string) (int64, error) {
	count, err := r.conn(ctx).NewSelect().Model((*model.Admin)(nil)).Where("role = ?", role).Count(ctx)
	return int64(count), err
}
