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
	// 列白名单（bun 更新写规范）：id/project_id/runtime/created_at 不可变。
	// declared_scopes 为 P0 执行身份可变列（SetFunctionScopes 全量替换）；
	// 池策略五列为 P0.5 可变列（latest_ready_deployment_id 由
	// ActivateDeployment/DeleteDeployment 事务维护，应用层更新不直写）；
	// concurrency 为 v3 可变列（迁移 000017，docs/design/functions-v3.md §1.5）；
	// client 四列为 P2 客户端调用面策略列。
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("name", "entrypoint", "timeout_seconds", "spec", "enabled", "declared_scopes",
			"min_instances", "max_instances", "idle_ttl_seconds", "max_requests_per_instance",
			"concurrency",
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

func (r *functionRepo) CreateDeployment(ctx context.Context, d *domainfunctions.Deployment) error {
	conn, sch, expr, err := r.scoped(ctx, d.ProjectID, "function_deployments", "fd")
	if err != nil {
		return err
	}
	m := mapDeploymentToModel(d)
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
}

func (r *functionRepo) GetDeployment(ctx context.Context, projectID, functionID, deploymentID string) (*domainfunctions.Deployment, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_deployments", "fd")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionDeployment)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("fd.project_id = ?", projectID).
		Where("fd.function_id = ?", functionID).
		Where("fd.id = ?", deploymentID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapDeploymentToDomain(m), nil
}

func (r *functionRepo) ListDeployments(ctx context.Context, projectID, functionID string) ([]domainfunctions.Deployment, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_deployments", "fd")
	if err != nil {
		return nil, err
	}
	var ms []model.FunctionDeployment
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("fd.project_id = ?", projectID).
		Where("fd.function_id = ?", functionID).
		Order("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domainfunctions.Deployment, len(ms))
	for i := range ms {
		out[i] = *mapDeploymentToDomain(&ms[i])
	}
	return out, nil
}

func (r *functionRepo) UpdateDeployment(ctx context.Context, d *domainfunctions.Deployment) error {
	conn, sch, expr, err := r.scoped(ctx, d.ProjectID, "function_deployments", "fd")
	if err != nil {
		return err
	}
	m := mapDeploymentToModel(d)
	// 列白名单（bun 更新写规范）：id/function_id/project_id/size/created_at
	// 不可变。不设状态 CAS——语义由既有集成测试锚定（跨项目误写静默 no-op、
	// ready→failed 合法），生产唯一写方 buildDeployment 串行推进状态。
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("status", "error", "updated_at").
		WherePK().
		Where("fd.project_id = ?", d.ProjectID).
		Where("fd.function_id = ?", d.FunctionID).
		Exec(ctx)
	return err
}

func (r *functionRepo) DeleteDeployment(ctx context.Context, projectID, functionID, deploymentID string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_deployments", "fd")
	if err != nil {
		return err
	}
	// 热路径清账（P0.5）：删除的是 latest_ready 指针指向的部署时，同事务
	// 清空指针（NULL 回退 selectDeployment 的全量列表逻辑）。
	err = r.db.RunInTx(ctx, func(txCtx context.Context) error {
		if _, err := conn.NewDelete().Model((*model.FunctionDeployment)(nil)).
			ModelTableExpr(expr, sch).
			Where("project_id = ?", projectID).
			Where("function_id = ?", functionID).
			Where("id = ?", deploymentID).
			Exec(txCtx); err != nil {
			return err
		}
		fconn, fsch, fexpr, err := r.scoped(txCtx, projectID, "functions", "f")
		if err != nil {
			return err
		}
		_, err = fconn.NewUpdate().Model((*model.Function)(nil)).
			ModelTableExpr(fexpr, fsch).
			Set("latest_ready_deployment_id = NULL").
			Where("f.project_id = ?", projectID).
			Where("f.id = ?", functionID).
			Where("f.latest_ready_deployment_id = ?", deploymentID).
			Exec(txCtx)
		return err
	})
	return err
}

// ActivateDeployment 在一个事务内把部署置 ready 并维护
// functions.latest_ready_deployment_id（热路径清账，P0.5：selectDeployment
// 优先读指针，消灭 ListDeployments 全量拉取）。
func (r *functionRepo) ActivateDeployment(ctx context.Context, d *domainfunctions.Deployment) error {
	conn, sch, expr, err := r.scoped(ctx, d.ProjectID, "function_deployments", "fd")
	if err != nil {
		return err
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		res, err := conn.NewUpdate().Model((*model.FunctionDeployment)(nil)).
			ModelTableExpr(expr, sch).
			Set("status = ?", d.Status).
			Set("error = ?", d.Error).
			Set("updated_at = ?", time.Now()).
			Where("fd.project_id = ?", d.ProjectID).
			Where("fd.function_id = ?", d.FunctionID).
			Where("fd.id = ?", d.ID).
			Exec(txCtx)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			// 部署行不存在（已被并发删除）：不 resurrect，也不动指针。
			return nil
		}
		fconn, fsch, fexpr, err := r.scoped(txCtx, d.ProjectID, "functions", "f")
		if err != nil {
			return err
		}
		_, err = fconn.NewUpdate().Model((*model.Function)(nil)).
			ModelTableExpr(fexpr, fsch).
			Set("latest_ready_deployment_id = ?", d.ID).
			Set("updated_at = ?", time.Now()).
			Where("f.project_id = ?", d.ProjectID).
			Where("f.id = ?", d.FunctionID).
			Exec(txCtx)
		return err
	})
}

func (r *functionRepo) SetVariables(ctx context.Context, projectID, functionID string, vars map[string]string) error {
	_, sch, expr, err := r.scoped(ctx, projectID, "function_variables", "fv")
	if err != nil {
		return err
	}
	return r.db.RunInTx(ctx, func(txCtx context.Context) error {
		conn := r.db.Conn(txCtx)
		if _, err := conn.NewDelete().Model((*model.FunctionVariable)(nil)).
			ModelTableExpr(expr, sch).
			Where("project_id = ?", projectID).
			Where("function_id = ?", functionID).
			Exec(txCtx); err != nil {
			return err
		}
		for k, v := range vars {
			if _, err := conn.NewInsert().Model(&model.FunctionVariable{
				ID:         "var_" + functionID + "_" + k,
				FunctionID: functionID,
				ProjectID:  projectID,
				Key:        k,
				Value:      v,
				// P0 只产 text 变量（secret 的 API 面随后续阶段开放）；
				// 显式写而非依赖列 DEFAULT，行内容不随迁移默认值漂移。
				Kind: domainfunctions.VariableKindText,
			}).ModelTableExpr(expr, sch).Exec(txCtx); err != nil {
				return err
			}
		}
		return nil
	})
}

// GetVariables 返回 key → value 视图；kind 随模型扫描读出（P0 的掩码与
// 注入语义不区分 kind——同表明文，见设计 §2），随后续阶段在 domain 层开放。
func (r *functionRepo) GetVariables(ctx context.Context, projectID, functionID string) (map[string]string, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_variables", "fv")
	if err != nil {
		return nil, err
	}
	var ms []model.FunctionVariable
	err = conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("fv.project_id = ?", projectID).
		Where("fv.function_id = ?", functionID).
		Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(ms))
	for i := range ms {
		out[ms[i].Key] = ms[i].Value
	}
	return out, nil
}

