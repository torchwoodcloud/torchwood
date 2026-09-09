package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
)

// BillingService 封装 Server API 的用量/账单只读查询（PR5）。
type BillingService struct {
	c   *Client
	api serverv1.BillingServiceClient
}

func (s *BillingService) GetUsage(ctx context.Context, req *serverv1.GetUsageRequest) (*serverv1.Usage, error) {
	return s.api.GetUsage(ctx, req)
}

func (s *BillingService) ListRollups(ctx context.Context, req *serverv1.ListRollupsRequest) (*serverv1.ListRollupsResponse, error) {
	return s.api.ListRollups(ctx, req)
}

func (s *BillingService) ListStatements(ctx context.Context, req *sharedv1.ListRequest) (*serverv1.ListStatementsResponse, error) {
	return s.api.ListStatements(ctx, req)
}
