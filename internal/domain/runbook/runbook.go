// Package runbook 是版本化资源迁移的状态域（docs/design/runbook.md §2.2）。
// 状态只记录"哪些 step 已应用"的事实，不执行任何资源动作（引擎在 CLI 侧，
// D3）；本包保持纯 domain——不依赖 gRPC，错误码映射在 app 用例层。
package runbook

import (
	"context"
	"errors"
	"time"
)

// 域错误（app 层映射为 gRPC code）。
var (
	// ErrStepExists 是 UNIQUE (project_id, runbook, version) 冲突：并发双跑
	// 漏过 CAS 的兜底落败（D8）。
	ErrStepExists = errors.New("runbook step already exists")
)

// StepState 是单条已应用 step 的状态（物理寻址字段 project_id 不进本结构
// ——项目上下文来自凭证，repo 方法显式传参）。
type StepState struct {
	Runbook   string
	Version   int64
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// StateRepo 持久化控制面 runbook_steps（迁移 000009）。全量 SELECT/INSERT/
// DELETE，无 UPDATE——迁移历史不可变（D6/D9：改历史 = 环境分叉之源，唯一
// 出路是摘除后重放，见 D21 forgive）；唯一键 (project_id, runbook, version)
// 冲突返回 ErrStepExists。
type StateRepo interface {
	// ListSteps 返回升序全集（version ASC）；空状态返回空切片。
	ListSteps(ctx context.Context, projectID, runbook string) ([]StepState, error)
	// InsertStep 落一条已应用记录（applied_at 由 repo 填充）。
	InsertStep(ctx context.Context, projectID string, step StepState) error
	// DeleteStep 摘除一条记录；不存在返回 false。
	DeleteStep(ctx context.Context, projectID, runbook string, version int64) (bool, error)
}