func (r *functionRepo) CreateExecution(ctx context.Context, e *domainfunctions.ExecutionRecord) error {
	conn, sch, expr, err := r.scoped(ctx, e.ProjectID, "function_executions", "fe")
	if err != nil {
		return err
	}
	m := mapExecutionToModel(e)
	if _, err := conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx); err != nil {
		// P2 幂等（设计 §4）：partial 唯一索引 (project, function, user, key)
		// 的并发冲突归一为领域哨兵错误——调用方（client invoke）回读既有行
		// 原样返回。本表其余唯一键只有主键（服务端生成 UUID，不会冲突），
		// 23505 在此只会来自幂等索引。
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: %v", domainfunctions.ErrExecutionIdempotencyConflict, err)
		}
		return err
	}
	return nil
}

// GetExecutionByIdempotencyKey 按 (project, function, user, key) 查既有执行
// （幂等冲突回读原样返回）。
func (r *functionRepo) GetExecutionByIdempotencyKey(ctx context.Context, projectID, functionID, userID, key string) (*domainfunctions.ExecutionRecord, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_executions", "fe")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionExecution)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("fe.project_id = ?", projectID).
		Where("fe.function_id = ?", functionID).
		Where("fe.invoking_user_id = ?", userID).
		Where("fe.client_idempotency_key = ?", key).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapExecutionToDomain(m), nil
}

