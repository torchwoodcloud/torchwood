package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/uptrace/bun"
)

type functionRepo struct {
	db *clients.Database
}

func NewFunctionRepository(db *clients.Database) domainfunctions.FunctionRepo {
	return &functionRepo{db: db}
}

func (r *functionRepo) scoped(ctx context.Context, projectID, table, alias string) (bun.IDB, bun.Ident, string, error) {
	return Scoped(ctx, r.db, projectID, table, alias)
}

func (r *functionRepo) CreateFunction(ctx context.Context, fn *domainfunctions.Function) error {
	conn, sch, expr, err := r.scoped(ctx, fn.ProjectID, "functions", "f")
	if err != nil {
		return err
	}
	m := mapFunctionToModel(fn)
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *functionRepo) GetFunction(ctx context.Context, projectID, functionID string) (*domainfunctions.Function, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "functions", "f")
	if err != nil {
		return nil, err
	}
	m := new(model.Function)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("f.project_id = ?", projectID).
		Where("f.id = ?", functionID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapFunctionToDomain(m), nil
}

func (r *functionRepo) ListFunctions(ctx context.Context, projectID string) ([]domainfunctions.Function, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "functions", "f")
	if err != nil {
		return nil, err
	}
	var ms []model.Function
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("f.project_id = ?", projectID).
		Order("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domainfunctions.Function, len(ms))
	for i := range ms {
		out[i] = *mapFunctionToDomain(&ms[i])
	}
	return out, nil
}

func (r *functionRepo) UpdateFunction(ctx context.Context, fn *domainfunctions.Function) error {
	conn, sch, expr, err := r.scoped(ctx, fn.ProjectID, "functions", "f")
	if err != nil {
		return err
	}
	m := mapFunctionToModel(fn)
	// 列白名单（bun 更新写规范）：id/project_id/created_at 不可变。
	// declared_scopes 为 P0 执行身份可变列（SetFunctionScopes 全量替换）；
	// target 列族为 v4 可变列（target_url 经 UpdateFunction；密钥列经
	// RotateFunctionSecret——组合根共用本全模型写，密钥改动只经 app 层轮换
	// 用例进入）；client 四列为 P2 客户端调用面策略列。
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("name", "timeout_seconds", "enabled", "declared_scopes",
			"target_url", "target_secret_enc", "target_previous_secret_enc",
			"target_secret_rotated_at", "target_secret_fingerprint",
			"client_callable", "client_anonymous_allowed", "client_per_user_limit", "client_limit_window",
			"updated_at").
		WherePK().
		Where("f.project_id = ?", fn.ProjectID).
		Exec(ctx)
	return err
}

func (r *functionRepo) DeleteFunction(ctx context.Context, projectID, functionID string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "functions", "f")
	if err != nil {
		return err
	}
	_, err = conn.NewDelete().Model((*model.Function)(nil)).ModelTableExpr(expr, sch).
		Where("project_id = ?", projectID).
		Where("id = ?", functionID).
		Exec(ctx)
	return err
}

func (r *functionRepo) CreateInvocation(ctx context.Context, e *domainfunctions.InvocationRecord) error {
	conn, sch, expr, err := r.scoped(ctx, e.ProjectID, "function_invocations", "fi")
	if err != nil {
		return err
	}
	m := mapInvocationToModel(e)
	if _, err := conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx); err != nil {
		// P2 幂等（设计 §4）：partial 唯一索引 (project, function, user, key)
		// 的并发冲突归一为领域哨兵错误——调用方（client invoke）回读既有行
		// 原样返回。本表其余唯一键只有主键（服务端生成 UUID，不会冲突），
		// 23505 在此只会来自幂等索引。
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %v", domainfunctions.ErrInvocationIdempotencyConflict, err)
		}
		return err
	}
	return nil
}

// GetInvocationByIdempotencyKey 按 (project, function, user, key) 查既有调用
// （幂等冲突回读原样返回）。
func (r *functionRepo) GetInvocationByIdempotencyKey(ctx context.Context, projectID, functionID, userID, key string) (*domainfunctions.InvocationRecord, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_invocations", "fi")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionInvocation)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("fi.project_id = ?", projectID).
		Where("fi.function_id = ?", functionID).
		Where("fi.invoking_user_id = ?", userID).
		Where("fi.client_idempotency_key = ?", key).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapInvocationToDomain(m), nil
}

