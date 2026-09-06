package interceptor

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// limiterStub 是可编程的限流端口桩：按注入的错误序列依次返回。
type limiterStub struct {
	errs []error // 逐次返回；耗尽后保持最后一个
	idx  int
}

func (s *limiterStub) Allow(_ context.Context, _ string, _ int, _ time.Duration) error {
	if len(s.errs) == 0 {
		return nil
	}
	var err error
	if s.idx < len(s.errs) {
		err = s.errs[s.idx]
	} else {
		err = s.errs[len(s.errs)-1]
	}
	s.idx++
	return err
}

// TestRateLimitBreaker_OpensAfterSustainedFailures：Round4 J5-1——
// 持续报错时先拒绝若干次（fail-closed），达到阈值后熔断放行且不再触碰
// limiter，infra_error 指标同步递增；半开探测成功后恢复正常判定。
func TestRateLimitBreaker_OpensAfterSustainedFailures(t *testing.T) {
	internalErr := status.Error(codes.Internal, "rate limit check failed")
	stub := &limiterStub{errs: []error{internalErr, internalErr, internalErr, internalErr}}
	ic := NewRateLimitInterceptor(stub, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	beforeErrors := testutil.ToFloat64(RateLimitInfraErrorTotal)

	// 前 4 次：熔断未开，fail-closed 拒绝（Internal），limiter 均被真实调用。
	for i := 0; i < rateLimitBreakerFailThreshold-1; i++ {
		handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
		require.Error(t, err)
		require.Equal(t, codes.Internal, status.Code(err))
		require.False(t, handlerCalled)
	}
	require.Equal(t, rateLimitBreakerFailThreshold-1, stub.idx)
	require.InDelta(t, beforeErrors+rateLimitBreakerFailThreshold-1,
		testutil.ToFloat64(RateLimitInfraErrorTotal), 0.001)

	// 第 5 次（阈值）：触发熔断开启；本请求仍 fail-closed 拒绝。
	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)
	require.False(t, handlerCalled)
	require.True(t, ic.breakerPassing(), "达到阈值后熔断应进入放行态")

	// 熔断放行中：请求直接放行，limiter 不被调用（跳过 Redis 往返）。
	for i := 0; i < 3; i++ {
		handlerCalled, err = runRateLimitMiddleware(ic, ctx, rateLimitMethod())
		require.NoError(t, err)
		require.True(t, handlerCalled)
	}
	require.Equal(t, rateLimitBreakerFailThreshold, stub.idx, "熔断期间不得调用 limiter")
}

// TestRateLimitBreaker_HalfOpenRecovers：熔断短窗结束后以真实调用探测
// （半开）；成功即恢复 closed，回到正常判定（超限照常 429、带 RetryInfo）。
func TestRateLimitBreaker_HalfOpenRecovers(t *testing.T) {
	internalErr := status.Error(codes.Internal, "rate limit check failed")
	stub := &limiterStub{errs: []error{
		internalErr, internalErr, internalErr, internalErr, internalErr, // 触发熔断
	}}
	ic := NewRateLimitInterceptor(stub, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	for i := 0; i < rateLimitBreakerFailThreshold; i++ {
		_, _ = runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	}
	require.True(t, ic.breakerPassing())

	// 半开探测成功：恢复 closed。
	ic.brMu.Lock()
	ic.brOpenUntil = time.Now().Add(-time.Second) // 拨快：短窗已过
	ic.brMu.Unlock()
	stub.errs = append(stub.errs, nil)
	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)

	// 恢复后正常判定：窗口超限照常透传 ResourceExhausted + RetryInfo，
	// rejected 指标递增；不触发 infra_error。
	beforeRejected := testutil.ToFloat64(RateLimitRejectedTotal)
	beforeInfra := testutil.ToFloat64(RateLimitInfraErrorTotal)
	stub.errs = append(stub.errs, status.Error(codes.ResourceExhausted, "rate limit exceeded"))
	handlerCalled, err = runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)
	require.False(t, handlerCalled)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
	require.InDelta(t, beforeRejected+1, testutil.ToFloat64(RateLimitRejectedTotal), 0.001)
	require.InDelta(t, beforeInfra, testutil.ToFloat64(RateLimitInfraErrorTotal), 0.001,
		"正常拒绝不得计入 infra_error")
}

// TestRateLimitBreaker_HalfOpenProbeFailureReopens：半开探测失败（Redis 仍
// 故障）→ 重新进入熔断放行短窗，该探测请求本身维持 fail-closed 拒绝。
func TestRateLimitBreaker_HalfOpenProbeFailureReopens(t *testing.T) {
	internalErr := status.Error(codes.Internal, "rate limit check failed")
	stub := &limiterStub{errs: []error{
		internalErr, internalErr, internalErr, internalErr, internalErr,
	}}
	ic := NewRateLimitInterceptor(stub, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	for i := 0; i < rateLimitBreakerFailThreshold; i++ {
		_, _ = runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	}
	ic.brMu.Lock()
	ic.brOpenUntil = time.Now().Add(-time.Second)
	ic.brMu.Unlock()

	// 半开探测：真实调用仍失败 → 该请求拒绝，但熔断重新开启继续放行后续。
	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)
	require.False(t, handlerCalled)
	require.True(t, ic.breakerPassing())

	handlerCalled, err = runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
}

// TestRateLimitBreaker_SporadicFailuresDoNotTrip：零星错误（超出滑动窗口）
// 不触发熔断——每次失败后把首错时间拨出窗口再失败，计数始终重新开始。
func TestRateLimitBreaker_SporadicFailuresDoNotTrip(t *testing.T) {
	internalErr := status.Error(codes.Internal, "rate limit check failed")
	stub := &limiterStub{}
	ic := NewRateLimitInterceptor(stub, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	for i := 0; i < rateLimitBreakerFailThreshold*3; i++ {
		stub.errs = append(stub.errs, internalErr)
		_, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
		require.Error(t, err)
		require.False(t, ic.breakerPassing(), "窗口外零星错误不得触发熔断")
		ic.brMu.Lock()
		ic.brFirstFail = time.Now().Add(-2 * rateLimitBreakerFailWindow) // 拨老：下次失败重开窗口
		ic.brMu.Unlock()
	}
}
