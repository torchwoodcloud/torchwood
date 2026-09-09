package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	domaingroups "github.com/torchwoodcloud/torchwood/internal/domain/groups"
	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ domaingroups.MembershipRepository = (*MembershipRepository)(nil)

type MembershipRepository struct {
	db *clients.Database
}

func NewMembershipRepository(db *clients.Database) *MembershipRepository {
	return &MembershipRepository{db: db}
}

func (r *MembershipRepository) Insert(ctx context.Context, projectID string, m *domaingroups.Membership) error {
	if m == nil || strings.TrimSpace(m.ID) == "" {
		return domaingroups.ErrMembershipIDRequired
	}
	if strings.TrimSpace(m.GroupID) == "" {
		return status.Error(codes.InvalidArgument, "group_id is required")
	}
	if strings.TrimSpace(m.UserID) == "" && domainusers.NormalizeEmail(m.Email) == "" {
		return status.Error(codes.InvalidArgument, "user_id or email is required")
	}
	statusVal := m.Status
	if statusVal == "" {
		statusVal = domaingroups.StatusPending
	}
	if err := domaingroups.ValidateStatus(statusVal); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if statusVal == domaingroups.StatusAccepted && strings.TrimSpace(m.UserID) == "" {
		return status.Error(codes.InvalidArgument, "user_id is required for accepted membership")
	}
	row, err := mapMembershipToModel(m)
	if err != nil {
		return err
	}
	row.Status = statusVal
	insert := func(txCtx context.Context) error {
		conn, sch, expr, err := Scoped(txCtx, r.db, projectID, membershipTable, "m")
		if err != nil {
			return err
		}
		if _, err := conn.NewInsert().Model(row).ModelTableExpr(expr, sch).Exec(txCtx); err != nil {
			return mapMembershipUniqueError(err)
		}
		if statusVal != domaingroups.StatusAccepted {
			return nil
		}
		return NewGroupRepository(r.db).AddTotal(txCtx, projectID, m.GroupID, 1)
	}
	if statusVal == domaingroups.StatusAccepted {
		return r.db.RunInTx(ctx, insert)
	}
	return insert(ctx)
}

func (r *MembershipRepository) GetByID(ctx context.Context, projectID, id string) (*domaingroups.Membership, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, membershipTable, "m")
	if err != nil {
		return nil, err
	}
	row := new(model.Membership)
	err = conn.NewSelect().Model(row).ModelTableExpr(expr, sch).
		Where("m.id = ?", id).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapMembershipToDomain(row), nil
}

func (r *MembershipRepository) ListByGroup(ctx context.Context, projectID, groupID string) ([]*domaingroups.Membership, error) {
	if strings.TrimSpace(groupID) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, membershipTable, "m")
	if err != nil {
		return nil, err
	}
	var ms []model.Membership
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("m.group_id = ?", groupID).
		OrderExpr("m.created_at DESC, m.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return mapMembershipsToDomain(ms), nil
}

func (r *MembershipRepository) ListByUser(ctx context.Context, projectID, userID string) ([]*domaingroups.Membership, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, membershipTable, "m")
	if err != nil {
		return nil, err
	}
	var ms []model.Membership
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("m.user_id = ?", userID).
		OrderExpr("m.created_at DESC, m.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	return mapMembershipsToDomain(ms), nil
}

func (r *MembershipRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domaingroups.ErrMembershipIDRequired
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn, sch, expr, row, err := r.lockByID(txCtx, projectID, id)
		if err != nil {
			if errors.Is(err, domaingroups.ErrMembershipNotFound) {
				return nil
			}
			return err
		}
		if row.Status == domaingroups.StatusAccepted {
			if err := NewGroupRepository(r.db).AddTotal(txCtx, projectID, row.GroupID, -1); err != nil {
				return err
			}
		}
		_, err = conn.NewDelete().Model((*model.Membership)(nil)).ModelTableExpr(expr, sch).
			Where("m.id = ?", id).
			Exec(txCtx)
		return err
	})
}

func (r *MembershipRepository) Accept(ctx context.Context, projectID, id, userID string, joinedAt time.Time) error {
	if strings.TrimSpace(id) == "" {
		return domaingroups.ErrMembershipIDRequired
	}
	if strings.TrimSpace(userID) == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}
	if joinedAt.IsZero() {
		joinedAt = time.Now()
	}
	groupsRepo := NewGroupRepository(r.db)
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn, sch, expr, row, err := r.lockByID(txCtx, projectID, id)
		if err != nil {
			return err
		}
		res, err := conn.NewUpdate().Model((*model.Membership)(nil)).ModelTableExpr(expr, sch).
			Set("status = ?", domaingroups.StatusAccepted).
			Set("user_id = COALESCE(NULLIF(user_id, ''), ?)", userID).
			Set("joined_at = ?", joinedAt).
			Set("updated_at = ?", time.Now()).
			Where("m.id = ?", id).
			Where("m.status = ?", domaingroups.StatusPending).
			Exec(txCtx)
		if err != nil {
			return mapMembershipUniqueError(err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return domaingroups.ErrMembershipNotPending
		}
		return groupsRepo.AddTotal(txCtx, projectID, row.GroupID, 1)
	})
}

