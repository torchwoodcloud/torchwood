package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	domaingroups "github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ domaingroups.GroupRepository = (*GroupRepository)(nil)

type GroupRepository struct {
	db *clients.Database
}

func NewGroupRepository(db *clients.Database) *GroupRepository {
	return &GroupRepository{db: db}
}

func (r *GroupRepository) Insert(ctx context.Context, projectID string, group *domaingroups.Group) error {
	if group == nil || strings.TrimSpace(group.ID) == "" {
		return domaingroups.ErrGroupIDRequired
	}
	if strings.TrimSpace(group.Name) == "" {
		return status.Error(codes.InvalidArgument, "name is required")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return err
	}
	m, err := mapGroupToModel(group)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *GroupRepository) GetByID(ctx context.Context, projectID, id string) (*domaingroups.Group, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return nil, err
	}
	m := new(model.Group)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("g.id = ?", id).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapGroupToDomain(m), nil
}

func (r *GroupRepository) Update(ctx context.Context, projectID, id string, cols map[string]any) error {
	if strings.TrimSpace(id) == "" {
		return domaingroups.ErrGroupIDRequired
	}
	cols, err := domaingroups.NormalizeGroupUpdateColumns(cols)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return err
	}
	q := conn.NewUpdate().Model((*model.Group)(nil)).ModelTableExpr(expr, sch).
		Where("g.id = ?", id)
	for col, val := range cols {
		encoded, err := encodeGroupUpdateValue(col, val)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		q = q.Set(col+" = ?", encoded)
	}
	if _, ok := cols["updated_at"]; !ok {
		q = q.Set("updated_at = ?", time.Now())
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Error(codes.NotFound, "group not found")
	}
	return nil
}

func (r *GroupRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domaingroups.ErrGroupIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.Group)(nil)).ModelTableExpr(expr, sch).
		Where("g.id = ?", id).
		Exec(ctx)
	return err
}

func (r *GroupRepository) List(ctx context.Context, projectID string) ([]*domaingroups.Group, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return nil, err
	}
	var ms []model.Group
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		OrderExpr("g.created_at DESC, g.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domaingroups.Group, len(ms))
	for i := range ms {
		out[i] = mapGroupToDomain(&ms[i])
	}
	return out, nil
}

func (r *GroupRepository) AddTotal(ctx context.Context, projectID, groupID string, delta int64) error {
	if strings.TrimSpace(groupID) == "" {
		return domaingroups.ErrGroupIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return err
	}
	res, err := conn.NewUpdate().Model((*model.Group)(nil)).ModelTableExpr(expr, sch).
		Set("total = GREATEST(total + ?, 0)", delta).
		Set("updated_at = ?", time.Now()).
		Where("g.id = ?", groupID).
		Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Error(codes.NotFound, "group not found")
	}
	return nil
}

func (r *GroupRepository) RecountAccepted(ctx context.Context, projectID, groupID string) error {
	if strings.TrimSpace(groupID) == "" {
		return domaingroups.ErrGroupIDRequired
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, groupTable, "g")
	if err != nil {
		return err
	}
	sub := fmt.Sprintf(
		"total = (SELECT COUNT(*) FROM %s.%s AS m WHERE m.group_id = g.id AND m.status = ?)",
		quoted, membershipTable,
	)
	res, err := conn.NewUpdate().Model((*model.Group)(nil)).ModelTableExpr(expr, sch).
		Set(sub, domaingroups.StatusAccepted).
		Set("updated_at = ?", time.Now()).
		Where("g.id = ?", groupID).
		Exec(ctx)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Error(codes.NotFound, "group not found")
	}
	return nil
}

func encodeGroupUpdateValue(col string, v any) (any, error) {
	switch col {
	case "permissions":
		return marshalJSONCol(v, jsonEmptyArray)
	case "prefs":
		return marshalJSONCol(v, jsonEmptyObject)
	default:
		return v, nil
	}
}

func mapGroupToModel(g *domaingroups.Group) (*model.Group, error) {
	now := time.Now()
	created := g.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := g.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	perms, err := marshalJSONCol(g.Permissions, jsonEmptyArray)
	if err != nil {
		return nil, err
	}
	prefs, err := marshalJSONCol(g.Prefs, jsonEmptyObject)
	if err != nil {
		return nil, err
	}
	return &model.Group{
		ID:          g.ID,
		Name:        strings.TrimSpace(g.Name),
		Permissions: perms,
		Total:       g.Total,
		Prefs:       prefs,
		CreatedAt:   created,
		UpdatedAt:   updated,
	}, nil
}

func mapGroupToDomain(m *model.Group) *domaingroups.Group {
	g := &domaingroups.Group{
		ID:          m.ID,
		Name:        m.Name,
		Permissions: unmarshalStringSlice(m.Permissions),
		Total:       m.Total,
		Prefs:       unmarshalAnyMap(m.Prefs),
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
	if g.Permissions == nil {
		g.Permissions = []string{}
	}
	return g
}
