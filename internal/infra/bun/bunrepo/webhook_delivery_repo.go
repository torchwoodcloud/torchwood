package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	domainwebhooks "github.com/torchwoodcloud/torchwood/internal/domain/webhooks"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

// webhookDeliveryRepo 是 domainwebhooks.DeliveryRepo 的 bun 适配器（迁移
// 000027）。webhook_deliveries 位于项目 schema，方法均按 project 收窄；
// 到期领取走 ClaimDue CAS（先 CAS 后投递红线，与 ClaimDueCron 同款）。
type webhookDeliveryRepo struct {
	db *clients.Database
}

// NewWebhookDeliveryRepository 构造投递仓储（Wire）。
func NewWebhookDeliveryRepository(db *clients.Database) domainwebhooks.DeliveryRepo {
	return &webhookDeliveryRepo{db: db}
}

func (r *webhookDeliveryRepo) scoped(ctx context.Context, projectID, table, alias string) (bun.IDB, bun.Ident, string, error) {
	return Scoped(ctx, r.db, projectID, table, alias)
}

// CreateDeliveries 批量插入：(webhook_id, event_id) 唯一索引冲突静默跳过
// （at-least-once 去重吸收），逐行独立 INSERT 保证单行坏行不拖垮整批。
func (r *webhookDeliveryRepo) CreateDeliveries(ctx context.Context, items []*domainwebhooks.Delivery) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}
	projectID := items[0].ProjectID
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return 0, err
	}
	inserted := 0
	for i := range items {
		m := mapDeliveryToModel(items[i])
		res, err := conn.NewInsert().Model(m).ModelTableExpr(expr, sch).
			On("CONFLICT (webhook_id, event_id) DO NOTHING").
			Exec(ctx)
		if err != nil {
			return inserted, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return inserted, err
		}
		inserted += int(n)
	}
	return inserted, nil
}

func (r *webhookDeliveryRepo) GetDelivery(ctx context.Context, projectID, webhookID, deliveryID string) (*domainwebhooks.Delivery, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return nil, err
	}
	m := new(model.WebhookDelivery)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("wd.project_id = ?", projectID).
		Where("wd.webhook_id = ?", webhookID).
		Where("wd.id = ?", deliveryID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domainwebhooks.ErrNotFound
		}
		return nil, err
	}
	return mapDeliveryToDomain(m), nil
}

func (r *webhookDeliveryRepo) ListDeliveries(ctx context.Context, projectID, webhookID, status string, limit, offset int) (domainwebhooks.DeliveryListPage, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return domainwebhooks.DeliveryListPage{}, err
	}
	q := conn.NewSelect().Model((*model.WebhookDelivery)(nil)).ModelTableExpr(expr, sch).
		Where("wd.project_id = ?", projectID).
		Where("wd.webhook_id = ?", webhookID)
	if status != "" {
		q = q.Where("wd.status = ?", status)
	}
	total, err := q.Count(ctx)
	if err != nil {
		return domainwebhooks.DeliveryListPage{}, err
	}
	if limit <= 0 {
		limit = 50
	}
	var ms []model.WebhookDelivery
	err = q.Order("created_at DESC").Limit(limit).Offset(offset).Scan(ctx, &ms)
	if err != nil {
		return domainwebhooks.DeliveryListPage{}, err
	}
	items := make([]domainwebhooks.Delivery, 0, len(ms))
	for i := range ms {
		items = append(items, *mapDeliveryToDomain(&ms[i]))
	}
	return domainwebhooks.DeliveryListPage{Items: items, Total: total}, nil
}

