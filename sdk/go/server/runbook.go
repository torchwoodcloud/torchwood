package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// RunbookService 封装 Server API 的迁移状态面（docs/design/runbook.md
// §2.2）：只记录"哪些 step 已应用"的事实，不执行资源动作。CAS（Record 的
// expect_prev_version）与顶版校验（Delete）在服务端把关，输家重拉状态收敛。
type RunbookService struct {
	c   *Client
	api serverv1.RunbookServiceClient
}

// GetRunbookState 返回指定 runbook 的已应用 step 升序全集（空 = 未应用）。
func (s *RunbookService) GetRunbookState(ctx context.Context, runbook string) (*serverv1.GetRunbookStateResponse, error) {
	return s.api.GetRunbookState(ctx, &serverv1.GetRunbookStateRequest{Runbook: runbook})
}

// RecordRunbookStep 记录一个已应用 step。expectPrevVersion 缺省（nil）= 断言
// 首步；显式值必须等于服务端当前顶版，否则 FailedPrecondition（并发双跑
// 输家重拉状态收敛）。
func (s *RunbookService) RecordRunbookStep(ctx context.Context, runbook string, version int64, name, checksum string, expectPrevVersion *int64) (*serverv1.RecordRunbookStepResponse, error) {
	return s.api.RecordRunbookStep(ctx, &serverv1.RecordRunbookStepRequest{
		Runbook:           runbook,
		Version:           version,
		Name:              name,
		Checksum:          checksum,
		ExpectPrevVersion: expectPrevVersion,
	})
}

// DeleteRunbookStep 摘除一条 step（down 回退 / forgive 逃生门）：仅允许
// 删除当前顶版（版本链单调），不执行任何资源动作。
func (s *RunbookService) DeleteRunbookStep(ctx context.Context, runbook string, version int64) (*sharedv1.Empty, error) {
	return s.api.DeleteRunbookStep(ctx, &serverv1.DeleteRunbookStepRequest{
		Runbook: runbook,
		Version: version,
	})
}