// CountClientInvocations 统计窗口起点以来该用户的 client 来源调用数
// （限频 DB 降级路径；partial 索引 function_invocations_client_quota 支撑）。
func (r *functionRepo) CountClientInvocations(ctx context.Context, projectID, functionID, userID string, since time.Time) (int, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_invocations", "fi")
	if err != nil {
		return 0, err
	}
	n, err := conn.NewSelect().Model((*model.FunctionInvocation)(nil)).ModelTableExpr(expr, sch).
		Where("fi.project_id = ?", projectID).
		Where("fi.function_id = ?", functionID).
		Where("fi.invoking_user_id = ?", userID).
		Where("fi.trigger_source = ?", domainfunctions.TriggerSourceClient).
		Where("fi.created_at >= ?", since).
		Count(ctx)
	return int(n), err
}

func (r *functionRepo) GetInvocation(ctx context.Context, projectID, functionID, invocationID string) (*domainfunctions.InvocationRecord, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_invocations", "fi")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionInvocation)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("fi.project_id = ?", projectID).
		Where("fi.function_id = ?", functionID).
		Where("fi.id = ?", invocationID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapInvocationToDomain(m), nil
}

func (r *functionRepo) ListInvocations(ctx context.Context, projectID, functionID string, limit, offset int, f domainfunctions.InvocationListFilter) ([]domainfunctions.InvocationRecord, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_invocations", "fi")
	if err != nil {
		return nil, 0, err
	}
	cond := func(q *bun.SelectQuery) *bun.SelectQuery {
		q = q.Where("fi.project_id = ?", projectID).Where("fi.function_id = ?", functionID)
		if f.Status != "" {
			q = q.Where("fi.status = ?", f.Status)
		}
		if !f.CreatedAfter.IsZero() {
			q = q.Where("fi.created_at >= ?", f.CreatedAfter)
		}
		if !f.CreatedBefore.IsZero() {
			q = q.Where("fi.created_at <= ?", f.CreatedBefore)
		}
		return q
	}
	total, err := cond(conn.NewSelect().Model((*model.FunctionInvocation)(nil)).ModelTableExpr(expr, sch)).Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	var ms []model.FunctionInvocation
	err = cond(conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch)).
		Order("created_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]domainfunctions.InvocationRecord, len(ms))
	for i := range ms {
		out[i] = *mapInvocationToDomain(&ms[i])
	}
	return out, total, nil
}

func (r *functionRepo) UpdateInvocation(ctx context.Context, e *domainfunctions.InvocationRecord) error {
	conn, sch, expr, err := r.scoped(ctx, e.ProjectID, "function_invocations", "fi")
	if err != nil {
		return err
	}
	m := mapInvocationToModel(e)
	// 列白名单（bun 更新写规范）：id/function_id/project_id/created_at 与
	// INSERT 期写入列（trigger_source/source_ip/invoking_user_id/
	// client_idempotency_key）不可变——白名单有意不含。
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("status", "response", "response_truncated", "status_code", "duration_ms", "error", "updated_at").
		WherePK().
		Where("fi.project_id = ?", e.ProjectID).
		Exec(ctx)
	return err
}

func (r *functionRepo) TransitionInvocationStatus(ctx context.Context, projectID, functionID, invocationID, from, to string) (bool, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_invocations", "fi")
	if err != nil {
		return false, err
	}
	res, err := conn.NewUpdate().Model((*model.FunctionInvocation)(nil)).
		ModelTableExpr(expr, sch).
		Set("status = ?", to).
		Set("updated_at = ?", time.Now()).
		Where("fi.project_id = ?", projectID).
		Where("fi.function_id = ?", functionID).
		Where("fi.id = ?", invocationID).
		Where("fi.status = ?", from).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (r *functionRepo) FailInvocationIfActive(ctx context.Context, projectID, functionID, invocationID, reason string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_invocations", "fi")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model((*model.FunctionInvocation)(nil)).
		ModelTableExpr(expr, sch).
		Set("status = ?", domainfunctions.InvocationStatusFailed).
		Set("error = ?", reason).
		Set("updated_at = ?", time.Now()).
		Where("fi.project_id = ?", projectID).
		Where("fi.function_id = ?", functionID).
		Where("fi.id = ?", invocationID).
		Where("fi.status IN (?)", bun.List([]string{
			domainfunctions.InvocationStatusQueued,
			domainfunctions.InvocationStatusRunning,
		})).
		Exec(ctx)
	return err
}

func (r *functionRepo) RecoverOrphanInvocationsInProject(ctx context.Context, projectID string, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	if _, _, _, err := r.scoped(ctx, projectID, "function_invocations", "fi"); err != nil {
		return 0, err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return 0, err
	}
	// 孤儿恢复：staleAfter 按行判定（domainfunctions.OrphanGraceSeconds 宽限
	// 与迁移/app 层同源）：timeout_seconds 非空 = 该值 + 宽限（两写预占的
	// 行内快照），NULL（存量行）回退 olderThan（worker 传 1h）。
	res, err := r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
WITH cte AS (
  SELECT id FROM %s.function_invocations
  WHERE project_id = ?
    AND status IN (?, ?)
    AND (
      (timeout_seconds IS NOT NULL
        AND updated_at < NOW() - (timeout_seconds + ?) * INTERVAL '1 second')
      OR
      (timeout_seconds IS NULL AND updated_at < ?)
    )
  ORDER BY updated_at
  LIMIT ?
  FOR UPDATE SKIP LOCKED
)
UPDATE %s.function_invocations AS fi
SET status = ?, error = ?, updated_at = NOW()
FROM cte WHERE fi.id = cte.id
`, quoted, quoted),
		projectID,
		domainfunctions.InvocationStatusQueued,
		domainfunctions.InvocationStatusRunning,
		domainfunctions.OrphanGraceSeconds,
		olderThan,
		limit,
		domainfunctions.InvocationStatusFailed,
		"orphaned invocation recovered",
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *functionRepo) PruneOldInvocationsInProject(ctx context.Context, projectID, functionID string, keepRecent int) error {
	if keepRecent <= 0 {
		return nil
	}
	if _, _, _, err := r.scoped(ctx, projectID, "function_invocations", "fi"); err != nil {
		return err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	// 只清理终态记录：queued/running 可能仍对应在途队列消息，
	// 物理删除会导致消息被消费时静默丢弃（rec==nil → Ack）。
	// 条数式保留只作用于 server 面行（trigger_source = ''，Q7 保留分级）——
	// client/http/cron 来源行是限频 DB 降级的计数依据，按 48h 时间窗保留
	//（PruneTriggerInvocationsInProject），条数裁剪会造成少计超发。
	_, err = r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.function_invocations
WHERE project_id = ? AND function_id = ?
  AND trigger_source = ''
  AND status IN (?, ?)
  AND id NOT IN (
    SELECT id FROM (
      SELECT id FROM %s.function_invocations
      WHERE project_id = ? AND function_id = ? AND trigger_source = ''
      ORDER BY created_at DESC
      LIMIT ?
    ) keep
  )
`, quoted, quoted),
		projectID, functionID,
		domainfunctions.InvocationStatusCompleted,
		domainfunctions.InvocationStatusFailed,
		projectID, functionID, keepRecent)
	return err
}

