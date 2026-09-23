package runtime

import (
	"context"
	"slices"
	"testing"

	grpcapiinterceptor "github.com/lynx-go/grpcapi/interceptor"
	"github.com/stretchr/testify/require"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// 本文件锁定 AuthService/VerifyToken（对外 token 校验，introspection）的
// 策略声明与两道拦截器行为：真实 PolicySet 下的 API key scope 门（读面
// users.read）与 protovalidate 形状校验（token required / 枚举值域）。

const verifyTokenMethod = "/torchwood.server.v1.AuthService/VerifyToken"

// stubVerifyValidator 是 interceptor.Validator 桩：任意合法 AuthnRequest 返回
// 固定 service principal（scope 由用例注入 Permissions）。
type stubVerifyValidator struct {
	principal *shared.Principal
}

func (s stubVerifyValidator) Authenticate(_ context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	if _, _, err := shared.ParseAuthnRequest(req); err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return s.principal, nil
}

func (stubVerifyValidator) ValidateToken(context.Context, string) (*shared.Principal, error) {
	return nil, status.Error(codes.Unauthenticated, "not implemented")
}

func (stubVerifyValidator) ValidateCredential(context.Context, string, shared.CredentialType) (*shared.Principal, error) {
	return nil, status.Error(codes.Unauthenticated, "not implemented")
}

func (stubVerifyValidator) ValidateAdminProjectAccess(context.Context, *shared.Principal) error {
	return nil
}

// TestAuthService_VerifyToken_Policy：新 RPC 在真实策略注册表中，且为
// SERVER 面读档（users.read、admin 角色不限）。
func TestAuthService_VerifyToken_Policy(t *testing.T) {
	t.Parallel()
	set, err := ProvideMethodPolicies()
	require.NoError(t, err)

	p, ok := set.Get(verifyTokenMethod)
	require.True(t, ok, "%s 必须在策略注册表（missing auth policy 会启动失败）", verifyTokenMethod)
	require.Equal(t, domainauth.AccessServer, p.Access)
	require.Empty(t, p.AdminRoles, "读面：admin 角色不限")
	require.NotNil(t, p.Scope)
	require.Equal(t, string(domainauth.ScopeUsers), p.Scope.Resource)
	require.Equal(t, domainauth.ScopeRead, p.Scope.Op)
	require.False(t, slices.Contains(p.RequestFields, "project_id"), "server 面请求体不得携带 project_id")
}

// TestAuthService_VerifyToken_APIKeyScopeGate：真实策略注册表 + 认证拦截器，
// 持 users.read / 通配符 scope 的 key 放行；scope 不足（users.write、空、
// 其他资源）→ PermissionDenied。
func TestAuthService_VerifyToken_APIKeyScopeGate(t *testing.T) {
	t.Parallel()
	set, err := ProvideMethodPolicies()
	require.NoError(t, err)

	cases := []struct {
		name    string
		scopes  []string
		allowed bool
	}{
		{"users.read allowed", []string{"users.read"}, true},
		{"users bare resource allowed", []string{"users"}, true},
		{"wildcard allowed", []string{"*"}, true},
		{"all allowed", []string{"all"}, true},
		{"users.write denied", []string{"users.write"}, false},
		{"other resource denied", []string{"storage.read"}, false},
		{"empty denied", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ic, err := interceptor.NewAuthInterceptor(stubVerifyValidator{principal: &shared.Principal{
				ActorKind:      shared.ActorKindService,
				CredentialType: shared.CredentialTypeAPIKey,
				ProjectID:      "proj-1",
				APIKeyID:       "key-1",
				Permissions:    tc.scopes,
			}}, set)
			require.NoError(t, err)

			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "test-key"))
			reached := false
			_, err = ic.UnaryAuthMiddleware(ctx, &serverv1.VerifyTokenRequest{Token: "t"}, &grpc.UnaryServerInfo{
				FullMethod: verifyTokenMethod,
			}, func(context.Context, any) (any, error) {
				reached = true
				return &serverv1.VerifyTokenResponse{}, nil
			})
			if tc.allowed {
				require.NoError(t, err)
				require.True(t, reached)
			} else {
				require.Equal(t, codes.PermissionDenied, status.Code(err))
				require.False(t, reached)
			}
		})
	}
}

// TestAuthService_VerifyToken_ShapeValidation：protovalidate 形状校验——
// 空 token → InvalidArgument（required）；未定义枚举值 → InvalidArgument。
// （校验拦截器实现换库 grpcapi/interceptor，调用面为库的 Unary()。）
func TestAuthService_VerifyToken_ShapeValidation(t *testing.T) {
	t.Parallel()
	v := grpcapiinterceptor.NewValidate()
	info := &grpc.UnaryServerInfo{FullMethod: verifyTokenMethod}

	_, err := v.Unary()(context.Background(),
		&serverv1.VerifyTokenRequest{Token: ""}, info,
		func(context.Context, any) (any, error) {
			t.Fatal("handler must not be reached on violation")
			return nil, nil
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "token: value is required", status.Convert(err).Message())

	_, err = v.Unary()(context.Background(),
		&serverv1.VerifyTokenRequest{Token: "t", Type: serverv1.VerifyCredentialType(99)}, info,
		func(context.Context, any) (any, error) {
			t.Fatal("handler must not be reached on violation")
			return nil, nil
		})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "未定义枚举值必须被拒绝（defined_only 默认开启）")

	// 合法请求透传。
	res, err := v.Unary()(context.Background(),
		&serverv1.VerifyTokenRequest{Token: "t", Type: serverv1.VerifyCredentialType_VERIFY_CREDENTIAL_TYPE_AUTO}, info,
		func(context.Context, any) (any, error) { return &serverv1.VerifyTokenResponse{}, nil })
	require.NoError(t, err)
	require.NotNil(t, res)
}
