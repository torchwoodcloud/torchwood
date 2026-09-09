package servergrpc

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/infra/documentdb"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// setupRateLimitEnv 装配生产同款拦截器链（clientInfo → auth → rate limit →
// audit）+ 内存假限流器（不依赖真 Redis），返回 env 与测试 API Key secret。
func setupRateLimitEnv(t *testing.T) (*testutil.InterceptorEnv, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping integration test")
	}
	ctx := context.Background()
	db := testutil.SetupTestDB(t)
	t.Cleanup(func() { _ = db.Close() })

	projectID, _, cleanup := testutil.CreateTestProject(ctx, db)
	t.Cleanup(cleanup)

	docDB := documentdb.NewPostgresDocumentDB(db, nil)
	env, err := testutil.NewInterceptorEnv(db, &config.AppConfig{}, docDB)
	require.NoError(t, err)

	secret, dropKey := testutil.CreateTestAPIKey(ctx, db, projectID, nil)
	t.Cleanup(dropKey)
	return env, secret
}

// TestRateLimitInterceptor_APIKeyEndToEnd：API Key 认证请求全链路按
// api:apikey:<actorID> 维度计数，达到阈值后返回 ResourceExhausted
// （grpc-gateway 映射 HTTP 429）。
func TestRateLimitInterceptor_APIKeyEndToEnd(t *testing.T) {
	env, secret := setupRateLimitEnv(t)
	ctx := context.Background()

	env.RateLimiter.Limit = 2
	md := metadata.Pairs("x-api-key", secret)
	require.NoError(t, env.InvokeUnary(ctx, testutil.MethodListUsers, md))
	require.NoError(t, env.InvokeUnary(ctx, testutil.MethodListUsers, md))

	err := env.InvokeUnary(ctx, testutil.MethodListUsers, md)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	counts := env.RateLimiter.Counts()
	require.Len(t, counts, 1)
	for key := range counts {
		require.True(t, strings.HasPrefix(key, "api:apikey:"), "unexpected rate limit key %q", key)
	}
}

// TestRateLimitInterceptor_UnauthenticatedIPByHeader：未认证的 public 请求
// 按 IP 维度计数（clientInfo 在无 peer 的进程内调用退化为直接取转发头）。
func TestRateLimitInterceptor_UnauthenticatedIPByHeader(t *testing.T) {
	env, _ := setupRateLimitEnv(t)
	ctx := context.Background()

	env.RateLimiter.Limit = 1
	md := metadata.Pairs("x-forwarded-for", "198.51.100.9")
	require.NoError(t, env.InvokeUnary(ctx, testutil.MethodHealthCheck, md))

	err := env.InvokeUnary(ctx, testutil.MethodHealthCheck, md)
	require.Equal(t, codes.ResourceExhausted, status.Code(err))

	counts := env.RateLimiter.Counts()
	require.Len(t, counts, 1)
	for key := range counts {
		require.Equal(t, "api:ip:198.51.100.9", key)
	}
}