// PruneTriggerInvocationsInProject 清理 client/http/cron 来源超过 olderThan 的
// 终态记录（Q7 保留分级，time-based：48h ≥ 2× 最长限频窗口 day，保证限频
// DB 降级在最长窗口内的计数行不被裁剪）。终态约束与 server 路径同源。
func (r *functionRepo) PruneTriggerInvocationsInProject(ctx context.Context, projectID, functionID string, olderThan time.Time) error {
	if _, _, _, err := r.scoped(ctx, projectID, "function_invocations", "fi"); err != nil {
		return err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	_, err = r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.function_invocations
WHERE project_id = ? AND function_id = ?
  AND status IN (?, ?)
  AND created_at < ?
  AND (
    trigger_source = ?
    OR trigger_source LIKE ?
    OR trigger_source LIKE ?
  )
`, quoted),
		projectID, functionID,
		domainfunctions.InvocationStatusCompleted,
		domainfunctions.InvocationStatusFailed,
		olderThan,
		domainfunctions.TriggerSourceClient,
		domainfunctions.TriggerTypeHTTP+":%",
		domainfunctions.TriggerTypeCron+":%")
	return err
}

func mapFunctionToModel(fn *domainfunctions.Function) *model.Function {
	// TEXT[] 列 NOT NULL：nil 切片经 bun 渲染为 NULL（列 DEFAULT 不生效），
	// 统一归一为空数组 '{}'（无平台访问权限的 fail-closed 形态）。
	declaredScopes := fn.DeclaredScopes
	if declaredScopes == nil {
		declaredScopes = []string{}
	}
	// client_limit_window 零值归一为平台默认 'day'（与迁移 000016 列 DEFAULT
	// 一致；UPDATE 是显式列白名单全模型写，空串会违反 CHECK）。
	limitWindow := fn.ClientLimitWindow
	if limitWindow == "" {
		limitWindow = domainfunctions.ClientLimitWindowDay
	}
	return &model.Function{
		ID:                      fn.ID,
		ProjectID:               fn.ProjectID,
		Name:                    fn.Name,
		TimeoutSeconds:          fn.TimeoutSeconds,
		Enabled:                 fn.Enabled,
		DeclaredScopes:          declaredScopes,
		TargetURL:               fn.TargetURL,
		TargetSecretEnc:         fn.TargetSecretEnc,
		TargetPreviousSecretEnc: fn.TargetPreviousSecretEnc,
		TargetSecretRotatedAt:   fn.TargetSecretRotatedAt,
		TargetSecretFingerprint: fn.TargetSecretFingerprint,
		ClientCallable:          fn.ClientCallable,
		ClientAnonymousAllowed:  fn.ClientAnonymousAllowed,
		ClientPerUserLimit:      fn.ClientPerUserLimit,
		ClientLimitWindow:       limitWindow,
		CreatedAt:               fn.CreatedAt,
		UpdatedAt:               fn.UpdatedAt,
	}
}

func mapFunctionToDomain(m *model.Function) *domainfunctions.Function {
	return &domainfunctions.Function{
		ID:                      m.ID,
		ProjectID:               m.ProjectID,
		Name:                    m.Name,
		TimeoutSeconds:          m.TimeoutSeconds,
		Enabled:                 m.Enabled,
		DeclaredScopes:          m.DeclaredScopes,
		TargetURL:               m.TargetURL,
		TargetSecretEnc:         m.TargetSecretEnc,
		TargetPreviousSecretEnc: m.TargetPreviousSecretEnc,
		TargetSecretRotatedAt:   m.TargetSecretRotatedAt,
		TargetSecretFingerprint: m.TargetSecretFingerprint,
		ClientCallable:          m.ClientCallable,
		ClientAnonymousAllowed:  m.ClientAnonymousAllowed,
		ClientPerUserLimit:      m.ClientPerUserLimit,
		ClientLimitWindow:       m.ClientLimitWindow,
		CreatedAt:               m.CreatedAt,
		UpdatedAt:               m.UpdatedAt,
	}
}

func mapInvocationToModel(e *domainfunctions.InvocationRecord) *model.FunctionInvocation {
	return &model.FunctionInvocation{
		ID:                e.ID,
		FunctionID:        e.FunctionID,
		ProjectID:         e.ProjectID,
		Status:            e.Status,
		Response:          e.Response,
		ResponseTruncated: e.ResponseTruncated,
		StatusCode:        e.StatusCode,
		DurationMS:        e.DurationMS,
		Error:             e.Error,
		TimeoutSeconds:    e.TimeoutSeconds,
		// 触发来源（P1，迁移 000015）与客户端调用面（P2，迁移 000016）：
		// INSERT 期写入、之后不可变——UpdateInvocation 列白名单有意不含
		// trigger_source/source_ip/invoking_user_id/client_idempotency_key。
		TriggerSource:        e.TriggerSource,
		SourceIP:             e.SourceIP,
		InvokingUserID:       e.InvokingUserID,
		ClientIdempotencyKey: e.ClientIdempotencyKey,
		CreatedAt:            e.CreatedAt,
		UpdatedAt:            e.UpdatedAt,
	}
}

func mapInvocationToDomain(m *model.FunctionInvocation) *domainfunctions.InvocationRecord {
	return &domainfunctions.InvocationRecord{
		ID:                   m.ID,
		FunctionID:           m.FunctionID,
		ProjectID:            m.ProjectID,
		Status:               m.Status,
		Response:             m.Response,
		ResponseTruncated:    m.ResponseTruncated,
		StatusCode:           m.StatusCode,
		DurationMS:           m.DurationMS,
		Error:                m.Error,
		TimeoutSeconds:       m.TimeoutSeconds,
		TriggerSource:        m.TriggerSource,
		SourceIP:             m.SourceIP,
		InvokingUserID:       m.InvokingUserID,
		ClientIdempotencyKey: m.ClientIdempotencyKey,
		CreatedAt:            m.CreatedAt,
		UpdatedAt:            m.UpdatedAt,
	}
}
