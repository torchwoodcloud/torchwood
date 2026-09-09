package bunrepo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

// triggerRepo 是 domainfunctions.TriggerRepo 的 bun 适配器（P1 触发器模块，
// 迁移 000015）。function_triggers 位于项目 schema（与其余 functions* 静态表
// 同库），方法均按 (project, function) 收窄。
type triggerRepo struct {
	db *clients.Database
}

func NewFunctionTriggerRepository(db *clients.Database) domainfunctions.TriggerRepo {
	return &triggerRepo{db: db}
}

func (r *triggerRepo) scoped(ctx context.Context, projectID, table, alias string) (bun.IDB, bun.Ident, string, error) {
	return Scoped(ctx, r.db, projectID, table, alias)
}

func (r *triggerRepo) CreateTrigger(ctx context.Context, t *domainfunctions.Trigger) error {
	conn, sch, expr, err := r.scoped(ctx, t.ProjectID, "function_triggers", "ft")
	if err != nil {
		return err
	}
	m, err := mapTriggerToModel(t)
	if err != nil {
		return err
	}
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *triggerRepo) GetTrigger(ctx context.Context, projectID, functionID, triggerID string) (*domainfunctions.Trigger, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_triggers", "ft")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionTrigger)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("ft.project_id = ?", projectID).
		Where("ft.function_id = ?", functionID).
		Where("ft.id = ?", triggerID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapTriggerToDomain(m)
}

func (r *triggerRepo) ListTriggers(ctx context.Context, projectID, functionID string) ([]domainfunctions.Trigger, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_triggers", "ft")
	if err != nil {
		return nil, err
	}
	var ms []model.FunctionTrigger
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("ft.project_id = ?", projectID).
		Where("ft.function_id = ?", functionID).
		Order("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domainfunctions.Trigger, 0, len(ms))
	for i := range ms {
		d, err := mapTriggerToDomain(&ms[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, nil
}

func (r *triggerRepo) UpdateTrigger(ctx context.Context, t *domainfunctions.Trigger) error {
	conn, sch, expr, err := r.scoped(ctx, t.ProjectID, "function_triggers", "ft")
	if err != nil {
		return err
	}
	m, err := mapTriggerToModel(t)
	if err != nil {
		return err
	}
	// 列白名单（bun 更新写规范）：id/project_id/function_id/type/created_at
	// 不可变；token/config/enabled/next_run_at 为可变列（轮换 token、改配置、
	// 启停、cron 推进）。
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("token", "config", "enabled", "next_run_at", "updated_at").
		WherePK().
		Where("ft.project_id = ?", t.ProjectID).
		Where("ft.function_id = ?", t.FunctionID).
		Exec(ctx)
	return err
}

func (r *triggerRepo) DeleteTrigger(ctx context.Context, projectID, functionID, triggerID string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_triggers", "ft")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.FunctionTrigger)(nil)).ModelTableExpr(expr, sch).
		Where("project_id = ?", projectID).
		Where("function_id = ?", functionID).
		Where("id = ?", triggerID).
		Exec(ctx)
	return err
}

func (r *triggerRepo) GetTriggerByToken(ctx context.Context, projectID, token string) (*domainfunctions.Trigger, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_triggers", "ft")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionTrigger)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("ft.project_id = ?", projectID).
		Where("ft.token = ?", token).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapTriggerToDomain(m)
}

// ClaimDueCron 单项目内原子领取到期 cron 触发器：候选扫描（无锁，命中
// function_triggers_cron_due partial 索引）后逐条 CAS 推进——
// `UPDATE ... SET next_run_at=新 WHERE id=$1 AND next_run_at=旧` 判 rows=1，
// 多实例并发只有赢家（先 CAS 后入队红线：先入队后 CAS 在多实例下双入队，
// 执行行 queued→building 的 CAS 防不了两条不同 execution）。
// 推进计划（misfire 语义、cron 解析）经 next 回调由领域层给出。
func (r *triggerRepo) ClaimDueCron(ctx context.Context, projectID string, now time.Time, limit int, next domainfunctions.CronNextFunc) ([]domainfunctions.CronClaim, error) {
	if limit <= 0 {
		return nil, nil
	}
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_triggers", "ft")
	if err != nil {
		return nil, err
	}
	var ms []model.FunctionTrigger
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("ft.project_id = ?", projectID).
		Where("ft.enabled = ?", true).
		Where("ft.type = ?", domainfunctions.TriggerTypeCron).
		Where("ft.next_run_at <= ?", now).
		Order("ft.next_run_at").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	var claims []domainfunctions.CronClaim
	for i := range ms {
		row := &ms[i]
		var cfg domainfunctions.TriggerConfig
		if err := json.Unmarshal(row.Config, &cfg); err != nil {
			return nil, fmt.Errorf("decode trigger %s config: %w", row.ID, err)
		}
		due := row.NextRunAt
		plan, err := next(domainfunctions.CronNextInput{Due: due, Now: now, Expr: cfg.Expr, Misfire: cfg.Misfire})
		if err != nil {
			// 坏表达式（理论不可达：创建期已校验）：跳过该行并不推进，
			// 避免单行坏配置阻塞整批；下轮扫描将再次命中。
			continue
		}
		res, err := conn.NewUpdate().Model((*model.FunctionTrigger)(nil)).
			ModelTableExpr(expr, sch).
			Set("next_run_at = ?", plan.Next).
			Set("updated_at = ?", now).
			Where("ft.project_id = ?", projectID).
			Where("ft.id = ?", row.ID).
			Where("ft.next_run_at = ?", due). // CAS：仅当仍为原到期值
			Exec(ctx)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n == 0 || !plan.Run {
			continue // CAS 输家（并发赢家已推进）或 skip 模式 misfire：只推进不补跑。
		}
		claims = append(claims, domainfunctions.CronClaim{
			ProjectID:    projectID,
			FunctionID:   row.FunctionID,
			TriggerID:    row.ID,
			ScheduledFor: due,
		})
	}
	return claims, nil
}

func mapTriggerToModel(t *domainfunctions.Trigger) (*model.FunctionTrigger, error) {
	cfg, err := json.Marshal(t.Config)
	if err != nil {
		return nil, err
	}
	m := &model.FunctionTrigger{
		ID:         t.ID,
		ProjectID:  t.ProjectID,
		FunctionID: t.FunctionID,
		Type:       t.Type,
		Config:     cfg,
		Token:      t.Token,
		Enabled:    t.Enabled,
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
	if t.NextRunAt != nil {
		m.NextRunAt = *t.NextRunAt
	}
	return m, nil
}

func mapTriggerToDomain(m *model.FunctionTrigger) (*domainfunctions.Trigger, error) {
	var cfg domainfunctions.TriggerConfig
	if len(m.Config) > 0 {
		if err := json.Unmarshal(m.Config, &cfg); err != nil {
			return nil, fmt.Errorf("decode trigger %s config: %w", m.ID, err)
		}
	}
	t := &domainfunctions.Trigger{
		ID:         m.ID,
		ProjectID:  m.ProjectID,
		FunctionID: m.FunctionID,
		Type:       m.Type,
		Config:     cfg,
		Token:      m.Token,
		Enabled:    m.Enabled,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
	}
	if !m.NextRunAt.IsZero() {
		next := m.NextRunAt
		t.NextRunAt = &next
	}
	return t, nil
}
