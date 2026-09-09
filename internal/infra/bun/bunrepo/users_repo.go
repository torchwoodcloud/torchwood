package bunrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	domainusers "github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"github.com/torchwoodcloud/torchwood/pkg/query"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/driver/pgdriver"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	userListDefaultLimit = 50
	userListMaxLimit     = 100
)

var (
	_ domainusers.Repository = (*UserRepository)(nil)

	jsonEmptyObject = json.RawMessage(`{}`)
	jsonEmptyArray  = json.RawMessage(`[]`)
)

type UserRepository struct {
	db *clients.Database
}

func NewUserRepository(db *clients.Database) *UserRepository {
	return &UserRepository{db: db}
}

func (r *UserRepository) GetByEmail(ctx context.Context, projectID, email string) (*domainusers.User, error) {
	email = domainusers.NormalizeEmail(email)
	if email == "" {
		return nil, nil
	}
	return r.getByColumn(ctx, projectID, "u.email", email)
}

func (r *UserRepository) GetByID(ctx context.Context, projectID, id string) (*domainusers.User, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	return r.getByColumn(ctx, projectID, "u.id", id)
}

func (r *UserRepository) GetByPhone(ctx context.Context, projectID, phone string) (*domainusers.User, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" {
		return nil, nil
	}
	return r.getByColumn(ctx, projectID, "u.phone", phone)
}

func (r *UserRepository) getByColumn(ctx context.Context, projectID, column, value string) (*domainusers.User, error) {
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, userTable, "u")
	if err != nil {
		return nil, err
	}
	m := new(model.User)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where(column+" = ?", value).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapUserToDomain(m), nil
}

func (r *UserRepository) Insert(ctx context.Context, projectID string, user *domainusers.User) error {
	if user == nil || strings.TrimSpace(user.ID) == "" {
		return domainusers.ErrUserIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, userTable, "u")
	if err != nil {
		return err
	}
	m, err := mapUserToModel(user)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return mapUserUniqueError(err)
}

func (r *UserRepository) Update(ctx context.Context, projectID, id string, cols map[string]any) error {
	if strings.TrimSpace(id) == "" {
		return domainusers.ErrUserIDRequired
	}
	cols, err := domainusers.NormalizeUpdateColumns(cols)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, userTable, "u")
	if err != nil {
		return err
	}
	q := conn.NewUpdate().Model((*model.User)(nil)).ModelTableExpr(expr, sch).
		Where("u.id = ?", id)
	for col, val := range cols {
		encoded, err := encodeUserUpdateValue(col, val)
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
		return mapUserUniqueError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return status.Error(codes.NotFound, "user not found")
	}
	return nil
}

func (r *UserRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domainusers.ErrUserIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, userTable, "u")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.User)(nil)).ModelTableExpr(expr, sch).
		Where("u.id = ?", id).
		Exec(ctx)
	return err
}

func (r *UserRepository) List(ctx context.Context, projectID string, f domainusers.ListFilter) (*domainusers.ListResult, error) {
	parsed, err := domainusers.ParseUserList(f.Queries)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, userTable, "u")
	if err != nil {
		return nil, err
	}
	limit, offset, err := userListPage(parsed, f)
	if err != nil {
		return nil, err
	}

	countQ := conn.NewSelect().Model((*model.User)(nil)).ModelTableExpr(expr, sch)
	countQ = applyUserListFilters(countQ, parsed)
	total, err := countQ.Count(ctx)
	if err != nil {
		return nil, err
	}

	var ms []model.User
	sel := conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch)
	sel = applyUserListFilters(sel, parsed)
	sel = applyUserListOrder(sel, parsed)
	if err := sel.Limit(limit).Offset(offset).Scan(ctx); err != nil {
		return nil, err
	}
	out := &domainusers.ListResult{
		Users:      make([]*domainusers.User, len(ms)),
		TotalCount: int64(total),
	}
	for i := range ms {
		out.Users[i] = mapUserToDomain(&ms[i])
	}
	if len(ms) > 0 && int64(offset+len(ms)) < out.TotalCount {
		token, err := crud.EncodePageToken(offset + len(ms))
		if err != nil {
			return nil, err
		}
		out.NextPageToken = token
	}
	return out, nil
}

