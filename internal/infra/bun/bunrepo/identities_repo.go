package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ domainauth.IdentityRepository = (*IdentityRepository)(nil)

type IdentityRepository struct {
	db *clients.Database
}

func NewIdentityRepository(db *clients.Database) *IdentityRepository {
	return &IdentityRepository{db: db}
}

func (r *IdentityRepository) Insert(ctx context.Context, projectID string, identity *domainauth.Identity) error {
	if identity == nil || strings.TrimSpace(identity.ID) == "" {
		return domainauth.ErrIdentityIDRequired
	}
	if strings.TrimSpace(identity.UserID) == "" {
		return status.Error(codes.InvalidArgument, "user_id is required")
	}
	if strings.TrimSpace(identity.Provider) == "" || strings.TrimSpace(identity.ProviderUID) == "" {
		return status.Error(codes.InvalidArgument, "provider and provider_uid are required")
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, identityTable, "i")
	if err != nil {
		return err
	}
	m, err := mapIdentityToModel(identity)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return mapIdentityUniqueError(err)
}

func (r *IdentityRepository) GetByID(ctx context.Context, projectID, id string) (*domainauth.Identity, error) {
	if strings.TrimSpace(id) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, identityTable, "i")
	if err != nil {
		return nil, err
	}
	m := new(model.Identity)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("i.id = ?", id).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapIdentityToDomain(m), nil
}

func (r *IdentityRepository) GetByProviderUID(ctx context.Context, projectID, provider, uid string) (*domainauth.Identity, error) {
	provider = strings.TrimSpace(provider)
	uid = strings.TrimSpace(uid)
	if provider == "" || uid == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, identityTable, "i")
	if err != nil {
		return nil, err
	}
	m := new(model.Identity)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("i.provider = ?", provider).
		Where("i.provider_uid = ?", uid).
		Limit(1).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapIdentityToDomain(m), nil
}

func (r *IdentityRepository) ListByUser(ctx context.Context, projectID, userID string) ([]*domainauth.Identity, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, nil
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, identityTable, "i")
	if err != nil {
		return nil, err
	}
	var ms []model.Identity
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("i.user_id = ?", userID).
		OrderExpr("i.created_at DESC, i.id DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domainauth.Identity, len(ms))
	for i := range ms {
		out[i] = mapIdentityToDomain(&ms[i])
	}
	return out, nil
}

func (r *IdentityRepository) Delete(ctx context.Context, projectID, id string) error {
	if strings.TrimSpace(id) == "" {
		return domainauth.ErrIdentityIDRequired
	}
	conn, sch, expr, err := Scoped(ctx, r.db, projectID, identityTable, "i")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.Identity)(nil)).ModelTableExpr(expr, sch).
		Where("i.id = ?", id).
		Exec(ctx)
	return err
}

func mapIdentityUniqueError(err error) error {
	if isUniqueViolation(err) {
		return domainauth.ErrIdentityAlreadyLinked
	}
	return err
}

func mapIdentityToModel(in *domainauth.Identity) (*model.Identity, error) {
	now := time.Now()
	created := in.CreatedAt
	if created.IsZero() {
		created = now
	}
	updated := in.UpdatedAt
	if updated.IsZero() {
		updated = now
	}
	data, err := marshalJSONCol(in.ProviderData, jsonEmptyObject)
	if err != nil {
		return nil, err
	}
	return &model.Identity{
		ID:            in.ID,
		UserID:        in.UserID,
		Provider:      strings.TrimSpace(in.Provider),
		ProviderUID:   strings.TrimSpace(in.ProviderUID),
		ProviderEmail: strings.TrimSpace(in.ProviderEmail),
		ProviderData:  data,
		ExpireAt:      in.ExpireAt,
		CreatedAt:     created,
		UpdatedAt:     updated,
	}, nil
}

func mapIdentityToDomain(m *model.Identity) *domainauth.Identity {
	return &domainauth.Identity{
		ID:            m.ID,
		UserID:        m.UserID,
		Provider:      m.Provider,
		ProviderUID:   m.ProviderUID,
		ProviderEmail: m.ProviderEmail,
		ProviderData:  unmarshalAnyMap(m.ProviderData),
		ExpireAt:      m.ExpireAt,
		CreatedAt:     m.CreatedAt,
		UpdatedAt:     m.UpdatedAt,
	}
}