// CountClientInvocations 统计窗口起点以来该用户的 client 来源执行数
// （限频 DB 降级路径；partial 索引 function_executions_client_quota 支撑）。
func (r *functionRepo) CountClientInvocations(ctx context.Context, projectID, functionID, userID string, since time.Time) (int, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_executions", "fe")
	if err != nil {
		return 0, err
	}
	n, err := conn.NewSelect().Model((*model.FunctionExecution)(nil)).ModelTableExpr(expr, sch).
		Where("fe.project_id = ?", projectID).
		Where("fe.function_id = ?", functionID).
		Where("fe.invoking_user_id = ?", userID).
		Where("fe.trigger_source = ?", domainfunctions.TriggerSourceClient).
		Where("fe.created_at >= ?", since).
		Count(ctx)
	return int(n), err
}

func (r *functionRepo) GetExecution(ctx context.Context, projectID, functionID, executionID string) (*domainfunctions.ExecutionRecord, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_executions", "fe")
	if err != nil {
		return nil, err
	}
	m := new(model.FunctionExecution)
	err = conn.NewSelect().Model(m).ModelTableExpr(expr, sch).
		Where("fe.project_id = ?", projectID).
		Where("fe.function_id = ?", functionID).
		Where("fe.id = ?", executionID).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return mapExecutionToDomain(m), nil
}

func (r *functionRepo) ListExecutions(ctx context.Context, projectID, functionID string, limit int) ([]domainfunctions.ExecutionRecord, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_executions", "fe")
	if err != nil {
		return nil, err
	}
	var ms []model.FunctionExecution
	q := conn.NewSelect().Model(&ms).ModelTableExpr(expr, sch).
		Where("fe.project_id = ?", projectID).
		Where("fe.function_id = ?", functionID).
		Order("created_at DESC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]domainfunctions.ExecutionRecord, len(ms))
	for i := range ms {
		out[i] = *mapExecutionToDomain(&ms[i])
	}
	return out, nil
}

func (r *functionRepo) UpdateExecution(ctx context.Context, e *domainfunctions.ExecutionRecord) error {
	conn, sch, expr, err := r.scoped(ctx, e.ProjectID, "function_executions", "fe")
	if err != nil {
		return err
	}
	m := mapExecutionToModel(e)
	// 列白名单（bun 更新写规范）：id/function_id/project_id/deployment_id/
	// created_at 不可变。
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("status", "response", "response_truncated", "stdout", "stdout_truncated",
			"stderr", "stderr_truncated", "status_code", "duration_ms", "error", "updated_at").
		WherePK().
		Where("fe.project_id = ?", e.ProjectID).
		Exec(ctx)
	return err
}

func (r *functionRepo) TransitionExecutionStatus(ctx context.Context, projectID, functionID, executionID, from, to string) (bool, error) {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_executions", "fe")
	if err != nil {
		return false, err
	}
	res, err := conn.NewUpdate().Model((*model.FunctionExecution)(nil)).
		ModelTableExpr(expr, sch).
		Set("status = ?", to).
		Set("updated_at = ?", time.Now()).
		Where("fe.project_id = ?", projectID).
		Where("fe.function_id = ?", functionID).
		Where("fe.id = ?", executionID).
		Where("fe.status = ?", from).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (r *functionRepo) FailExecutionIfActive(ctx context.Context, projectID, functionID, executionID, reason string) error {
	conn, sch, expr, err := r.scoped(ctx, projectID, "function_executions", "fe")
	if err != nil {
		return err
	}
	_, err = conn.NewUpdate().Model((*model.FunctionExecution)(nil)).
		ModelTableExpr(expr, sch).
		Set("status = ?", domainfunctions.ExecutionStatusFailed).
		Set("error = ?", reason).
		Set("updated_at = ?", time.Now()).
		Where("fe.project_id = ?", projectID).
		Where("fe.function_id = ?", functionID).
		Where("fe.id = ?", executionID).
		Where("fe.status IN (?)", bun.List([]string{
			domainfunctions.ExecutionStatusQueued,
			domainfunctions.ExecutionStatusBuilding,
			domainfunctions.ExecutionStatusRunning,
		})).
		Exec(ctx)
	return err
}

func (r *functionRepo) RecoverOrphanExecutionsInProject(ctx context.Context, projectID string, olderThan time.Time, limit int) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	if _, _, _, err := r.scoped(ctx, projectID, "function_executions", "fe"); err != nil {
		return 0, err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return 0, err
	}
	// P0.5 孤儿恢复：扫描范围纳入 queued（红队指出的 v1 既有洞——同步快路径
	// 崩溃曾留 queued 永不入队）；staleAfter 按行判定（domainfunctions.
	// OrphanGraceSeconds 宽限与迁移/app 层同源）：timeout_seconds 非空 =
	// 该值 + 宽限（两写预占的行内快照），NULL（存量行）回退 olderThan
	// （worker 传 1h）。
	res, err := r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
WITH cte AS (
  SELECT id FROM %s.function_executions
  WHERE project_id = ?
    AND status IN (?, ?, ?)
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
UPDATE %s.function_executions AS fe
SET status = ?, error = ?, updated_at = NOW()
FROM cte WHERE fe.id = cte.id
`, quoted, quoted),
		projectID,
		domainfunctions.ExecutionStatusQueued,
		domainfunctions.ExecutionStatusBuilding,
		domainfunctions.ExecutionStatusRunning,
		domainfunctions.OrphanGraceSeconds,
		olderThan,
		limit,
		domainfunctions.ExecutionStatusFailed,
		"orphaned execution recovered",
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (r *functionRepo) PruneOldExecutionsInProject(ctx context.Context, projectID, functionID string, keepRecent int) error {
	if keepRecent <= 0 {
		return nil
	}
	if _, _, _, err := r.scoped(ctx, projectID, "function_executions", "fe"); err != nil {
		return err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	// 只清理终态记录：queued/building/running 可能仍对应在途队列消息，
	// 物理删除会导致消息被消费时静默丢弃（rec==nil → Ack）。
	// 条数式保留只作用于 server 面行（trigger_source = ''，Q7 保留分级）——
	// client/http/cron 来源行是限频 DB 降级的计数依据，按 48h 时间窗保留
	// （PruneTriggerExecutionsInProject），条数裁剪会造成少计超发。
	_, err = r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.function_executions
WHERE project_id = ? AND function_id = ?
  AND trigger_source = ''
  AND status IN (?, ?)
  AND id NOT IN (
    SELECT id FROM (
      SELECT id FROM %s.function_executions
      WHERE project_id = ? AND function_id = ? AND trigger_source = ''
      ORDER BY created_at DESC
      LIMIT ?
    ) keep
  )
`, quoted, quoted),
		projectID, functionID,
		domainfunctions.ExecutionStatusCompleted,
		domainfunctions.ExecutionStatusFailed,
		projectID, functionID, keepRecent)
	return err
}