func (r *UserRepository) UpdateFactors(ctx context.Context, projectID, id string, mutate func(current json.RawMessage) (json.RawMessage, error)) error {
	if strings.TrimSpace(id) == "" {
		return domainusers.ErrUserIDRequired
	}
	if mutate == nil {
		return status.Error(codes.InvalidArgument, "factors mutate is required")
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn, sch, expr, err := Scoped(txCtx, r.db, projectID, userTable, "u")
		if err != nil {
			return err
		}
		m := new(model.User)
		err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
			Column("u.factors").
			Where("u.id = ?", id).
			For("UPDATE").
			Scan(txCtx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return status.Error(codes.NotFound, "user not found")
			}
			return err
		}
		current := m.Factors
		if len(current) == 0 {
			current = append(json.RawMessage(nil), jsonEmptyObject...)
		}
		next, err := mutate(current)
		if err != nil {
			return err
		}
		if len(next) == 0 {
			next = append(json.RawMessage(nil), jsonEmptyObject...)
		}
		var decoded any
		if err := json.Unmarshal(next, &decoded); err != nil {
			return status.Error(codes.InvalidArgument, "factors must be valid JSON")
		}
		_, err = conn.NewUpdate().Model((*model.User)(nil)).ModelTableExpr(expr, sch).
			Set("factors = ?", next).
			Set("updated_at = ?", time.Now()).
			Where("u.id = ?", id).
			Exec(txCtx)
		return err
	})
}

func applyUserListFilters(q *bun.SelectQuery, parsed *query.Query) *bun.SelectQuery {
	if parsed == nil {
		return q
	}
	parsed.WalkLeaves(func(f query.Filter) {
		col := "u." + f.Attribute
		switch f.Op {
		case query.OpEqual:
			if len(f.Values) == 1 {
				q = q.Where(col+" = ?", listFilterValue(f.Attribute, f.Values[0]))
			} else if len(f.Values) > 1 {
				args := make([]any, len(f.Values))
				for i, v := range f.Values {
					args[i] = listFilterValue(f.Attribute, v)
				}
				q = q.Where(col+" IN (?)", bun.List(args))
			}
		case query.OpGreaterThan:
			if len(f.Values) > 0 {
				q = q.Where(col+" > ?", listFilterValue(f.Attribute, f.Values[0]))
			}
		case query.OpLessThan:
			if len(f.Values) > 0 {
				q = q.Where(col+" < ?", listFilterValue(f.Attribute, f.Values[0]))
			}
		}
	})
	return q
}

func applyUserListOrder(q *bun.SelectQuery, parsed *query.Query) *bun.SelectQuery {
	if parsed == nil || len(parsed.Orders) == 0 {
		return q.OrderExpr("u.created_at DESC, u.id DESC")
	}
	for _, o := range parsed.Orders {
		dir := "ASC"
		if o.Desc {
			dir = "DESC"
		}
		q = q.OrderExpr("u." + o.Attribute + " " + dir)
	}
	return q.OrderExpr("u.id DESC")
}

func userListPage(parsed *query.Query, f domainusers.ListFilter) (limit, offset int, err error) {
	limit = 0
	if parsed != nil {
		limit = parsed.Limit
		offset = parsed.Offset
	}
	if limit == 0 {
		limit = int(f.PageSize)
	}
	if limit <= 0 {
		limit = userListDefaultLimit
	}
	if limit > userListMaxLimit {
		limit = userListMaxLimit
	}
	if f.PageToken != "" {
		off, decErr := crud.DecodePageToken(f.PageToken)
		if decErr != nil {
			return 0, 0, status.Error(codes.InvalidArgument, "invalid page token")
		}
		offset = off
	}
	if offset < 0 {
		return 0, 0, status.Error(codes.InvalidArgument, "offset must be non-negative")
	}
	return limit, offset, nil
}

