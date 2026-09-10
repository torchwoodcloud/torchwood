package interceptor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// T-02 复扫补充发现修复:PUBLIC 面显式携带的无效 X-API-Key 不得静默降级为
// 匿名——401 + 失败限速计数;无凭证/无效 Bearer 保持匿名降级(公开页语义)。

const testPublicMethod = "/torchwood.client.v1.DatabasesService/ListDocuments"

func newPublicInterceptor(t *testing.T, limiter domainauth.RateLimiter, limit int) *AuthInterceptor {
	t.Helper()
	set, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{{
		Method: testPublicMethod, Service: serviceOf(testPublicMethod),
		Access: domainauth.AccessPublic,
	}})
	require.NoError(t, err)
	ic, err := NewAuthInterceptor(failingAPIKeyValidator{}, set)
	require.NoError(t, err)
	return ic.WithAPIKeyFailThrottle(limiter, limit, time.Minute)
}

func publicRequest(t *testing.T, ic *AuthInterceptor, md map[string][]string, ip string) (ran bool, err error) {
	t.Helper()
	ctx := contexts.WithClientInfo(metadata.NewIncomingContext(context.Background(), md), contexts.ClientInfo{IP: ip})
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testPublicMethod},
		func(context.Context, any) (any, error) { ran = true; return "ok", nil })
	return ran, err
}

// TestPublic_InvalidAPIKeyRejected:无效 X-API-Key → 401(不进 handler),且
// 计入失败限速(与 SERVER 面同一键空间)。
func TestPublic_InvalidAPIKeyRejected(t *testing.T) {
	t.Parallel()

	limiter := newCountingLimiter(100)
	ic := newPublicInterceptor(t, limiter, 100)

	ran, err := publicRequest(t, ic, map[string][]string{"x-api-key": {"sk-wrong"}}, "203.0.113.60")
	require.False(t, ran, "无效 key 不得进入 handler")
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	require.Equal(t, 1, limiter.counts["apikeyauth:ip:203.0.113.60"], "PUBLIC 面无效 key 必须计入失败限速")
}

// TestPublic_AnonymousStillAllowed:无凭证 → 匿名放行(公开读基线不变)。
func TestPublic_AnonymousStillAllowed(t *testing.T) {
	t.Parallel()

	limiter := newCountingLimiter(100)
	ic := newPublicInterceptor(t, limiter, 100)

	ran, err := publicRequest(t, ic, nil, "203.0.113.61")
	require.NoError(t, err)
	require.True(t, ran, "无凭证匿名必须照常放行")
	require.Empty(t, limiter.counts)
}

// TestPublic_InvalidBearerStillAnonymous:无效 Bearer(如过期会话)保持匿名
// 降级——不把公开页面上的过期凭证读者挡在门外。
func TestPublic_InvalidBearerStillAnonymous(t *testing.T) {
	t.Parallel()

	limiter := newCountingLimiter(100)
	ic := newPublicInterceptor(t, limiter, 100)

	ran, err := publicRequest(t, ic, map[string][]string{"authorization": {"Bearer stale-token"}}, "203.0.113.62")
	require.NoError(t, err, "无效 Bearer 必须降级匿名而非 401")
	require.True(t, ran)
	require.Empty(t, limiter.counts, "Bearer 失败不进入 key 失败限速")
}

// TestPublic_InvalidAPIKeyThrottled:连续无效 key 达到阈值 → 429(限速联动)。
func TestPublic_InvalidAPIKeyThrottled(t *testing.T) {
	t.Parallel()

	limiter := newCountingLimiter(3)
	ic := newPublicInterceptor(t, limiter, 3)

	for i := 1; i <= 3; i++ {
		ran, err := publicRequest(t, ic, map[string][]string{"x-api-key": {"sk-wrong"}}, "203.0.113.63")
		require.Equal(t, codes.Unauthenticated, status.Code(err), "第 %d 次失败应为 401", i)
		require.False(t, ran)
	}
	_, err := publicRequest(t, ic, map[string][]string{"x-api-key": {"sk-wrong"}}, "203.0.113.63")
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "第 4 次失败应 429")
}
