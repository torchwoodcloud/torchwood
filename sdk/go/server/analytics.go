package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// AnalyticsService 封装 Server API 的事件分析面：服务端权威事件摄入
// （analytics.write）与固定形状查询（analytics.read；docs/design/analytics.md §4.1）。
type AnalyticsService struct {
	c   *Client
	api serverv1.AnalyticsServiceClient
}

// IngestEvents 服务端权威事件批量摄入（可信代报 user_id；部分接收语义，
// 响应含 accepted/skipped；at-least-once 不做去重）。
func (s *AnalyticsService) IngestEvents(ctx context.Context, req *serverv1.IngestServerEventsRequest) (*serverv1.IngestEventsResponse, error) {
	return s.api.IngestEvents(ctx, req)
}

// GetOverview 窗口 KPI + Top 事件 + 今日实时数（source 口径标注 rollup|raw）。
func (s *AnalyticsService) GetOverview(ctx context.Context, req *serverv1.GetAnalyticsOverviewRequest) (*serverv1.AnalyticsOverview, error) {
	return s.api.GetOverview(ctx, req)
}

// ListEventDefinitions 事件字典分页（自由上报 + 事后发现，D12）。
func (s *AnalyticsService) ListEventDefinitions(ctx context.Context, req *serverv1.ListEventDefinitionsRequest) (*serverv1.ListEventDefinitionsResponse, error) {
	return s.api.ListEventDefinitions(ctx, req)
}

// QueryTimeseries 事件趋势（HOUR → raw，DAY → rollup）。
func (s *AnalyticsService) QueryTimeseries(ctx context.Context, req *serverv1.QueryTimeseriesRequest) (*serverv1.QueryTimeseriesResponse, error) {
	return s.api.QueryTimeseries(ctx, req)
}

// QueryBreakdown 维度拆解（raw + Top-N + __other__ 归并）。
func (s *AnalyticsService) QueryBreakdown(ctx context.Context, req *serverv1.QueryBreakdownRequest) (*serverv1.QueryBreakdownResponse, error) {
	return s.api.QueryBreakdown(ctx, req)
}

// QueryRetention 留存矩阵（first_seen ⋈ user_days，cohort × D0–D14）。
func (s *AnalyticsService) QueryRetention(ctx context.Context, req *serverv1.QueryRetentionRequest) (*serverv1.QueryRetentionResponse, error) {
	return s.api.QueryRetention(ctx, req)
}

// ListUserEvents 用户行为轨迹下钻（keyset 分页）。
func (s *AnalyticsService) ListUserEvents(ctx context.Context, req *serverv1.ListUserEventsRequest) (*serverv1.ListUserEventsResponse, error) {
	return s.api.ListUserEvents(ctx, req)
}