func listFilterValue(attr, raw string) any {
	switch attr {
	case "created_at", "updated_at":
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t
		}
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			return t
		}
	}
	return raw
}

func mapUserUniqueError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) && pgErr.Field('C') == "23505" {
		name := strings.ToLower(pgErr.Field('N') + " " + err.Error())
		if domainusers.IsEmailUniqueViolation(name) {
			return domainusers.ErrEmailAlreadyRegistered
		}
	}
	if domainusers.IsEmailUniqueViolation(err.Error()) {
		return domainusers.ErrEmailAlreadyRegistered
	}
	return err
}

func encodeUserUpdateValue(col string, v any) (any, error) {
	switch col {
	case "labels":
		return marshalJSONCol(v, jsonEmptyArray)
	case "prefs", "factors":
		return marshalJSONCol(v, jsonEmptyObject)
	default:
		return v, nil
	}
}

func marshalJSONCol(v any, empty json.RawMessage) (json.RawMessage, error) {
	if v == nil {
		return append(json.RawMessage(nil), empty...), nil
	}
	switch t := v.(type) {
	case json.RawMessage:
		if len(t) == 0 {
			return append(json.RawMessage(nil), empty...), nil
		}
		return t, nil
	case []byte:
		if len(t) == 0 {
			return append(json.RawMessage(nil), empty...), nil
		}
		return json.RawMessage(t), nil
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if string(b) == "null" {
			return append(json.RawMessage(nil), empty...), nil
		}
		return json.RawMessage(b), nil
	}
}

func mapUserToModel(u *domainusers.User) (*model.User, error) {
	now := time.Now()
	created := u.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := u.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	userStatus := u.Status
	if userStatus == "" {
		userStatus = domainusers.StatusActive
	}
	labels, err := marshalJSONCol(u.Labels, jsonEmptyArray)
	if err != nil {
		return nil, err
	}
	prefs, err := marshalJSONCol(u.Prefs, jsonEmptyObject)
	if err != nil {
		return nil, err
	}
	factors, err := marshalJSONCol(u.Factors, jsonEmptyObject)
	if err != nil {
		return nil, err
	}
	return &model.User{
		ID:            u.ID,
		Email:         domainusers.NormalizeEmail(u.Email),
		PasswordHash:  u.PasswordHash,
		Name:          u.Name,
		Status:        userStatus,
		EmailVerified: u.EmailVerified,
		PendingEmail:  u.PendingEmail,
		Phone:         strings.TrimSpace(u.Phone),
		PhoneVerified: u.PhoneVerified,
		Labels:        labels,
		Prefs:         prefs,
		Factors:       factors,
		CreatedAt:     created,
		UpdatedAt:     updated,
	}, nil
}

func mapUserToDomain(m *model.User) *domainusers.User {
	u := &domainusers.User{
		ID:            m.ID,
		Email:         m.Email,
		PasswordHash:  m.PasswordHash,
		Name:          m.Name,
		Status:        m.Status,
		EmailVerified: m.EmailVerified,
		PendingEmail:  m.PendingEmail,
		Phone:         m.Phone,
		PhoneVerified: m.PhoneVerified,
		Factors:       append(json.RawMessage(nil), m.Factors...),
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
	if len(m.Labels) > 0 && string(m.Labels) != "null" {
		_ = json.Unmarshal(m.Labels, &u.Labels)
	}
	if len(m.Prefs) > 0 && string(m.Prefs) != "null" {
		_ = json.Unmarshal(m.Prefs, &u.Prefs)
	}
	if u.Prefs == nil {
		u.Prefs = map[string]any{}
	}
	if len(u.Factors) == 0 {
		u.Factors = append(json.RawMessage(nil), jsonEmptyObject...)
	}
	return u
}
