package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/pkg/config"
	"github.com/torchwoodcloud/torchwood/pkg/secretbox"
	"github.com/uptrace/bun"
)

type oauthProviderRepo struct {
	db            *clients.Database
	encryptionKey string
	legacyKey     string
}

func NewOAuthProviderRepository(db *clients.Database, cfg *config.AppConfig) projects.OAuthProviderRepository {
	key, _ := config.EncryptionSecret(cfg)
	// 旧版本直接用 jwt.secret 原文加密（无域分离）；配置独立 encryption_key
	// 后存量密文靠 legacyKey 读兼容（W-I 迁移期）。
	legacy := ""
	if cfg != nil && cfg.GetSecurity() != nil && cfg.GetSecurity().GetJwt() != nil {
		legacy = cfg.GetSecurity().GetJwt().GetSecret()
	}
	return &oauthProviderRepo{db: db, encryptionKey: key, legacyKey: legacy}
}

func (r *oauthProviderRepo) scoped(ctx context.Context, projectID string) (bun.IDB, bun.Ident, string, error) {
	return Scoped(ctx, r.db, projectID, "project_oauth_providers", "pop")
}

func (r *oauthProviderRepo) GetOAuthProvider(ctx context.Context, projectID, provider string) (*projects.OAuthProvider, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID)
	if err != nil {
		return nil, err
	}
	m := new(model.ProjectOAuthProvider)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("project_id = ? AND provider = ?", projectID, provider).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapOAuthProvider(m, r.encryptionKey, r.legacyKey)
}

func (r *oauthProviderRepo) ListOAuthProviders(ctx context.Context, projectID string) ([]projects.OAuthProvider, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID)
	if err != nil {
		return nil, err
	}
	var rows []model.ProjectOAuthProvider
	err = conn.NewSelect().Model(&rows).ModelTableExpr(expr, sch).
		Where("project_id = ?", projectID).
		Order("provider ASC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]projects.OAuthProvider, len(rows))
	for i := range rows {
		mapped, err := mapOAuthProvider(&rows[i], r.encryptionKey, r.legacyKey)
		if err != nil {
			return nil, err
		}
		out[i] = *mapped
	}
	return out, nil
}

func (r *oauthProviderRepo) UpsertOAuthProvider(ctx context.Context, cfg *projects.OAuthProvider) error {
	if cfg == nil {
		return errors.New("oauth provider is nil")
	}
	secret := cfg.ClientSecret
	if secret != "" {
		enc, err := secretbox.Encrypt(secret, r.encryptionKey)
		if err != nil {
			return err
		}
		secret = enc
	}
	now := time.Now()
	m := &model.ProjectOAuthProvider{
		ProjectID:    cfg.ProjectID,
		Provider:     cfg.Provider,
		Enabled:      cfg.Enabled,
		ClientID:     cfg.ClientID,
		ClientSecret: secret,
		Scopes:       append([]string(nil), cfg.Scopes...),
		UpdatedAt:    now,
	}
	conn, sch, expr, err := r.scoped(ctx, cfg.ProjectID)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).
		On("CONFLICT (project_id, provider) DO UPDATE").
		Set("enabled = EXCLUDED.enabled").
		Set("client_id = EXCLUDED.client_id").
		Set("client_secret = EXCLUDED.client_secret").
		Set("scopes = EXCLUDED.scopes").
		Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

func (r *oauthProviderRepo) DeleteOAuthProvider(ctx context.Context, projectID, provider string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID)
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.ProjectOAuthProvider)(nil)).
		ModelTableExpr(expr, sch).
		Where("project_id = ? AND provider = ?", projectID, provider).
		Exec(ctx)
	return err
}

func mapOAuthProvider(m *model.ProjectOAuthProvider, encryptionKey, legacyOAuthKey string) (*projects.OAuthProvider, error) {
	if m == nil {
		return nil, nil
	}
	secret, err := secretbox.Decrypt(m.ClientSecret, encryptionKey)
	if err != nil {
		secret, err = secretbox.Decrypt(m.ClientSecret, legacyOAuthKey)
		if err != nil {
			return nil, err
		}
	}
	return &projects.OAuthProvider{
		ProjectID:    m.ProjectID,
		Provider:     m.Provider,
		Enabled:      m.Enabled,
		ClientID:     m.ClientID,
		ClientSecret: secret,
		Scopes:       append([]string(nil), m.Scopes...),
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}, nil
}
