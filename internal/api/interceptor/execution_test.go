package interceptor

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// executionServerMethod / executionAssetsMethod 是测试用 full method：
// assetsMethod 带 assets.write 的 key scope 门（镜像资产写通道），
// usersPermMethod 是 END_USER permission 面（roles=["users"]）。
const (
	executionAssetsMethod = "/torchwood.server.v1.AssetsService/Grant"
	executionUsersMethod  = "/torchwood.client.v1.GroupsService/CreateGroup"
)

func newExecutionTestInterceptor(principal *shared.Principal) (*AuthInterceptor, error) {
	policies, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{
		{
			Method: executionAssetsMethod, Service: "/torchwood.server.v1.AssetsService",
			Access: domainauth.AccessServer,
			Scope:  &domainauth.ScopeRule{Resource: domainauth.ScopeAssets, Op: domainauth.ScopeWrite},
		},
		{
			Method: executionUsersMethod, Service: "/torchwood.client.v1.GroupsService",
			Access: domainauth.AccessPermission, Permissions: []string{"users"},
		},
	})
	if err != nil {
		return nil, err
	}
	return NewAuthInterceptor(stubValidator{principal: principal}, policies)
}

func executionPrincipal(perms ...string) *shared.Principal {
	return &shared.Principal{
		ActorID:        "fn_1",
		ActorKind:      shared.ActorKindExecution,
		CredentialType: shared.CredentialTypeExecution,
		ProjectID:      "p1",
		FunctionID:     "fn_1",
		ExecutionID:    "e1",
		Roles:          []string{"keys", "key:function:fn_1"},
		Permissions:    perms,
	}
}

// P0 执行身份接合点①：SERVER 面凭证门放行 execution principal。
func TestAuthInterceptor_ExecutionPrincipalPassesServerGate(t *testing.T) {
	t.Parallel()

	ic, err := newExecutionTestInterceptor(executionPrincipal("assets.write"))
	require.NoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer twx_token"))
	called := false
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: executionAssetsMethod},
		func(context.Context, any) (any, error) {
			called = true
			return "ok", nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

// 接合点②：scope 门对 execution principal 同样求值——缺 scope 403。
func TestAuthInterceptor_ExecutionPrincipalScopeGate(t *testing.T) {
	t.Parallel()

	ic, err := newExecutionTestInterceptor(executionPrincipal("databases.read"))
	require.NoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer twx_token"))
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: executionAssetsMethod},
		func(context.Context, any) (any, error) {
			t.Fatal("handler should not run")
			return nil, nil
		})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// 接合点⑤：END_USER permission 门对 execution principal 照旧拒绝
// （roles=["keys","key:function:<id>"] 不命中 users）。
func TestAuthInterceptor_ExecutionPrincipalRejectedOnEndUserMethod(t *testing.T) {
	t.Parallel()

	ic, err := newExecutionTestInterceptor(executionPrincipal("assets.write"))
	require.NoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer twx_token"))
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{FullMethod: executionUsersMethod},
		func(context.Context, any) (any, error) {
			t.Fatal("handler should not run")
			return nil, nil
		})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

// twx_ 前缀在凭证解析层即判定为 execution 凭证族（不进 JWT 解析）。
func TestParseAuthorizationHeader_ExecutionPrefix(t *testing.T) {
	t.Parallel()

	ct, token, ok := shared.ParseAuthorizationHeader("Bearer twx_abc123")
	require.True(t, ok)
	require.Equal(t, shared.CredentialTypeExecution, ct)
	require.Equal(t, "twx_abc123", token)

	// 普通 JWT 形态不受影响。
	ct, token, ok = shared.ParseAuthorizationHeader("Bearer eyJa.b.c")
	require.True(t, ok)
	require.Equal(t, shared.CredentialTypeToken, ct)
	require.Equal(t, "eyJa.b.c", token)

	// 非 bearer scheme 不误判。
	ct, _, ok = shared.ParseAuthorizationHeader("apikey twx_abc")
	require.True(t, ok)
	require.Equal(t, shared.CredentialTypeAPIKey, ct)
}

// 接合点③：限流维度——execution 在 user 回落之前特判，键 =
// api:execution:<project>:<function>，默认对齐 api-key 档。
func TestRateLimitInterceptor_ExecutionDimension(t *testing.T) {
	t.Parallel()

	rec := &rateLimitRecorder{}
	ic := NewRateLimitInterceptor(rec, rateLimitAppConfig(nil))
	p := executionPrincipal("assets.write")
	ctx := contexts.WithClientInfo(contexts.WithPrincipal(context.Background(), p), contexts.ClientInfo{IP: "203.0.113.7"})

	handlerCalled, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.True(t, handlerCalled)
	require.Len(t, rec.calls, 1)
	require.Equal(t, "api:execution:p1:fn_1", rec.calls[0].key)
	require.Equal(t, defaultExecutionRateLimit, rec.calls[0].limit)
	require.Equal(t, defaultRateLimitWindow, rec.calls[0].window)
}

// execution 维度可配（security.rate_limit.functions_execution 跟随既有机制）。
func TestRateLimitInterceptor_ExecutionDimensionConfigurable(t *testing.T) {
	t.Parallel()

	rec := &rateLimitRecorder{}
	cfg := rateLimitAppConfig(&config.Security_RateLimit{
		FunctionsExecution: &config.Security_RateLimit_Dimension{Limit: 42, Window: "30s"},
	})
	ic := NewRateLimitInterceptor(rec, cfg)
	ctx := contexts.WithPrincipal(context.Background(), executionPrincipal())

	_, err := runRateLimitMiddleware(ic, ctx, rateLimitMethod())
	require.NoError(t, err)
	require.Len(t, rec.calls, 1)
	require.Equal(t, "api:execution:p1:fn_1", rec.calls[0].key)
	require.Equal(t, 42, rec.calls[0].limit)
	require.Equal(t, 30*time.Second, rec.calls[0].window)
}
