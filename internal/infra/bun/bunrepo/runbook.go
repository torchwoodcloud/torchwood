package bunrepo

import (
	"context"
	"time"

	"github.com/torchwoodcloud/torchwood/internal/domain/runbook"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
)

// runbookStateRepo 实现 runbook.StateRepo（控制面 runbook_steps，迁移
// 000009）。SELECT/INSERT/DELETE 三动词、恒无 UPDATE——迁移历史不可变
// （docs/design/runbook.md D6/D9），无 bun 更新写规范适用面。
type runbookStateRepo struct {
	db *clients.Database
}

// NewRunbookStateRepository 构造迁移状态仓储。
func NewRunbookStateRepository(db *clients.Database) runbook.StateRepo {
	return &runbookStateRepo{db: db}
}

// ListSteps 返回 (project, runbook) 的升序全集；空状态返回空切片。
func (r *runbookStateRepo) ListSteps(ctx context.Context, projectID, runbookName string) ([]runbook.StepState, error) {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var rows []model.RunbookStep
	err := r.db.NewSelect().Model(&rows).
		Where("project_id = ?", projectID).
		Where("runbook = ?", runbookName).
		Order("version ASC").
		Scan(ctx2)
	if err != nil {
		return nil, err
	}
	out := make([]runbook.StepState, len(rows))
	for i := range rows {
		out[i] = mapRunbookStepToDomain(&rows[i])
	}
	return out, nil
}

// InsertStep 落一条已应用记录；唯一键 (project_id, runbook, version) 冲突
// 返回 runbook.ErrStepExists（D8 并发兜底：CAS 漏过的双写只剩一个赢家）。
func (r *runbookStateRepo) InsertStep(ctx context.Context, projectID string, step runbook.StepState) error {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	m := &model.RunbookStep{
		ProjectID: projectID,
		Runbook:   step.Runbook,
		Version:   step.Version,
		Name:      step.Name,
		Checksum:  step.Checksum,
	}
	// applied_at 走列缺省 now()：记录"服务端确认时刻"，与客户端时钟无关。
	_, err := r.db.NewInsert().Model(m).Exec(ctx2)
	if isUniqueViolation(err) {
		return runbook.ErrStepExists
	}
	return err
}

// DeleteStep 摘除一条记录（顶版校验在 app 用例层）；不存在返回 false。
func (r *runbookStateRepo) DeleteStep(ctx context.Context, projectID, runbookName string, version int64) (bool, error) {
	ctx2, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := r.db.NewDelete().Model((*model.RunbookStep)(nil)).
		Where("project_id = ?", projectID).
		Where("runbook = ?", runbookName).
		Where("version = ?", version).
		Exec(ctx2)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func mapRunbookStepToDomain(m *model.RunbookStep) runbook.StepState {
	return runbook.StepState{
		Runbook:   m.Runbook,
		Version:   m.Version,
		Name:      m.Name,
		Checksum:  m.Checksum,
		AppliedAt: m.AppliedAt,
	}
}
