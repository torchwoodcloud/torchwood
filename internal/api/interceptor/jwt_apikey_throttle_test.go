package interceptor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// countingLimiter 是 RateLimiter 测试桩：记录每 key 的 Allow 次数，超过
// limit 后返回 ResourceExhausted（与 Redis 固定窗口语义同构）。
type countingLimiter struct {
	counts map[string]int
	limit  int
}

func newCountingLimiter(limit int) *countingLimiter {
	return &countingLimiter{counts: map[string]int{}, limit: limit}
}

func (c *countingLimiter) Allow(_ context.Context, key string, limit int, _ time.Duration) error {
	c.counts[key]++
	if c.counts[key] > limit {
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	return nil
}

// failingAPIKeyValidator 模拟 X-API-Key 哈希不匹配：携带 api key 一律
// Unauthenticated。
type failingAPIKeyValidator struct{}

func (failingAPIKeyValidator) Authenticate(_ context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	if _, _, err := shared.ParseAuthnRequest(req); err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return nil, status.Error(codes.Unauthenticated, "invalid or disabled api key")
}

func (failingAPIKeyValidator) ValidateToken(context.Context, string) (*shared.Principal, error) {
	return nil, status.Error(codes.Unauthenticated, "invalid token")
}

func (failingAPIKeyValidator) ValidateCredential(context.Context, string, shared.CredentialType) (*shared.Principal, error) {
	return nil, status.Error(codes.Unauthenticated, "invalid credential")
}

func (failingAPIKeyValidator) ValidateAdminProjectAccess(context.Context, *shared.Principal) error {
	return nil
}

const testAPIKeyMethod = "/torchwood.server.v1.UsersService/CreateUser"

func newAPIKeyThrottleInterceptor(t *testing.T, limiter domainauth.RateLimiter, limit int) *AuthInterceptor {
	t.Helper()
	set, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{{
		Method: testAPIKeyMethod, Service: serviceOf(testAPIKeyMethod),
		Access: domainauth.AccessServer,
		Scope:  &domainauth.ScopeRule{Resource: domainauth.ScopeUsers, Op: domainauth.ScopeWrite},
	}})
	require.NoError(t, err)
	ic, err := NewAuthInterceptor(failingAPIKeyValidator{}, set)
	require.NoError(t, err)
	return ic.WithAPIKeyFailThrottle(limiter, limit, time.Minute)
}

func apiKeyRequest(ctx context.Context, ic *AuthInterceptor, ip string) error {
	ctx = contexts.WithClientInfo(metadata.NewIncomingContext(ctx, metadata.Pairs("x-api-key", "sk-wrong")), contexts.ClientInfo{IP: ip})
	_, err := ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: testAPIKeyMethod},
		func(context.Context, any) (any, error) { return nil, errors.New("handler should not run") })
	return err
}

// TestAuthInterceptor_APIKeyFailThrottle（T-02 验收：key 认证连续失败触发
// 限速 429）：前 limit 次失败 401，第 limit+1 次 429；窗口内持续 429。
func TestAuthInterceptor_APIKeyFailThrottle(t *testing.T) {
	t.Parallel()

	limiter := newCountingLimiter(3)
	ic := newAPIKeyThrottleInterceptor(t, limiter, 3)

	for i := 1; i <= 3; i++ {
		err := apiKeyRequest(context.Background(), ic, "203.0.113.50")
		require.Equal(t, codes.Unauthenticated, status.Code(err), "第 %d 次失败应为 401", i)
	}
	err := apiKeyRequest(context.Background(), ic, "203.0.113.50")
	require.Equal(t, codes.ResourceExhausted, status.Code(err), "第 4 次失败应 429")
	require.Equal(t, codes.ResourceExhausted, status.Code(apiKeyRequest(context.Background(), ic, "203.0.113.50")))
	// 只按失败的 key 计数（3 次 401 + 2 次 429 = 5 次 Allow）。
	require.Equal(t, 5, limiter.counts["apikeyauth:ip:203.0.113.50"])
}

// TestAuthInterceptor_APIKeyFailThrottle_OtherIPUnaffected：换 IP 不受他人
// 失败影响；不带 IP 上下文时不计数不 429。
func TestAuthInterceptor_APIKeyFailThrottle_OtherIPUnaffected(t *testing.T) {
	t.Parallel()

	limiter := newCountingLimiter(2)
	ic := newAPIKeyThrottleInterceptor(t, limiter, 2)

	for i := 1; i <= 2; i++ {
		require.Equal(t, codes.Unauthenticated, status.Code(apiKeyRequest(context.Background(), ic, "203.0.113.51")), "IP-A 第 %d 次失败应为 401", i)
	}
	require.Equal(t, codes.ResourceExhausted, status.Code(apiKeyRequest(context.Background(), ic, "203.0.113.51")), "IP-A 第 3 次失败应 429")
	require.Equal(t, codes.Unauthenticated, status.Code(apiKeyRequest(context.Background(), ic, "203.0.113.52")), "IP-B 不受 IP-A 失败影响")

	// 无 IP 上下文：不计数、保持 401。
	noIP := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "k"))
	_, err := ic.UnaryAuthMiddleware(noIP, nil, &grpc.UnaryServerInfo{FullMethod: testAPIKeyMethod},
		func(context.Context, any) (any, error) { return nil, errors.New("handler should not run") })
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// TestAuthInterceptor_APIKeyFailThrottle_Disabled：未注入 limiter 时不启用
// （保持既有 401 行为）。
func TestAuthInterceptor_APIKeyFailThrottle_Disabled(t *testing.T) {
	t.Parallel()

	ic := newAPIKeyThrottleInterceptor(t, nil, 0)
	for i := 0; i < 20; i++ {
		require.Equal(t, codes.Unauthenticated, status.Code(apiKeyRequest(context.Background(), ic, "203.0.113.53")))
	}
}
