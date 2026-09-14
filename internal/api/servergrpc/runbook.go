package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	domainrunbook "github.com/torchwoodcloud/torchwood/internal/domain/runbook"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// RunbookService 是迁移状态面 gRPC handler（薄：scope/角色门在拦截器，
// CAS/顶版校验在 use-case；项目上下文来自凭证——决策 v8 项目寻址不变量）。
type RunbookService struct {
	serverv1.UnimplementedRunbookServiceServer
	runbook *appserver.Runbook
}

// NewRunbookService constructs the server runbook service.
func NewRunbookService(runbook *appserver.Runbook) *RunbookService {
	return &RunbookService{runbook: runbook}
}

func (s *RunbookService) GetRunbookState(ctx context.Context, req *serverv1.GetRunbookStateRequest) (*serverv1.GetRunbookStateResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	steps, err := s.runbook.List(ctx, projectID, req.GetRunbook())
	if err != nil {
		return nil, err
	}
	out := &serverv1.GetRunbookStateResponse{Steps: make([]*serverv1.RunbookStepState, len(steps))}
	for i := range steps {
		out.Steps[i] = mapRunbookStepState(&steps[i])
	}
	return out, nil
}

func (s *RunbookService) RecordRunbookStep(ctx context.Context, req *serverv1.RecordRunbookStepRequest) (*serverv1.RecordRunbookStepResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	current, err := s.runbook.Record(withAuditResource(ctx, req.GetRunbook()), projectID, appserver.RecordCommand{
		Runbook:           req.GetRunbook(),
		Version:           req.GetVersion(),
		Name:              req.GetName(),
		Checksum:          req.GetChecksum(),
		ExpectPrevVersion: req.ExpectPrevVersion,
	})
	if err != nil {
		return nil, err
	}
	return &serverv1.RecordRunbookStepResponse{CurrentVersion: current}, nil
}

func (s *RunbookService) DeleteRunbookStep(ctx context.Context, req *serverv1.DeleteRunbookStepRequest) (*sharedv1.Empty, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "invalid request")
	}
	projectID := projectIDFromContext(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if err := s.runbook.Delete(withAuditResource(ctx, req.GetRunbook()), projectID, req.GetRunbook(), req.GetVersion()); err != nil {
		return nil, err
	}
	return &sharedv1.Empty{}, nil
}

// mapRunbookStepState 是 domain → proto 的状态投影（物理寻址字段
// project_id 不出现在任何 API 响应，见 docs/design/runbook.md §2.2）。
func mapRunbookStepState(step *domainrunbook.StepState) *serverv1.RunbookStepState {
	return &serverv1.RunbookStepState{
		Version:   step.Version,
		Name:      step.Name,
		Checksum:  step.Checksum,
		AppliedAt: timestamppb.New(step.AppliedAt),
	}
}