// ClaimDue 单项目内原子领取到期投递：候选扫描（无锁，命中
// webhook_deliveries_due partial 索引）后逐条 CAS 推进——
// `UPDATE attempts/next_attempt_at/updated_at WHERE id AND attempts=旧 AND
// next_attempt_at=旧` 判 rows=1，多实例并发只有赢家。**先 CAS 后投递**：
// 领取即按梯度乐观推进，投递中崩溃的行退避后自愈（不热循环）。终态判定
// （达 MaxAttempts / 越 RetryWindow）经 plan 回调由领域层给出，CAS 直接置
// failed（DLQ）且不返回该行。
func (r *webhookDeliveryRepo) ClaimDue(ctx context.Context, projectID string, now time.Time, limit int, plan func(d *domainwebhooks.Delivery) domainwebhooks.AttemptPlan) ([]domainwebhooks.DeliveryClaim, error) {
	if limit <= 0 {
		return nil, nil
	}
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return nil, err
	}
	var ms []model.WebhookDelivery
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("wd.project_id = ?", projectID).
		Where("wd.status = ?", domainwebhooks.DeliveryStatusPending).
		Where("wd.next_attempt_at <= ?", now).
		Order("wd.next_attempt_at").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	var claims []domainwebhooks.DeliveryClaim
	for i := range ms {
		row := ms[i]
		d := mapDeliveryToDomain(&row)
		p := plan(d)
		if p.Terminal {
			// 梯度耗尽 / 越窗：CAS 终态化（DLQ），不返回（不再尝试）。
			if err := r.MarkFailed(ctx, projectID, row.ID, now, 0, "retry policy exhausted"); err != nil {
				return nil, err
			}
			continue
		}
		res, err := conn.NewUpdate().Model((*model.WebhookDelivery)(nil)).
			ModelTableExpr(expr, sch).
			Set("attempts = ?", row.Attempts+1).
			Set("next_attempt_at = ?", p.Next).
			Set("updated_at = ?", now).
			Where("wd.project_id = ?", projectID).
			Where("wd.id = ?", row.ID).
			Where("wd.attempts = ?", row.Attempts).             // CAS：仅当仍为原计数
			Where("wd.next_attempt_at = ?", row.NextAttemptAt). // CAS：仅当仍为原到期值
			Exec(ctx)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 || !p.Attempt {
			continue // CAS 输家（并发赢家已推进）
		}
		claim := domainwebhooks.DeliveryClaim{Delivery: *d}
		claim.Attempts = row.Attempts + 1
		claim.NextAttemptAt = p.Next
		claims = append(claims, claim)
	}
	return claims, nil
}

// MarkDelivered 投递成功收口（仅 pending 行可收口——与重置/领取的竞争由
// status 条件化解）。列白名单：status/next_attempt_at/last_response_code/
// last_error/updated_at。
func (r *webhookDeliveryRepo) MarkDelivered(ctx context.Context, projectID, deliveryID string, now time.Time, responseCode int) error {
	return r.updateOutcome(ctx, projectID, deliveryID, now, domainwebhooks.DeliveryStatusDelivered, responseCode, "")
}

// MarkFailed 终态失败收口（梯度耗尽/越窗/载荷损坏；仅 pending 行可收口）。
func (r *webhookDeliveryRepo) MarkFailed(ctx context.Context, projectID, deliveryID string, now time.Time, responseCode int, lastErr string) error {
	return r.updateOutcome(ctx, projectID, deliveryID, now, domainwebhooks.DeliveryStatusFailed, responseCode, lastErr)
}

func (r *webhookDeliveryRepo) updateOutcome(ctx context.Context, projectID, deliveryID string, now time.Time, status string, responseCode int, lastErr string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model((*model.WebhookDelivery)(nil)).
		ModelTableExpr(expr, sch).
		Set("status = ?", status).
		Set("next_attempt_at = ?", now).
		Set("last_response_code = ?", responseCode).
		Set("last_error = ?", lastErr).
		Set("updated_at = ?", now).
		Where("wd.project_id = ?", projectID).
		Where("wd.id = ?", deliveryID).
		Where("wd.status = ?", domainwebhooks.DeliveryStatusPending). // 仅 pending 可收口
		Exec(ctx)
	return err
}

