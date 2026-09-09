package bunrepo

import (
	"context"
	"database/sql"
	"errors"

	"github.com/torchwoodcloud/torchwood/internal/domain/payments"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type providerIndexRepo struct {
	db *clients.Database
}

func NewProviderIndexRepository(db *clients.Database) payments.ProviderIndexRepo {
	return &providerIndexRepo{db: db}
}

func (r *providerIndexRepo) Lookup(ctx context.Context, provider, kind, providerRef string) (string, error) {
	if provider == "" || kind == "" || providerRef == "" {
		return "", nil
	}
	m := new(model.ProviderResourceIndex)
	err := r.db.Conn(ctx).NewSelect().Model(m).
		Where("pri.provider = ?", provider).
		Where("pri.kind = ?", kind).
		Where("pri.provider_ref = ?", providerRef).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return m.ProjectID, nil
}

func (r *providerIndexRepo) Upsert(ctx context.Context, provider, kind, providerRef, projectID string) error {
	if provider == "" || kind == "" || providerRef == "" || projectID == "" {
		return status.Error(codes.InvalidArgument, "provider index requires provider, kind, ref and project_id")
	}
	res, err := r.db.Conn(ctx).NewInsert().Model(&model.ProviderResourceIndex{
		Provider:    provider,
		Kind:        kind,
		ProviderRef: providerRef,
		ProjectID:   projectID,
	}).On("CONFLICT (provider, kind, provider_ref) DO NOTHING").Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	existing, err := r.Lookup(ctx, provider, kind, providerRef)
	if err != nil {
		return err
	}
	if existing != "" && existing != projectID {
		return status.Error(codes.PermissionDenied, "provider resource already bound to another project")
	}
	return nil
}
