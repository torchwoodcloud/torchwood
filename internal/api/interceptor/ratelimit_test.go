package interceptor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/anypb"
)

// rateLimitCall 记录一次 Allow 调用的参数。
type rateLimitCall struct {
	key    string
	limit  int
	window time.Duration
}

// rateLimitRecorder 是限流端口假实现：记录全部 Allow 调用并可注入返回错误。
type rateLimitRecorder struct {
	err   error
	calls []rateLimitCall
}

func (r *rateLimitRecorder) Allow(_ context.Context, key string, limit int, window time.Duration) error {
	r.calls = append(r.calls, rateLimitCall{key: key, limit: limit, window: window})
	return r.err
}

func rateLimitMethod() string { return "/torchwood.server.v1.UsersService/ListUsers" }

// runRateLimitMiddleware 执行一次限流中间件，返回 handler 是否被调用。
func runRateLimitMiddleware(ic *RateLimitInterceptor, ctx context.Context, method string) (bool, error) {
	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: method}
	_, err := ic.UnaryRateLimitMiddleware(ctx, nil, info, func(context.Context, any) (any, error) {
		handlerCalled = true
		return nil, nil
	})
	return handlerCalled, err
}

func rateLimitAppConfig(rl *config.Security_RateLimit) *config.AppConfig {
	return &config.AppConfig{Security: &config.Security{RateLimit: rl}}
}

func disabledRateLimitAppConfig() *config.AppConfig {
	enabled := false
	return rateLimitAppConfig(&config.Security_RateLimit{Enabled: &enabled})
}

// TestRateLimitInterceptor_UnauthenticatedUsesIP：未认证请求按 IP 维度
// 计数，使用默认阈值（300/60s），键为 api:ip:<ip>。
func TestRateLimitInterceptor_UnauthenticatedUsesIP(t *testing.T) {
	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
	require.Len(t, rec.calls, 1)
	require.Equal(t, "api:ip:203.0.113.7", rec.calls[0].key)
	require.Equal(t, defaultIPRateLimit, rec.calls[0].limit)
	require.Equal(t, defaultRateLimitWindow, rec.calls[0].window)
}

// TestRateLimitInterceptor_APIKeyTakesPrecedence：API Key principal 优先
// 于 user 维度（同一请求只按命中的一个维度计数）。
func TestRateLimitInterceptor_APIKeyTakesPrecedence(t *testing.T) {
	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	p := &shared.Principal{
		ActorID:        "key-1",
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		UserID:         "user-1",
		APIKeyID:       "key-1",
	}

	handlerCalled, err := runRateLimitMiddleware(ic, contexts.WithPrincipal(context.Background(), p), rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
	require.Len(t, rec.calls, 1)
	require.Equal(t, "api:apikey:key-1", rec.calls[0].key)
	require.Equal(t, defaultAPIKeyRateLimit, rec.calls[0].limit)
	require.Equal(t, defaultRateLimitWindow, rec.calls[0].window)
}

// TestRateLimitInterceptor_UserPrincipal：user/session principal 按 user
// 维度计数（键为 api:user:<actorID>），即使携带 ClientInfo 也不按 IP 计。
func TestRateLimitInterceptor_UserPrincipal(t *testing.T) {
	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	p := &shared.Principal{
		ActorID:        "user-1",
		ActorKind:      shared.ActorKindEndUser,
		CredentialType: shared.CredentialTypeSession,
		UserID:         "user-1",
	}
	ctx := contexts.WithClientInfo(contexts.WithPrincipal(context.Background(), p), contexts.ClientInfo{IP: "203.0.113.7"})

	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
	require.Len(t, rec.calls, 1)
	require.Equal(t, "api:user:user-1", rec.calls[0].key)
	require.Equal(t, defaultUserRateLimit, rec.calls[0].limit)
	require.Equal(t, defaultRateLimitWindow, rec.calls[0].window)
}

// TestRateLimitInterceptor_ResourceExhausted：端口返回 ResourceExhausted
// 时透传且 handler 不再执行（grpc-gateway 映射 HTTP 429）。
func TestRateLimitInterceptor_ResourceExhausted(t *testing.T) {
	rec := &rateLimitRecorder{err: status.Error(codes.ResourceExhausted, "rate limit exceeded")}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)
	require.False(t, handlerCalled)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))
}

