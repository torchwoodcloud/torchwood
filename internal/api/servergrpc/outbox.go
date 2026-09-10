package servergrpc

import (
	"context"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/app/events"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type OutboxService struct {
	serverv1.UnimplementedOutboxServiceServer
	outbox *events.OutboxAdmin
}

func NewOutboxService(outbox *events.OutboxAdmin) *OutboxService {
	return &OutboxService{outbox: outbox}
}

// projectContext 落实项目寻址不变量（决策 v8，修 viewer 跨项目枚举死信）：
// 项目上下文一律来自凭证（API key=密钥行绑定；admin=X-Torchwood-Project，
// 含平台 admin），请求体不做寻址回退；缺失即 FailedPrecondition。
func (s *OutboxService) projectContext(ctx context.Context) (string, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil || p.ProjectID == "" {
		return "", status.Error(codes.FailedPrecondition, "project context required (X-Torchwood-Project header for admin sessions)")
	}
	return p.ProjectID, nil
}

func (s *OutboxService) ListDeadLetters(ctx context.Context, req *serverv1.ListDeadLettersRequest) (*serverv1.ListDeadLettersResponse, error) {
	projectID, err := s.projectContext(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "outbox/dead")
	letters, total, next, err := s.outbox.ListDeadLetters(ctx, projectID, req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.DeadLetter, len(letters))
	for i, dl := range letters {
		out[i] = &serverv1.DeadLetter{
			EventId:   dl.EventID,
			ProjectId: dl.ProjectID,
			Topic:     dl.Topic,
			Channel:   dl.Channel,
			Payload:   dl.Payload,
			Attempts:  dl.Attempts,
			LastError: dl.LastError,
			CreatedAt: timestamppb.New(dl.CreatedAt),
		}
	}
	return &serverv1.ListDeadLettersResponse{
		DeadLetters: out,
		Meta:        &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), TotalCount: int32(total), NextPageToken: next},
	}, nil
}

func (s *OutboxService) ReplayDeadLetter(ctx context.Context, req *serverv1.ReplayDeadLetterRequest) (*serverv1.ReplayDeadLetterResponse, error) {
	// event_id required 由 buf.validate 注解在 validate 拦截器承担（09-api-guide §2.3）。
	projectID, err := s.projectContext(ctx)
	if err != nil {
		return nil, err
	}
	ctx = contexts.WithAuditResource(ctx, "outbox/dead/"+req.GetEventId())
	if err := s.outbox.ReplayDeadLetter(ctx, req.GetEventId(), projectID); err != nil {
		return nil, err
	}
	// available_at 为重放时刻的 NOW（与 outbox_repo 的 AvailableAt 一致在秒级内）.
	return &serverv1.ReplayDeadLetterResponse{EventId: req.GetEventId(), AvailableAt: timestamppb.New(time.Now())}, nil
}
