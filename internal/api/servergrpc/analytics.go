package servergrpc

import (
	"context"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AnalyticsService 是事件分析的管理/查询面（docs/design/analytics.md §4.1）。
// 本 PR（PR2）只实现摄入 IngestEvents；七个查询 RPC 留在嵌入的
// UnimplementedAnalyticsServiceServer 占位（实现随 PR3 到位，未实现即
// codes.Unimplemented）。项目上下文来自凭证（API key 绑定项目 / admin 会话
// X-Torchwood-Project），请求体不携带 project_id。
type AnalyticsService struct {
	serverv1.UnimplementedAnalyticsServiceServer
	ingest *appanalytics.Ingest
}

func NewAnalyticsService(ingest *appanalytics.Ingest) *AnalyticsService {
	return &AnalyticsService{ingest: ingest}
}

// IngestEvents 服务端权威事件摄入（支付完成、订阅续费、函数侧业务事件）：
// 可信代报 user_id（D3，缺省=无归属）、source='server' 服务端落定。审计豁免
// 显式登记（D13：高频非读动词不落 audit_logs，见 interceptor/audit.go 静默
// 清单）。
func (s *AnalyticsService) IngestEvents(ctx context.Context, req *serverv1.IngestServerEventsRequest) (*serverv1.IngestEventsResponse, error) {
	projectID := s.projectID(ctx)
	if projectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if len(req.GetEvents()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "events must not be empty")
	}
	events := make([]appanalytics.IncomingEvent, 0, len(req.GetEvents()))
	for _, e := range req.GetEvents() {
		if e == nil {
			continue
		}
		ev := appanalytics.IncomingEvent{
			Name:       e.GetName(),
			OccurredAt: serverEventTime(e.GetOccurredAt()),
			Props:      e.GetProps(),
			SessionID:  e.GetSessionId(),
		}
		if e.UserId != nil {
			ev.UserID = e.GetUserId()
		}
		events = append(events, ev)
	}
	res, err := s.ingest.IngestEvents(ctx, appanalytics.IngestEventsCommand{
		ProjectID: projectID,
		Source:    domainanalytics.SourceServer,
		Events:    events,
	})
	if err != nil {
		return nil, err
	}
	return &serverv1.IngestEventsResponse{Accepted: res.Accepted, Skipped: res.Skipped}, nil
}

func (s *AnalyticsService) projectID(ctx context.Context) string {
	p, ok := contexts.Principal(ctx)
	if !ok {
		return ""
	}
	return p.ProjectID
}

// serverEventTime 把 proto optional Timestamp 转 *time.Time（nil 保持 nil =
// 服务端 now 缺省；presence 语义：显式零值时间戳视为已设置，进钳制窗判定）。
func serverEventTime(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
