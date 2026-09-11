package clientgrpc

import (
	"context"
	"time"

	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AnalyticsService 是端侧事件摄入面（docs/design/analytics.md §4.2）：
// 只有写入、没有查询（行为数据是项目方资产）。归因（user_id）与
// source='client' 由服务端从 Principal 落定，请求体无法伪造（红线 D3——
// client 面请求不携带 user_id 字段）。
type AnalyticsService struct {
	clientv1.UnimplementedAnalyticsServiceServer
	ingest *appanalytics.Ingest
}

func NewAnalyticsService(ingest *appanalytics.Ingest) *AnalyticsService {
	return &AnalyticsService{ingest: ingest}
}

// IngestEvents 批量摄入端侧事件（部分接收语义：坏事件 skipped、好事件照收；
// at-least-once，D14）。门禁 = 至少匿名会话（ACCESS_END_USER——匿名会话也是
// users 表真实行，UserID 恒非空）；session_id 取事件体（端自报的 SDK 会话
// 边界，设计 §2「会话边界由各端 SDK 定义，平台只收 session_id」——Principal
// .SessionID 是平台登录会话，语义不同，不写入分析面）。
func (s *AnalyticsService) IngestEvents(ctx context.Context, req *clientv1.IngestEventsRequest) (*clientv1.IngestEventsResponse, error) {
	p, ok := contexts.Principal(ctx)
	if !ok || p == nil || p.ProjectID == "" {
		return nil, status.Error(codes.Unauthenticated, "missing project context")
	}
	if p.ActorKind != shared.ActorKindEndUser || p.UserID == "" {
		// client 面归因唯一来源是端用户 Principal（D3 红线）。
		return nil, status.Error(codes.Unauthenticated, "analytics ingestion requires an end-user session")
	}
	if len(req.GetEvents()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "events must not be empty")
	}
	res, err := s.ingest.IngestEvents(ctx, appanalytics.IngestEventsCommand{
		ProjectID:    p.ProjectID,
		Source:       domainanalytics.SourceClient,
		ClientUserID: p.UserID,
		Events:       incomingEvents(req.GetEvents()),
	})
	if err != nil {
		return nil, err
	}
	return &clientv1.IngestEventsResponse{Accepted: res.Accepted, Skipped: res.Skipped}, nil
}

// incomingEvents 把 client 面事件原样搬运为用例入参（不搬运任何归因字段——
// proto 无 user_id 字段，归因在用例层由 Principal 落定）。
func incomingEvents(events []*clientv1.AnalyticsEvent) []appanalytics.IncomingEvent {
	out := make([]appanalytics.IncomingEvent, 0, len(events))
	for _, e := range events {
		if e == nil {
			continue
		}
		out = append(out, appanalytics.IncomingEvent{
			Name:       e.GetName(),
			OccurredAt: tsTimePtr(e.GetOccurredAt()),
			Props:      e.GetProps(),
			SessionID:  e.GetSessionId(),
		})
	}
	return out
}

// tsTimePtr 把 proto optional Timestamp 转 *time.Time（nil 保持 nil = 服务端
// now 缺省；presence 语义：显式零值时间戳视为已设置，进钳制窗判定）。
func tsTimePtr(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}