// PruneTriggerExecutionsInProject 清理 client/http/cron 来源超过 olderThan 的
// 终态记录（Q7 保留分级，time-based：48h ≥ 2× 最长限频窗口 day，保证限频
// DB 降级在最长窗口内的计数行不被裁剪）。终态约束与 server 路径同源。
func (r *functionRepo) PruneTriggerExecutionsInProject(ctx context.Context, projectID, functionID string, olderThan time.Time) error {
	if _, _, _, err := r.scoped(ctx, projectID, "function_executions", "fe"); err != nil {
		return err
	}
	quoted, err := ProjectQuoted(projectID)
	if err != nil {
		return err
	}
	_, err = r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.function_executions
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
		domainfunctions.ExecutionStatusCompleted,
		domainfunctions.ExecutionStatusFailed,
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
	// 池策略（P0.5）零值归一为平台默认：UPDATE 是显式列白名单全模型写
	// （bun 更新写规范），struct 零值会被原样写成 0 而违反迁移 000014 的
	// CHECK 下限（max>=1 / idle>=30 / max_requests>=1）；「零值 = 平台默认」
	// 与 INSERT 侧 bun default tag 语义一致。下限值与迁移 CHECK 同源。
	minInstances, maxInstances := fn.MinInstances, fn.MaxInstances
	idleTTL, maxRequests := fn.IdleTTLSeconds, fn.MaxRequestsPerInstance
	concurrency := fn.Concurrency
	if maxInstances < 1 {
		maxInstances = 2
	}
	if idleTTL < 30 {
		idleTTL = 300
	}
	if maxRequests < 1 {
		maxRequests = 1000
	}
	if minInstances > maxInstances {
		minInstances = maxInstances
	}
	// concurrency 零值归一为平台默认 1（迁移 000017 列 DEFAULT 同值；v3
	// §1.1 fail-closed：不 opt-in 即串行）。上界 16 由 DB CHECK 兜底——超限
	// 报错而非静默截断（管理面校验随 B 切片 proto API 落地）。
	if concurrency < 1 {
		concurrency = 1
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
		Runtime:                 fn.Runtime,
		Entrypoint:              fn.Entrypoint,
		TimeoutSeconds:          fn.TimeoutSeconds,
		Spec:                    fn.Spec,
		Enabled:                 fn.Enabled,
		DeclaredScopes:          declaredScopes,
		MinInstances:            minInstances,
		MaxInstances:            maxInstances,
		IdleTTLSeconds:          idleTTL,
		MaxRequestsPerInstance:  maxRequests,
		Concurrency:             concurrency,
		LatestReadyDeploymentID: fn.LatestReadyDeploymentID,
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
		Runtime:                 m.Runtime,
		Entrypoint:              m.Entrypoint,
		TimeoutSeconds:          m.TimeoutSeconds,
		Spec:                    m.Spec,
		Enabled:                 m.Enabled,
		DeclaredScopes:          m.DeclaredScopes,
		MinInstances:            m.MinInstances,
		MaxInstances:            m.MaxInstances,
		IdleTTLSeconds:          m.IdleTTLSeconds,
		MaxRequestsPerInstance:  m.MaxRequestsPerInstance,
		Concurrency:             m.Concurrency,
		LatestReadyDeploymentID: m.LatestReadyDeploymentID,
		ClientCallable:          m.ClientCallable,
		ClientAnonymousAllowed:  m.ClientAnonymousAllowed,
		ClientPerUserLimit:      m.ClientPerUserLimit,
		ClientLimitWindow:       m.ClientLimitWindow,
		CreatedAt:               m.CreatedAt,
		UpdatedAt:               m.UpdatedAt,
	}
}

