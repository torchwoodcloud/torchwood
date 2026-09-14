package server

import (
	"context"
	"errors"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/runbook"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Runbook 是版本化资源迁移的状态用例（docs/design/runbook.md §2.2）：
// 只记录事实、不执行动作；CAS（D8）与顶版校验（版本链单调）在此把关，
// 唯一约束 (project_id, runbook, version) 是并发双写的最后兜底。
type Runbook struct {
	repo runbook.StateRepo
}

// NewRunbook constructs the runbook state use-case.
func NewRunbook(repo runbook.StateRepo) *Runbook {
	return &Runbook{repo: repo}
}

// List 返回指定 runbook 的已应用 step 升序全集（空状态 = 空切片）。
func (r *Runbook) List(ctx context.Context, projectID, runbookName string) ([]runbook.StepState, error) {
	return r.repo.ListSteps(ctx, projectID, runbookName)
}

// RecordCommand 是记录一个已应用 step 的输入。ExpectPrevVersion 为 CAS
// 断言值（nil/0 = 断言无前序，即首步）。
type RecordCommand struct {
	Runbook           string
	Version           int64
	Name              string
	Checksum          string
	ExpectPrevVersion *int64
}

// Record 记录已应用 step（D8 CAS 语义）：
//   - ExpectPrevVersion 缺省/0：要求当前无任何 step（首步）；
//   - 显式值必须等于当前顶版（最后一条的 version）；
//   - 不满足 → FailedPrecondition（并发双跑输家重拉状态收敛，CLI 侧裁决）。
//
// checksum/名称原样落库（sha256 对账是引擎职责 D9，服务端不做值比较）。
// 并发漏过 CAS 的双写由唯一约束兜底 → AlreadyExists（等价于"对方已记录"）。
func (r *Runbook) Record(ctx context.Context, projectID string, cmd RecordCommand) (int64, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return 0, err
	}
	steps, err := r.repo.ListSteps(ctx, projectID, cmd.Runbook)
	if err != nil {
		return 0, err
	}
	top := int64(0)
	if len(steps) > 0 {
		top = steps[len(steps)-1].Version
	}
	expect := int64(0)
	if cmd.ExpectPrevVersion != nil {
		expect = *cmd.ExpectPrevVersion
	}
	if expect != top {
		return 0, status.Errorf(codes.FailedPrecondition,
			"runbook %q: expected previous version %d, but current top version is %d (concurrent writer or stale state; re-fetch and retry)",
			cmd.Runbook, expect, top)
	}
	if err := r.repo.InsertStep(ctx, projectID, runbook.StepState{
		Runbook:  cmd.Runbook,
		Version:  cmd.Version,
		Name:     cmd.Name,
		Checksum: cmd.Checksum,
	}); err != nil {
		if errors.Is(err, runbook.ErrStepExists) {
			return 0, status.Errorf(codes.AlreadyExists,
				"runbook %q: version %d already recorded (concurrent winner)", cmd.Runbook, cmd.Version)
		}
		return 0, err
	}
	return cmd.Version, nil
}

// Delete 摘除一条 step（down 回退 / D21 forgive）：仅允许删除当前顶版，
// 保证版本链单调；非顶版 → FailedPrecondition，不存在 → NotFound。
// 摘记录不执行任何资源动作——资源回退由调用方先完成。
func (r *Runbook) Delete(ctx context.Context, projectID, runbookName string, version int64) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	steps, err := r.repo.ListSteps(ctx, projectID, runbookName)
	if err != nil {
		return err
	}
	if len(steps) == 0 {
		return status.Errorf(codes.NotFound, "runbook %q has no recorded steps", runbookName)
	}
	top := steps[len(steps)-1].Version
	if version != top {
		return status.Errorf(codes.FailedPrecondition,
			"runbook %q: can only delete the top version (current top is %d, requested %d)",
			runbookName, top, version)
	}
	deleted, err := r.repo.DeleteStep(ctx, projectID, runbookName, version)
	if err != nil {
		return err
	}
	if !deleted {
		return status.Errorf(codes.NotFound, "runbook %q: version %d not found", runbookName, version)
	}
	return nil
}
