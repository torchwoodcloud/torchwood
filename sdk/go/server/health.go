package server

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
)

// HealthService 封装 Server API 的 Health 服务。
type HealthService struct {
	c   *Client
	api serverv1.HealthServiceClient
}

// Check 返回服务健康状态。
func (h *HealthService) Check(ctx context.Context) (*serverv1.HealthCheckResponse, error) {
	return h.api.Check(ctx, &serverv1.HealthCheckRequest{})
}

// GetVersion 返回服务版本信息（与 proto RPC 名一致）。
func (h *HealthService) GetVersion(ctx context.Context) (*serverv1.GetVersionResponse, error) {
	return h.api.GetVersion(ctx, &serverv1.GetVersionRequest{})
}
