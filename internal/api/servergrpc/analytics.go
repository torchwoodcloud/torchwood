package servergrpc

import (
	"context"
	"encoding/json"
	"time"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appanalytics "github.com/torchwoodcloud/torchwood/internal/app/analytics"
	domainanalytics "github.com/torchwoodcloud/torchwood/internal/domain/analytics"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AnalyticsService 是事件分析的管理/查询面（docs/design/analytics.md §4.1）：
// server 面摄入（IngestEvents，PR2）+ 六个固定形状查询 RPC（PR3，D8）。
// 项目上下文来自凭证（API key 绑定项目 / admin 会话 X-Torchwood-Project），
// 请求体不携带 project_id。
type AnalyticsService struct {
	serverv1.UnimplementedAnalyticsServiceServer
	ingest *appanalytics.Ingest
	query  *appanalytics.Query
}

func NewAnalyticsService(ingest *appanalytics.Ingest, query *appanalytics.Query) *AnalyticsService {
	return &AnalyticsService{ingest: ingest, query: query}
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

// ---------------------------------------------------------------------------
// 查询面（PR3，D8 固定形状）：传输层只做 proto ↔ 命令/结果搬运，护栏与
// 择路在用例层。
// ---------------------------------------------------------------------------

// GetOverview 窗口 KPI + Top 事件（覆盖检测择路）+ 今日实时数（恒 raw）。
func (s *AnalyticsService) GetOverview(ctx context.Context, req *serverv1.GetAnalyticsOverviewRequest) (*serverv1.AnalyticsOverview, error) {
	res, err := s.query.GetOverview(ctx, appanalytics.OverviewCommand{
		ProjectID:   s.projectID(ctx),
		PeriodStart: req.GetPeriodStart().AsTime(),
		PeriodEnd:   req.GetPeriodEnd().AsTime(),
	})
	if err != nil {
		return nil, err
	}
	out := &serverv1.AnalyticsOverview{
		Kpi: &serverv1.AnalyticsOverviewKpi{
			TotalEvents:   res.Kpi.TotalEvents,
			UniqueUsers:   res.Kpi.UniqueUsers,
			NewUsers:      res.Kpi.NewUsers,
			EventsPerUser: res.Kpi.EventsPerUser,
		},
		TopEvents: make([]*serverv1.AnalyticsTopEvent, 0, len(res.TopEvents)),
		Today: &serverv1.AnalyticsTodayStats{
			TotalEvents: res.Today.TotalEvents,
			UniqueUsers: res.Today.UniqueUsers,
		},
		Source: res.Source,
	}
	for _, e := range res.TopEvents {
		out.TopEvents = append(out.TopEvents, &serverv1.AnalyticsTopEvent{Name: e.Name, Total: e.Total})
	}
	return out, nil
}

// ListEventDefinitions 事件字典分页（事后发现入口，D12）。
func (s *AnalyticsService) ListEventDefinitions(ctx context.Context, req *serverv1.ListEventDefinitionsRequest) (*serverv1.ListEventDefinitionsResponse, error) {
	res, err := s.query.ListEventDefinitions(ctx, appanalytics.ListDefinitionsCommand{
		ProjectID: s.projectID(ctx),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	})
	if err != nil {
		return nil, err
	}
	out := &serverv1.ListEventDefinitionsResponse{
		Definitions: make([]*serverv1.AnalyticsEventDefinition, 0, len(res.Definitions)),
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      res.PageSize,
			TotalCount:    res.TotalCount,
			NextPageToken: res.NextPageToken,
			PrevPageToken: res.PrevPageToken,
		},
	}
	for _, d := range res.Definitions {
		out.Definitions = append(out.Definitions, &serverv1.AnalyticsEventDefinition{
			Name:      d.Name,
			FirstSeen: timestamppb.New(d.FirstSeen),
			LastSeen:  timestamppb.New(d.LastSeen),
			Total_30D: d.Total30d,
		})
	}
	return out, nil
}

// QueryTimeseries 事件趋势（DAY 优先 rollup/覆盖回退 raw；HOUR 恒 raw）。
func (s *AnalyticsService) QueryTimeseries(ctx context.Context, req *serverv1.QueryTimeseriesRequest) (*serverv1.QueryTimeseriesResponse, error) {
	res, err := s.query.QueryTimeseries(ctx, appanalytics.TimeseriesCommand{
		ProjectID:   s.projectID(ctx),
		Names:       req.GetNames(),
		PeriodStart: req.GetPeriodStart().AsTime(),
		PeriodEnd:   req.GetPeriodEnd().AsTime(),
		Granularity: mapGranularity(req.GetGranularity()),
	})
	if err != nil {
		return nil, err
	}
	out := &serverv1.QueryTimeseriesResponse{
		Points: make([]*serverv1.AnalyticsTimeseriesPoint, 0, len(res.Points)),
		Source: res.Source,
	}
	for _, p := range res.Points {
		out.Points = append(out.Points, &serverv1.AnalyticsTimeseriesPoint{
			Bucket:      timestamppb.New(p.Bucket),
			Total:       p.Total,
			UniqueUsers: p.UniqueUsers,
		})
	}
	return out, nil
}

// QueryBreakdown 维度拆解（raw + Top-N + __other__，D9）。
func (s *AnalyticsService) QueryBreakdown(ctx context.Context, req *serverv1.QueryBreakdownRequest) (*serverv1.QueryBreakdownResponse, error) {
	res, err := s.query.QueryBreakdown(ctx, appanalytics.BreakdownCommand{
		ProjectID:   s.projectID(ctx),
		Name:        req.GetName(),
		PropKey:     req.GetPropKey(),
		PeriodStart: req.GetPeriodStart().AsTime(),
		PeriodEnd:   req.GetPeriodEnd().AsTime(),
		TopN:        req.GetTopN(),
	})
	if err != nil {
		return nil, err
	}
	out := &serverv1.QueryBreakdownResponse{
		Buckets: make([]*serverv1.AnalyticsBreakdownBucket, 0, len(res.Buckets)),
		Source:  res.Source,
	}
	for _, b := range res.Buckets {
		out.Buckets = append(out.Buckets, &serverv1.AnalyticsBreakdownBucket{
			Value:       b.Value,
			Total:       b.Total,
			UniqueUsers: b.UniqueUsers,
		})
	}
	return out, nil
}

// QueryRetention cohort × D0–D14 留存矩阵（D11；基座表空返回空矩阵）。
func (s *AnalyticsService) QueryRetention(ctx context.Context, req *serverv1.QueryRetentionRequest) (*serverv1.QueryRetentionResponse, error) {
	res, err := s.query.QueryRetention(ctx, appanalytics.RetentionCommand{
		ProjectID:   s.projectID(ctx),
		CohortStart: req.GetCohortStart().AsTime(),
		CohortEnd:   req.GetCohortEnd().AsTime(),
	})
	if err != nil {
		return nil, err
	}
	out := &serverv1.QueryRetentionResponse{
		Cohorts: make([]*serverv1.AnalyticsRetentionCohort, 0, len(res.Cohorts)),
		Source:  res.Source,
	}
	for _, c := range res.Cohorts {
		out.Cohorts = append(out.Cohorts, &serverv1.AnalyticsRetentionCohort{
			Cohort:   timestamppb.New(c.Cohort),
			Size:     c.Size,
			Retained: c.Retained,
		})
	}
	return out, nil
}

// ListUserEvents 用户行为轨迹（raw keyset 分页）。
func (s *AnalyticsService) ListUserEvents(ctx context.Context, req *serverv1.ListUserEventsRequest) (*serverv1.ListUserEventsResponse, error) {
	cmd := appanalytics.UserEventsCommand{
		ProjectID: s.projectID(ctx),
		UserID:    req.GetUserId(),
		PageSize:  req.GetPageSize(),
		PageToken: req.GetPageToken(),
	}
	if ts := req.GetPeriodStart(); ts != nil {
		t := ts.AsTime()
		cmd.PeriodStart = &t
	}
	if ts := req.GetPeriodEnd(); ts != nil {
		t := ts.AsTime()
		cmd.PeriodEnd = &t
	}
	res, err := s.query.ListUserEvents(ctx, cmd)
	if err != nil {
		return nil, err
	}
	out := &serverv1.ListUserEventsResponse{
		Events: make([]*serverv1.AnalyticsUserEvent, 0, len(res.Events)),
		Meta: &sharedv1.ListResponseMeta{
			PageSize:      res.PageSize,
			NextPageToken: res.NextPageToken,
		},
		Source: domainanalytics.QuerySourceRaw,
	}
	for _, e := range res.Events {
		out.Events = append(out.Events, &serverv1.AnalyticsUserEvent{
			Id:         e.ID,
			Name:       e.Name,
			OccurredAt: timestamppb.New(e.OccurredAt),
			IngestedAt: timestamppb.New(e.IngestedAt),
			Source:     e.Source,
			Platform:   e.Platform,
			AppVersion: e.AppVersion,
			SessionId:  e.SessionID,
			Props:      userEventProps(e.Props),
		})
	}
	return out, nil
}

// mapGranularity proto 枚举 → 领域粒度。
func mapGranularity(g serverv1.AnalyticsGranularity) domainanalytics.Granularity {
	switch g {
	case serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_HOUR:
		return domainanalytics.GranularityHour
	case serverv1.AnalyticsGranularity_ANALYTICS_GRANULARITY_DAY:
		return domainanalytics.GranularityDay
	default:
		return domainanalytics.GranularityUnspecified
	}
}

// userEventProps 把存储 props JSON（[]byte）转 proto Struct；空/解析失败
// 省略（props 非关键列，不因单行畸形拖垮整页）。
func userEventProps(raw []byte) *structpb.Struct {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	st, err := structpb.NewStruct(m)
	if err != nil {
		return nil
	}
	return st
}