func mapDeploymentToModel(d *domainfunctions.Deployment) *model.FunctionDeployment {
	return &model.FunctionDeployment{
		ID:              d.ID,
		FunctionID:      d.FunctionID,
		ProjectID:       d.ProjectID,
		Size:            d.Size,
		Status:          d.Status,
		Error:           d.Error,
		TemplateVersion: d.TemplateVersion,
		CreatedAt:       d.CreatedAt,
		UpdatedAt:       d.UpdatedAt,
	}
}

func mapDeploymentToDomain(m *model.FunctionDeployment) *domainfunctions.Deployment {
	return &domainfunctions.Deployment{
		ID:              m.ID,
		FunctionID:      m.FunctionID,
		ProjectID:       m.ProjectID,
		Size:            m.Size,
		Status:          m.Status,
		Error:           m.Error,
		TemplateVersion: m.TemplateVersion,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
	}
}

func mapExecutionToModel(e *domainfunctions.ExecutionRecord) *model.FunctionExecution {
	return &model.FunctionExecution{
		ID:                e.ID,
		FunctionID:        e.FunctionID,
		ProjectID:         e.ProjectID,
		DeploymentID:      e.DeploymentID,
		Status:            e.Status,
		Response:          e.Response,
		ResponseTruncated: e.ResponseTruncated,
		Stdout:            e.Stdout,
		StdoutTruncated:   e.StdoutTruncated,
		Stderr:            e.Stderr,
		StderrTruncated:   e.StderrTruncated,
		StatusCode:        e.StatusCode,
		DurationMS:        e.DurationMS,
		Error:             e.Error,
		TimeoutSeconds:    e.TimeoutSeconds,
		// 触发来源（P1，迁移 000015）与客户端调用面（P2，迁移 000016）：
		// INSERT 期写入、之后不可变——UpdateExecution 列白名单有意不含
		// trigger_source/source_ip/invoking_user_id/client_idempotency_key。
		TriggerSource:        e.TriggerSource,
		SourceIP:             e.SourceIP,
		InvokingUserID:       e.InvokingUserID,
		ClientIdempotencyKey: e.ClientIdempotencyKey,
		CreatedAt:            e.CreatedAt,
		UpdatedAt:            e.UpdatedAt,
	}
}

func mapExecutionToDomain(m *model.FunctionExecution) *domainfunctions.ExecutionRecord {
	return &domainfunctions.ExecutionRecord{
		ID:                   m.ID,
		FunctionID:           m.FunctionID,
		ProjectID:            m.ProjectID,
		DeploymentID:         m.DeploymentID,
		Status:               m.Status,
		Response:             m.Response,
		ResponseTruncated:    m.ResponseTruncated,
		Stdout:               m.Stdout,
		StdoutTruncated:      m.StdoutTruncated,
		Stderr:               m.Stderr,
		StderrTruncated:      m.StderrTruncated,
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