func (r *MembershipRepository) Reject(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domaingroups.ErrMembershipIDRequired
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn, sch, expr, _, err := r.lockByID(txCtx, projectID, id)
		if err != nil {
			return err
		}
		res, err := conn.NewUpdate().Model((*model.Membership)(nil)).ModelTableExpr(expr, sch).
			Set("status = ?", domaingroups.StatusRejected).
			Set("updated_at = ?", time.Now()).
			Where("m.id = ?", id).
			Where("m.status = ?", domaingroups.StatusPending).
			Exec(txCtx)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return domaingroups.ErrMembershipNotPending
		}
		return nil
	})
}

func (r *MembershipRepository) UpdateRoles(ctx context.Context, projectID, id string, mutate func(ctx context.Context, current *domaingroups.Membership) ([]string, error)) error {
	if strings.TrimSpace(id) == "" {
		return domaingroups.ErrMembershipIDRequired
	}
	if mutate == nil {
		return status.Error(codes.InvalidArgument, "roles mutate is required")
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn, sch, expr, row, err := r.lockByID(txCtx, projectID, id)
		if err != nil {
			return err
		}
		roles, err := mutate(txCtx, mapMembershipToDomain(row))
		if err != nil {
			return err
		}
		encoded, err := marshalJSONCol(roles, jsonEmptyArray)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		_, err = conn.NewUpdate().Model((*model.Membership)(nil)).ModelTableExpr(expr, sch).
			Set("roles = ?", encoded).
			Set("updated_at = ?", time.Now()).
			Where("m.id = ?", id).
			Exec(txCtx)
		return err
	})
}

func (r *MembershipRepository) lockByID(ctx context.Context, projectID, id string) (bun.IDB, bun.Ident, string, *model.Membership, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, membershipTable, "m")
	if err != nil {
		return nil, bun.Ident(""), "", nil, err
	}
	row := new(model.Membership)
	err = conn.NewSelect().Model(row).ModelTableExpr(expr, sch).
		Where("m.id = ?", id).
		For("UPDATE").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, bun.Ident(""), "", nil, domaingroups.ErrMembershipNotFound
		}
		return nil, bun.Ident(""), "", nil, err
	}
	return conn, sch, expr, row, nil
}

func mapMembershipUniqueError(err error) error {
	if err == nil {
		return nil
	}
	if isUniqueViolation(err) {
		return domaingroups.ErrMembershipAlreadyExists
	}
	return err
}

func mapMembershipsToDomain(ms []model.Membership) []*domaingroups.Membership {
	out := make([]*domaingroups.Membership, len(ms))
	for i := range ms {
		out[i] = mapMembershipToDomain(&ms[i])
	}
	return out
}

func mapMembershipToModel(m *domaingroups.Membership) (*model.Membership, error) {
	now := time.Now()
	created := m.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := m.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	roles, err := marshalJSONCol(m.Roles, jsonEmptyArray)
	if err != nil {
		return nil, err
	}
	statusVal := m.Status
	if statusVal == "" {
		statusVal = domaingroups.StatusPending
	}
	invited := timePtr(m.InvitedAt)
	if invited == nil && statusVal == domaingroups.StatusPending {
		invited = &now
	}
	return &model.Membership{
		ID:        m.ID,
		GroupID:   m.GroupID,
		UserID:    nullIfEmpty(strings.TrimSpace(m.UserID)),
		Email:     domainusers.NormalizeEmail(m.Email),
		Name:      m.Name,
		Roles:     roles,
		Status:    statusVal,
		InvitedAt: invited,
		JoinedAt:  timePtr(m.JoinedAt),
		CreatedAt: created,
		UpdatedAt: updated,
	}, nil
}

func mapMembershipToDomain(m *model.Membership) *domaingroups.Membership {
	roles := unmarshalStringSlice(m.Roles)
	if roles == nil {
		roles = []string{}
	}
	return &domaingroups.Membership{
		ID:        m.ID,
		GroupID:   m.GroupID,
		UserID:    derefString(m.UserID),
		Email:     m.Email,
		Name:      m.Name,
		Roles:     roles,
		Status:    m.Status,
		InvitedAt: derefTime(m.InvitedAt),
		JoinedAt:  derefTime(m.JoinedAt),
		CreatedAt: m.CreatedAt,
		UpdatedAt: m.UpdatedAt,
	}
}
