package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appserver "github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	"github.com/torchwoodcloud/torchwood/pkg/crud"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type AuditLogsService struct {
	serverv1.UnimplementedAuditLogsServiceServer
	auditLogs *appserver.AuditLogs
}

func NewAuditLogsService(auditLogs *appserver.AuditLogs) *AuditLogsService {
	return &AuditLogsService{auditLogs: auditLogs}
}

// ListAuditLogs 查询审计日志：offset 分页（pkg/crud token，仿 apikeys），
// 结构化过滤 exact 匹配，项目作用域语义在 app 用例层（决策对齐 Outbox）。
func (s *AuditLogsService) ListAuditLogs(ctx context.Context, req *serverv1.ListAuditLogsRequest) (*serverv1.ListAuditLogsResponse, error) {
	params, err := crud.ParseListParams(req.GetPageSize(), req.GetPageToken(), "", "")
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	query := appserver.AuditLogsQuery{
		ActorID:         req.GetActorId(),
		ActorKind:       req.GetActorKind(),
		Action:          req.GetAction(),
		Status:          req.GetStatus(),
		ResourceID:      req.GetResourceId(),
		IncludePlatform: req.GetIncludePlatform(),
		AllProjects:     req.GetAllProjects(),
		Offset:          params.Offset,
		PageSize:        int(params.PageSize),
	}
	if ts := req.GetCreatedAfter(); ts != nil {
		t := ts.AsTime()
		query.CreatedAfter = &t
	}
	if ts := req.GetCreatedBefore(); ts != nil {
		t := ts.AsTime()
		query.CreatedBefore = &t
	}
	entries, total, err := s.auditLogs.List(ctx, query)
	if err != nil {
		return nil, err
	}
	info := crud.BuildPaginationInfo(params, total, params.Offset+int(params.PageSize) < total)
	var nextToken, prevToken string
	if info.HasNext {
		if nextToken, err = crud.EncodePageToken(info.NextOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if info.HasPrevious {
		if prevToken, err = crud.EncodePageToken(info.PreviousOffset); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	out := make([]*serverv1.AuditLog, len(entries))
	for i := range entries {
		out[i] = mapAuditLog(&entries[i])
	}
	return &serverv1.ListAuditLogsResponse{
		AuditLogs: out,
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      info.PageSize,
			TotalCount:    int32(info.TotalCount),
			NextPageToken: nextToken,
			PrevPageToken: prevToken,
		},
	}, nil
}

func mapAuditLog(e *audit.Entry) *serverv1.AuditLog {
	out := &serverv1.AuditLog{
		Id:         e.ID,
		ProjectId:  e.ProjectID,
		ActorId:    e.ActorID,
		ActorKind:  e.ActorKind,
		Action:     e.Action,
		Status:     e.Status,
		ResourceId: e.ResourceID,
		Ip:         e.IP,
		UserAgent:  e.UserAgent,
		CreatedAt:  timestamppb.New(e.CreatedAt),
	}
	if e.Metadata != nil {
		if md, err := structpb.NewStruct(e.Metadata); err == nil {
			out.Metadata = md
		}
	}
	return out
}
