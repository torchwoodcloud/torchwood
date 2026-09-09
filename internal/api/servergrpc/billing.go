package servergrpc

import (
	"context"
	"encoding/json"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	appbilling "github.com/torchwoodcloud/torchwood/internal/app/billing"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BillingService 是平台用量查询面 gRPC handler（薄：scope 在拦截器）。
type BillingService struct {
	serverv1.UnimplementedBillingServiceServer
	billing *appbilling.Billing
}

// NewBillingService constructs the server billing service.
func NewBillingService(billing *appbilling.Billing) *BillingService {
	return &BillingService{billing: billing}
}

func (s *BillingService) GetUsage(ctx context.Context, req *serverv1.GetUsageRequest) (*serverv1.Usage, error) {
	q := appbilling.UsageQuery{Metric: req.GetMetric()}
	if ts := req.GetPeriodStart(); ts != nil {
		q.PeriodStart = ts.AsTime()
	}
	if ts := req.GetPeriodEnd(); ts != nil {
		q.PeriodEnd = ts.AsTime()
	}
	out, err := s.billing.GetUsage(ctx, q)
	if err != nil {
		return nil, err
	}
	metrics := make([]*serverv1.UsageMetric, 0, len(out.Metrics))
	for name, value := range out.Metrics {
		metrics = append(metrics, &serverv1.UsageMetric{Metric: name, Value: value})
	}
	return &serverv1.Usage{
		ProjectId:   out.ProjectID,
		PeriodStart: timestamppb.New(out.PeriodStart),
		PeriodEnd:   timestamppb.New(out.PeriodEnd),
		Metrics:     metrics,
	}, nil
}

func (s *BillingService) ListRollups(ctx context.Context, req *serverv1.ListRollupsRequest) (*serverv1.ListRollupsResponse, error) {
	q := appbilling.ListRollupsQuery{
		Metric:    req.GetMetric(),
		Limit:     int(req.GetPageSize()),
		PageToken: req.GetPageToken(),
	}
	if ts := req.GetPeriodStart(); ts != nil {
		q.PeriodStart = ts.AsTime()
	}
	if ts := req.GetPeriodEnd(); ts != nil {
		q.PeriodEnd = ts.AsTime()
	}
	rows, next, err := s.billing.ListRollups(ctx, q)
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.UsageRollup, len(rows))
	for i := range rows {
		out[i] = mapUsageRollup(&rows[i])
	}
	return &serverv1.ListRollupsResponse{
		Rollups: out,
		Meta:    &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), NextPageToken: next},
	}, nil
}

func (s *BillingService) ListStatements(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListStatementsResponse, error) {
	rows, next, err := s.billing.ListStatements(ctx, int(req.GetPageSize()), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := make([]*serverv1.BillingStatement, len(rows))
	for i := range rows {
		mapped, err := mapBillingStatement(&rows[i])
		if err != nil {
			return nil, err
		}
		out[i] = mapped
	}
	return &serverv1.ListStatementsResponse{
		Statements: out,
		Meta:       &sharedv1.ListResponseMeta{PageSize: req.GetPageSize(), NextPageToken: next},
	}, nil
}

func mapUsageRollup(r *domainbilling.Rollup) *serverv1.UsageRollup {
	return &serverv1.UsageRollup{
		Id:          r.ID,
		ProjectId:   r.ProjectID,
		Metric:      r.Metric,
		PeriodStart: timestamppb.New(r.PeriodStart),
		Value:       r.Value,
	}
}

func mapBillingStatement(s *domainbilling.Statement) (*serverv1.BillingStatement, error) {
	if s == nil {
		return nil, status.Error(codes.NotFound, "statement not found")
	}
	raw, err := domainbilling.MarshalDetails(s.Details)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "encode statement details: %v", err)
	}
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, status.Errorf(codes.Internal, "decode statement details: %v", err)
		}
	}
	details, err := structpb.NewStruct(m)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "statement details: %v", err)
	}
	out := &serverv1.BillingStatement{
		Id:          s.ID,
		ProjectId:   s.ProjectID,
		PeriodStart: timestamppb.New(s.PeriodStart),
		PeriodEnd:   timestamppb.New(s.PeriodEnd),
		Status:      string(s.Status),
		Details:     details,
		CreatedAt:   timestamppb.New(s.CreatedAt),
	}
	if s.FinalizedAt != nil {
		out.FinalizedAt = timestamppb.New(*s.FinalizedAt)
	}
	return out, nil
}
