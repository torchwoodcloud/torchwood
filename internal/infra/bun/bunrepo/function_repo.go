package bunrepo

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	domainfunctions "github.com/torchwooddev/torchwood/internal/domain/functions"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
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
	_, err = conn.NewUpdate().Model(m).ModelTableExpr(expr, sch).
		Column("name", "entrypoint", "timeout_seconds", "spec", "enabled", "updated_at").
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
	_, err = conn.NewDelete().Model((*model.FunctionDeployment)(nil)).ModelTableExpr(expr, sch).
		Where("project_id = ?", projectID).
		Where("function_id = ?", functionID).
		Where("id = ?", deploymentID).
		Exec(ctx)
	return err
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
			}).ModelTableExpr(expr, sch).Exec(txCtx); err != nil {
				return err
			}
		}
		return nil
	})
}

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
	_, err = conn.NewInsert().Model(m).ModelTableExpr(expr, sch).Exec(ctx)
	return err
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
	res, err := r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
WITH cte AS (
  SELECT id FROM %s.function_executions
  WHERE project_id = ? AND status IN (?, ?) AND updated_at < ?
  ORDER BY updated_at
  LIMIT ?
  FOR UPDATE SKIP LOCKED
)
UPDATE %s.function_executions AS fe
SET status = ?, error = ?, updated_at = NOW()
FROM cte WHERE fe.id = cte.id
`, quoted, quoted),
		projectID,
		domainfunctions.ExecutionStatusBuilding,
		domainfunctions.ExecutionStatusRunning,
		olderThan,
		limit,
		domainfunctions.ExecutionStatusFailed,
		"worker restarted",
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
	_, err = r.db.Conn(ctx).ExecContext(ctx, fmt.Sprintf(`
DELETE FROM %s.function_executions
WHERE project_id = ? AND function_id = ?
  AND status IN (?, ?)
  AND id NOT IN (
    SELECT id FROM (
      SELECT id FROM %s.function_executions
      WHERE project_id = ? AND function_id = ?
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

func mapFunctionToModel(fn *domainfunctions.Function) *model.Function {
	return &model.Function{
		ID:             fn.ID,
		ProjectID:      fn.ProjectID,
		Name:           fn.Name,
		Runtime:        fn.Runtime,
		Entrypoint:     fn.Entrypoint,
		TimeoutSeconds: fn.TimeoutSeconds,
		Spec:           fn.Spec,
		Enabled:        fn.Enabled,
		CreatedAt:      fn.CreatedAt,
		UpdatedAt:      fn.UpdatedAt,
	}
}

func mapFunctionToDomain(m *model.Function) *domainfunctions.Function {
	return &domainfunctions.Function{
		ID:             m.ID,
		ProjectID:      m.ProjectID,
		Name:           m.Name,
		Runtime:        m.Runtime,
		Entrypoint:     m.Entrypoint,
		TimeoutSeconds: m.TimeoutSeconds,
		Spec:           m.Spec,
		Enabled:        m.Enabled,
		CreatedAt:      m.CreatedAt,
		UpdatedAt:      m.UpdatedAt,
	}
}

func mapDeploymentToModel(d *domainfunctions.Deployment) *model.FunctionDeployment {
	return &model.FunctionDeployment{
		ID:         d.ID,
		FunctionID: d.FunctionID,
		ProjectID:  d.ProjectID,
		Size:       d.Size,
		Status:     d.Status,
		Error:      d.Error,
		CreatedAt:  d.CreatedAt,
		UpdatedAt:  d.UpdatedAt,
	}
}

func mapDeploymentToDomain(m *model.FunctionDeployment) *domainfunctions.Deployment {
	return &domainfunctions.Deployment{
		ID:         m.ID,
		FunctionID: m.FunctionID,
		ProjectID:  m.ProjectID,
		Size:       m.Size,
		Status:     m.Status,
		Error:      m.Error,
		CreatedAt:  m.CreatedAt,
		UpdatedAt:  m.UpdatedAt,
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
		CreatedAt:         e.CreatedAt,
		UpdatedAt:         e.UpdatedAt,
	}
}

func mapExecutionToDomain(m *model.FunctionExecution) *domainfunctions.ExecutionRecord {
	return &domainfunctions.ExecutionRecord{
		ID:                m.ID,
		FunctionID:        m.FunctionID,
		ProjectID:         m.ProjectID,
		DeploymentID:      m.DeploymentID,
		Status:            m.Status,
		Response:          m.Response,
		ResponseTruncated: m.ResponseTruncated,
		Stdout:            m.Stdout,
		StdoutTruncated:   m.StdoutTruncated,
		Stderr:            m.Stderr,
		StderrTruncated:   m.StderrTruncated,
		StatusCode:        m.StatusCode,
		DurationMS:        m.DurationMS,
		Error:             m.Error,
		CreatedAt:         m.CreatedAt,
		UpdatedAt:         m.UpdatedAt,
	}
}
