package interceptor

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	domainbilling "github.com/torchwooddev/torchwood/internal/domain/billing"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
)

type memMeter struct {
	mu   sync.Mutex
	incr map[string]int64
}

func (m *memMeter) Incr(_ context.Context, projectID, metric string, delta int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.incr == nil {
		m.incr = map[string]int64{}
	}
	m.incr[projectID+"|"+metric] += delta
	return nil
}

func (m *memMeter) get(projectID, metric string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.incr[projectID+"|"+metric]
}

func TestUsageInterceptorCountsProjectRPC(t *testing.T) {
	t.Parallel()
	meter := &memMeter{}
	u := NewUsageInterceptor(meter)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{ProjectID: "proj-a"})
	info := &grpc.UnaryServerInfo{FullMethod: "/torchwood.server.v1.UsersService/ListUsers"}
	_, err := u.UnaryUsageMiddleware(ctx, nil, info, func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), meter.get("proj-a", domainbilling.MetricAPICalls))
}

func TestUsageInterceptorSkipsHealthAndNoProject(t *testing.T) {
	t.Parallel()
	meter := &memMeter{}
	u := NewUsageInterceptor(meter)
	infoHealth := &grpc.UnaryServerInfo{FullMethod: "/grpc.health.v1.Health/Check"}
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{ProjectID: "proj-a"})
	_, err := u.UnaryUsageMiddleware(ctx, nil, infoHealth, func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	require.NoError(t, err)

	infoUsers := &grpc.UnaryServerInfo{FullMethod: "/torchwood.server.v1.UsersService/ListUsers"}
	_, err = u.UnaryUsageMiddleware(context.Background(), nil, infoUsers, func(ctx context.Context, req any) (any, error) {
		return "ok", nil
	})
	require.NoError(t, err)
	require.Equal(t, int64(0), meter.get("proj-a", domainbilling.MetricAPICalls))
}

func TestUsageExemptDoesNotIncludeRealtime(t *testing.T) {
	t.Parallel()
	// Realtime 不是 gRPC 方法；确认豁免名单不含任何 realtime 前缀以外的业务 RPC。
	require.False(t, usageExempt("/torchwood.server.v1.UsersService/ListUsers"))
	require.True(t, usageExempt("/grpc.health.v1.Health/Check"))
	require.True(t, usageExempt("/grpc.reflection.v1.ServerReflection/ServerReflectionInfo"))
}
