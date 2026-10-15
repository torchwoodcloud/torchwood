package bunrepo

import (
	"context"
	"database/sql"
	"errors"

	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

// webhookRepo 是 domainwebhooks.WebhookRepo 的 bun 适配器（迁移 000027）。
// webhook_subscriptions 位于项目 schema，方法均按 project 收窄。
type webhookRepo struct {
	db *clients.Database
}

// NewWebhookRepository 构造订阅仓储（Wire）。
func NewWebhookRepository(db *clients.Database) domainwebhooks.WebhookRepo {
	return &webhookRepo{db: db}
}

func (r *webhookRepo) scoped(ctx context.Context, projectID, table, alias string) (bun.IDB, bun.Ident, string, error) {
	return Scoped(ctx, r.db, projectID, table, alias)
}

func (r *webhookRepo) CreateWebhook(ctx context.Context, w *domainwebhooks.Webhook) error {
	conn, sch, expr, err := r.scoped(ctx, w.ProjectID, "webhook_subscriptions", "ws")
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(mapWebhookToModel(w)).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *webhookRepo) GetWebhook(ctx context.Context, projectID, webhookID string) (*domainwebhooks.Webhook, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_subscriptions", "ws")
	if err != nil {
		return nil, err
	}
	m := new(model.WebhookSubscription)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("ws.project_id = ?", projectID).
		Where("ws.id = ?", webhookID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domainwebhooks.ErrNotFound
		}
		return nil, err
	}
	return mapWebhookToDomain(m), nil
}

func (r *webhookRepo) ListWebhooks(ctx context.Context, projectID string) ([]domainwebhooks.Webhook, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_subscriptions", "ws")
	if err != nil {
		return nil, err
	}
	var ms []model.WebhookSubscription
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("ws.project_id = ?", projectID).
		Order("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domainwebhooks.Webhook, 0, len(ms))
	for i := range ms {
		out = append(out, *mapWebhookToDomain(&ms[i]))
	}
	return out, nil
}

// UpdateWebhook 全量更新可变列。列白名单（bun 更新写规范）：id/project_id/
// created_at 不可变；url/events/status/updated_at + 密钥四列（previous_
// secret_enc/secret_rotated_at/secret_enc/secret_fingerprint，轮换路径复用）
// 为可变列。
func (r *webhookRepo) UpdateWebhook(ctx context.Context, w *domainwebhooks.Webhook) error {
	conn, sch, expr, err := r.scoped(ctx, w.ProjectID, "webhook_subscriptions", "ws")
	if err != nil {
		return err
	}
	m := mapWebhookToModel(w)
	res, err := conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("url", "events", "status", "secret_enc", "previous_secret_enc",
			"secret_rotated_at", "secret_fingerprint", "updated_at").
		WherePK().
		Where("ws.project_id = ?", w.ProjectID).
		Exec(ctx)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domainwebhooks.ErrNotFound
	}
	return nil
}

func (r *webhookRepo) DeleteWebhook(ctx context.Context, projectID, webhookID string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_subscriptions", "ws")
	if err != nil {
		return err
	}
	res, err := conn.NewDelete().Model((*model.WebhookSubscription)(nil)).ModelTableExpr(expr, sch).
		Where("project_id = ?", projectID).
		Where("id = ?", webhookID).
		Exec(ctx)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return domainwebhooks.ErrNotFound
	}
	return nil
}

func (r *webhookRepo) CountWebhooks(ctx context.Context, projectID string) (int, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_subscriptions", "ws")
	if err != nil {
		return 0, err
	}
	return conn.NewSelect().Model((*model.WebhookSubscription)(nil)).ModelTableExpr(expr, sch).
		Where("ws.project_id = ?", projectID).
		Count(ctx)
}

// ListEnabledWebhooks 项目内启用的订阅（worker 订阅匹配器周期快照扫描；
// 走 webhook_subscriptions_enabled_scan partial 索引）。
func (r *webhookRepo) ListEnabledWebhooks(ctx context.Context, projectID string) ([]domainwebhooks.Webhook, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_subscriptions", "ws")
	if err != nil {
		return nil, err
	}
	var ms []model.WebhookSubscription
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("ws.project_id = ?", projectID).
		Where("ws.status = ?", domainwebhooks.StatusEnabled).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domainwebhooks.Webhook, 0, len(ms))
	for i := range ms {
		out = append(out, *mapWebhookToDomain(&ms[i]))
	}
	return out, nil
}

func mapWebhookToModel(w *domainwebhooks.Webhook) *model.WebhookSubscription {
	m := &model.WebhookSubscription{
		ID:                w.ID,
		ProjectID:         w.ProjectID,
		URL:               w.URL,
		Events:            w.Events,
		Status:            w.Status,
		SecretEnc:         w.SecretEnc,
		PreviousSecretEnc: w.PreviousSecretEnc,
		SecretRotatedAt:   w.SecretRotatedAt,
		SecretFingerprint: w.SecretFingerprint,
		CreatedAt:         w.CreatedAt,
		UpdatedAt:         w.UpdatedAt,
	}
	if m.Events == nil {
		m.Events = []string{}
	}
	return m
}

func mapWebhookToDomain(m *model.WebhookSubscription) *domainwebhooks.Webhook {
	return &domainwebhooks.Webhook{
		ID:                m.ID,
		ProjectID:         m.ProjectID,
		URL:               m.URL,
		Events:            m.Events,
		Status:            m.Status,
		SecretEnc:         m.SecretEnc,
		PreviousSecretEnc: m.PreviousSecretEnc,
		SecretRotatedAt:   m.SecretRotatedAt,
		SecretFingerprint: m.SecretFingerprint,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}