// RecordAttemptError 记录一次非终态失败的审计面（next_attempt_at 已由领取
// 推进；仅补 last_response_code/last_error——列白名单同 updateOutcome 去掉
// status/next_attempt_at）。
func (r *webhookDeliveryRepo) RecordAttemptError(ctx context.Context, projectID, deliveryID string, now time.Time, responseCode int, lastErr string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model((*model.WebhookDelivery)(nil)).
		ModelTableExpr(expr, sch).
		Set("last_response_code = ?", responseCode).
		Set("last_error = ?", lastErr).
		Set("updated_at = ?", now).
		Where("wd.project_id = ?", projectID).
		Where("wd.id = ?", deliveryID).
		Exec(ctx)
	return err
}

// ResetForRedeliver 手动重投重置：终态行 → pending、attempts=0、
// next_attempt_at=now、清错误（列白名单六列）。pending 行拒绝（返回 false）。
func (r *webhookDeliveryRepo) ResetForRedeliver(ctx context.Context, projectID, webhookID, deliveryID string, now time.Time) (bool, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd")
	if err != nil {
		return false, err
	}
	res, err := conn.NewUpdate().Model((*model.WebhookDelivery)(nil)).
		ModelTableExpr(expr, sch).
		Set("status = ?", domainwebhooks.DeliveryStatusPending).
		Set("attempts = ?", 0).
		Set("next_attempt_at = ?", now).
		Set("last_response_code = ?", nil).
		Set("last_error = ?", "").
		Set("updated_at = ?", now).
		Where("wd.project_id = ?", projectID).
		Where("wd.webhook_id = ?", webhookID).
		Where("wd.id = ?", deliveryID).
		Where("wd.status IN (?, ?)",
			domainwebhooks.DeliveryStatusDelivered, domainwebhooks.DeliveryStatusFailed).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// PruneTerminal 清理超保留窗的终态行（delivered/failed；pending 行由重试窗
// 终态化后自然进入本口径）。批量经 ctid 子选（bun 的 Postgres 方言不支持
// DELETE ... LIMIT；与 PruneOldExecutions 的受限删除同款原生 SQL 形态）。
func (r *webhookDeliveryRepo) PruneTerminal(ctx context.Context, projectID string, cutoff time.Time, limit int) (int64, error) {
	if _, _, _, err := r.scoped(ctx, projectID, "webhook_deliveries", "wd"); err != nil {
		return 0, err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return 0, err
	}
	res, err := r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.webhook_deliveries
WHERE ctid IN (
  SELECT ctid FROM %s.webhook_deliveries
  WHERE project_id = ?
    AND status IN (?, ?)
    AND created_at < ?
  LIMIT ?
)`, quoted, quoted),
		projectID,
		domainwebhooks.DeliveryStatusDelivered,
		domainwebhooks.DeliveryStatusFailed,
		cutoff, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func mapDeliveryToModel(d *domainwebhooks.Delivery) *model.WebhookDelivery {
	return &model.WebhookDelivery{
		ID:               d.ID,
		ProjectID:        d.ProjectID,
		WebhookID:        d.WebhookID,
		EventID:          d.EventID,
		EventType:        d.EventType,
		Seq:              d.Seq,
		Payload:          d.Payload,
		Status:           d.Status,
		Attempts:         d.Attempts,
		NextAttemptAt:    d.NextAttemptAt,
		LastResponseCode: d.LastResponseCode,
		LastError:        d.LastError,
		CreatedAt:        d.CreatedAt,
		UpdatedAt:        d.UpdatedAt,
	}
}

func mapDeliveryToDomain(m *model.WebhookDelivery) *domainwebhooks.Delivery {
	return &domainwebhooks.Delivery{
		ID:               m.ID,
		ProjectID:        m.ProjectID,
		WebhookID:        m.WebhookID,
		EventID:          m.EventID,
		EventType:        m.EventType,
		Seq:              m.Seq,
		Payload:          m.Payload,
		Status:           m.Status,
		Attempts:         m.Attempts,
		NextAttemptAt:    m.NextAttemptAt,
		LastResponseCode: m.LastResponseCode,
		LastError:        m.LastError,
		CreatedAt:        m.CreatedAt,
		UpdatedAt:        m.UpdatedAt,
	}
}