// TestRateLimitInterceptor_RetryInfoFallback：端口返回的 ResourceExhausted
// 未携带 RetryInfo detail 时，拦截器补上以维度窗口为建议退避的 detail
// （Round4 J3-6）；非 ResourceExhausted 错误不加 detail。
func TestRateLimitInterceptor_RetryInfoFallback(t *testing.T) {
	rec := &rateLimitRecorder{err: status.Error(codes.ResourceExhausted, "rate limit exceeded")}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	_, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok)
	var ri *errdetails.RetryInfo
	for _, d := range st.Details() {
		if r, isRetry := d.(*errdetails.RetryInfo); isRetry {
			ri = r
		}
	}
	require.NotNil(t, ri, "ResourceExhausted 应携带 RetryInfo detail")
	require.Equal(t, defaultRateLimitWindow, ri.GetRetryDelay().AsDuration())

	// Internal（fail-closed）不附加限流 detail。
	rec.err = status.Error(codes.Internal, "rate limit check failed")
	_, err = runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)
	require.Empty(t, mustDetails(t, err))
}

// mustDetails 提取错误的 detail 列表（辅助断言）。
func mustDetails(t *testing.T, err error) []*anypb.Any {
	t.Helper()
	st, ok := status.FromError(err)
	require.True(t, ok)
	return st.Proto().Details
}

// TestRateLimitInterceptor_InternalFailClosed：Redis 故障（Internal）沿用
// 端口 fail-closed 语义透传，handler 不执行。
func TestRateLimitInterceptor_InternalFailClosed(t *testing.T) {
	rec := &rateLimitRecorder{err: status.Error(codes.Internal, "rate limit check failed")}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.Error(t, err)
	require.False(t, handlerCalled)
	require.Equal(t, codes.Internal, status.Code(err))
}

// TestRateLimitInterceptor_Disabled：enabled=false 时全部放行、不计数。
func TestRateLimitInterceptor_Disabled(t *testing.T) {
	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, disabledRateLimitAppConfig())
	ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
	require.Empty(t, rec.calls)
}

// TestRateLimitInterceptor_EnabledByDefault：未显式配置 enabled（含整个
// rate_limit 节缺失）时默认开启。
func TestRateLimitInterceptor_EnabledByDefault(t *testing.T) {
	for name, cfg := range map[string]*config.AppConfig{
		"no rate_limit section":   &config.AppConfig{},
		"section without enabled": rateLimitAppConfig(&config.Security_RateLimit{}),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &rateLimitRecorder{}
			ic := NewRateLimitInterceptor(rec, cfg)
			ctx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})

			_, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
			require.NoError(t, err)
			require.Len(t, rec.calls, 1)
		})
	}
}

// TestRateLimitInterceptor_ExemptMethods：gRPC 健康检查与 reflection 不限流。
func TestRateLimitInterceptor_ExemptMethods(t *testing.T) {
	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))

	for _, method := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.health.v1.Health/Watch",
		"/grpc.reflection.v1.ServerReflection/ServerReflectionInfo",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		handlerCalled, err := runRateLimitMiddleware(ic, context.Background(), method)
		require.NoError(t, err, method)
		require.True(t, handlerCalled, method)
	}
	require.Empty(t, rec.calls)
}

// TestRateLimitInterceptor_NoDimensionPassesThrough：无 principal 且解析
// 不到 IP 时不计数直接放行（计数交给上游/局部限流）。
func TestRateLimitInterceptor_NoDimensionPassesThrough(t *testing.T) {
	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))

	handlerCalled, err := runRateLimitMiddleware(ic, context.Background(), rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
	require.Empty(t, rec.calls)
}

// TestRateLimitInterceptor_ConfigOverrides：维度 limit/window 配置生效；
// 非法 window 回落默认值。
func TestRateLimitInterceptor_ConfigOverrides(t *testing.T) {
	rec := &rateLimitRecorder{}
	cfg := rateLimitAppConfig(&config.Security_RateLimit{
		Ip:   &config.Security_RateLimit_Dimension{Limit: 10, Window: "30s"},
		User: &config.Security_RateLimit_Dimension{Limit: 20, Window: "bogus"},
	})
	ic := NewRateLimitInterceptor(rec, cfg)

	ipCtx := contexts.WithClientInfo(context.Background(), contexts.ClientInfo{IP: "203.0.113.7"})
	_, err := runRateLimitMiddleware(ic, ipCtx, rateLimitMethod())
	require.NoError(t, err)

	userCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{ActorID: "user-1", UserID: "user-1"})
	_, err = runRateLimitMiddleware(ic, userCtx, rateLimitMethod())
	require.NoError(t, err)

	require.Len(t, rec.calls, 2)
	require.Equal(t, 10, rec.calls[0].limit)
	require.Equal(t, 30*time.Second, rec.calls[0].window)
	require.Equal(t, 20, rec.calls[1].limit)
	require.Equal(t, defaultRateLimitWindow, rec.calls[1].window)
}
