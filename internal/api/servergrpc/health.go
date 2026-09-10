package servergrpc

import (
	"context"

	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/pkg/buildinfo"
)

// HealthCheckers 是健康检查所需的最小依赖探测面（infra/health.Checkers 满足；
// 接口化消除 api→infra 直依赖，仿 realtime/handler.go 模式）。
type HealthCheckers interface {
	Details(ctx context.Context) []*serverv1.DependencyStatus
}

type HealthService struct {
	serverv1.UnimplementedHealthServiceServer
	checkers HealthCheckers
	info     buildinfo.BuildInfo
}

func NewHealthService(checkers HealthCheckers, info buildinfo.BuildInfo) *HealthService {
	return &HealthService{checkers: checkers, info: info}
}

// Check 并行探测各依赖，任一失败整体 status=unavailable（gRPC 返回码保持
// OK；503 语义由 /healthz/readiness 承担）。
func (s *HealthService) Check(ctx context.Context, _ *serverv1.HealthCheckRequest) (*serverv1.HealthCheckResponse, error) {
	deps := s.checkers.Details(ctx)
	status := "ok"
	for _, d := range deps {
		if d.GetStatus() != "ok" {
			status = "unavailable"
			break
		}
	}
	return &serverv1.HealthCheckResponse{Status: status, Dependencies: deps}, nil
}

func (s *HealthService) GetVersion(_ context.Context, _ *serverv1.GetVersionRequest) (*serverv1.GetVersionResponse, error) {
	return &serverv1.GetVersionResponse{
		Version: s.info.Version,
		Commit:  s.info.Commit,
		Date:    s.info.Date,
	}, nil
}
