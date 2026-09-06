package interceptor

import (
	"context"
	"testing"

	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var _ Validator = (*auth.Validator)(nil)

type stubValidator struct {
	principal *shared.Principal
}

func (s stubValidator) Authenticate(_ context.Context, req shared.AuthnRequest) (*shared.Principal, error) {
	if _, _, err := shared.ParseAuthnRequest(req); err != nil {
		return nil, status.Error(codes.Unauthenticated, err.Error())
	}
	return s.principal, nil
}

func (s stubValidator) ValidateToken(_ context.Context, _ string) (*shared.Principal, error) {
	return s.principal, nil
}

func (s stubValidator) ValidateCredential(_ context.Context, _ string, _ shared.CredentialType) (*shared.Principal, error) {
	return s.principal, nil
}

func (s stubValidator) ValidateAdminProjectAccess(context.Context, *shared.Principal) error {
	return nil
}

func TestAuthInterceptor_RejectsAPIKeyOnUsersPermissionMethod(t *testing.T) {
	t.Parallel()

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		Roles:          []string{"keys"},
		Permissions:    []string{"projects.read"},
	}}, nil, nil, map[string][]string{
		"/torchwood.client.v1.GroupsService/CreateGroup": {"users"},
	})
	requireNoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "test-key"))
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
		FullMethod: "/torchwood.client.v1.GroupsService/CreateGroup",
	}, func(context.Context, any) (any, error) {
		t.Fatal("handler should not run")
		return nil, nil
	})
	requirePermissionDenied(t, err)
}

func TestAuthInterceptor_AllowsEndUserOnUsersPermissionMethod(t *testing.T) {
	t.Parallel()

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		UserID:    "user-1",
		Roles:     []string{"users", "user:user-1"},
	}}, nil, nil, map[string][]string{
		"/torchwood.client.v1.GroupsService/CreateGroup": {"users"},
	})
	requireNoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer token"))
	called := false
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
		FullMethod: "/torchwood.client.v1.GroupsService/CreateGroup",
	}, func(context.Context, any) (any, error) {
		called = true
		return "ok", nil
	})
	requireNoError(t, err)
	if !called {
		t.Fatal("expected handler to run")
	}
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func requirePermissionDenied(t *testing.T, err error) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("expected permission denied, got %v", err)
	}
}

func TestAuthInterceptor_DeniesAPIKeyOnAPIKeysService(t *testing.T) {
	t.Parallel()

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind:      shared.ActorKindService,
		CredentialType: shared.CredentialTypeAPIKey,
		Roles:          []string{"keys"},
		Permissions:    []string{"*"},
	}}, nil, []string{"/torchwood.server.v1.APIKeysService/CreateAPIKey"}, nil)
	requireNoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "test-key"))
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
		FullMethod: "/torchwood.server.v1.APIKeysService/CreateAPIKey",
	}, func(context.Context, any) (any, error) {
		t.Fatal("handler should not run")
		return nil, nil
	})
	requirePermissionDenied(t, err)
}

func TestAuthInterceptor_AllowsAdminSessionOnAPIKeysService(t *testing.T) {
	t.Parallel()

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind:      shared.ActorKindAdmin,
		CredentialType: shared.CredentialTypeSession,
		Roles:          []string{"admin"},
	}}, nil, []string{"/torchwood.server.v1.APIKeysService/CreateAPIKey"}, nil)
	requireNoError(t, err)

	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
	called := false
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
		FullMethod: "/torchwood.server.v1.APIKeysService/CreateAPIKey",
	}, func(context.Context, any) (any, error) {
		called = true
		return "ok", nil
	})
	requireNoError(t, err)
	if !called {
		t.Fatal("expected handler to run")
	}
}

// TestAuthInterceptor_RejectsAPIKeyWildcardScopeOnAdminsService（F2-1）：
// scope 为 * / all 的 API Key 调用 console AdminsService（permission 方法）
// 必须 PermissionDenied——API Key 只能经 apiKeyMethods 的 scope 门禁调用。
func TestAuthInterceptor_RejectsAPIKeyWildcardScopeOnAdminsService(t *testing.T) {
	t.Parallel()

	methods := []string{
		"/torchwood.console.v1.AdminsService/CreateAdmin",
		"/torchwood.console.v1.AdminsService/ListAdmins",
		"/torchwood.console.v1.AdminsService/UpdateAdmin",
		"/torchwood.console.v1.AdminsService/DeleteAdmin",
	}
	for _, scopes := range [][]string{{"*"}, {"all"}} {
		for _, method := range methods {
			ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
				ActorKind:      shared.ActorKindService,
				CredentialType: shared.CredentialTypeAPIKey,
				Roles:          []string{"keys"},
				Permissions:    scopes,
			}}, nil, nil, map[string][]string{method: {"owner"}})
			requireNoError(t, err)

			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-api-key", "test-key"))
			_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
				FullMethod: method,
			}, func(context.Context, any) (any, error) {
				t.Fatal("handler should not run")
				return nil, nil
			})
			requirePermissionDenied(t, err)
		}
	}
}

// TestAuthInterceptor_RejectsViewerOrMemberAdminOnWriteMethods（F2-2）：
// viewer/member 角色 admin 会话调用仅 owner/admin 的 Server API 写方法
// 必须 PermissionDenied。
func TestAuthInterceptor_RejectsViewerOrMemberAdminOnWriteMethods(t *testing.T) {
	t.Parallel()

	writeMethods := []string{
		"/torchwood.server.v1.APIKeysService/CreateAPIKey",
		"/torchwood.server.v1.UsersService/CreateUserToken",
		"/torchwood.server.v1.UsersService/UpdateUserPassword",
		"/torchwood.server.v1.UsersService/DeleteUser",
		"/torchwood.server.v1.DatabasesService/CreateDatabase",
		"/torchwood.server.v1.DatabasesService/UpdateCollection",
		"/torchwood.server.v1.DatabasesService/DeleteIndex",
		"/torchwood.server.v1.FunctionsService/SetVariables",
		"/torchwood.server.v1.OAuthProvidersService/UpsertOAuthProvider",
	}
	for _, role := range []string{"viewer", "member"} {
		for _, method := range writeMethods {
			ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
				ActorKind:      shared.ActorKindAdmin,
				CredentialType: shared.CredentialTypeSession,
				Roles:          []string{role},
			}}, nil, []string{method}, nil)
			requireNoError(t, err)

			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
			_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
				FullMethod: method,
			}, func(context.Context, any) (any, error) {
				t.Fatal("handler should not run")
				return nil, nil
			})
			requirePermissionDenied(t, err)
		}
	}
}

// TestAuthInterceptor_AllowsOwnerAdminOnWriteMethods（F2-2）：owner/admin 角色
// admin 会话调用同一批写方法放行。
func TestAuthInterceptor_AllowsOwnerAdminOnWriteMethods(t *testing.T) {
	t.Parallel()

	for _, role := range []string{"owner", "admin"} {
		for _, method := range []string{
			"/torchwood.server.v1.APIKeysService/CreateAPIKey",
			"/torchwood.server.v1.UsersService/CreateUserToken",
			"/torchwood.server.v1.DatabasesService/CreateDatabase",
			"/torchwood.server.v1.FunctionsService/SetVariables",
		} {
			ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
				ActorKind:      shared.ActorKindAdmin,
				CredentialType: shared.CredentialTypeSession,
				Roles:          []string{role},
			}}, nil, []string{method}, nil)
			requireNoError(t, err)

			ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
			called := false
			_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
				FullMethod: method,
			}, func(context.Context, any) (any, error) {
				called = true
				return "ok", nil
			})
			requireNoError(t, err)
			if !called {
				t.Fatalf("expected handler to run for role %s on %s", role, method)
			}
		}
	}
}

// TestAuthInterceptor_RejectsMultipleCredentials（F2-4）：同一请求携带多种凭证
// （Authorization + x-api-key / cookie）必须拒绝，防凭证混淆。
func TestAuthInterceptor_RejectsMultipleCredentials(t *testing.T) {
	t.Parallel()

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		UserID:    "user-1",
		Roles:     []string{"users"},
	}}, nil, nil, nil)
	requireNoError(t, err)

	for _, md := range []metadata.MD{
		metadata.Pairs("authorization", "Bearer token", "x-api-key", "key"),
		metadata.Pairs("authorization", "Bearer token", "cookie", "TORCHWOOD_session_console=abc"),
		metadata.Pairs("cookie", "TORCHWOOD_session_console=abc", "x-api-key", "key"),
	} {
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
			FullMethod: "/torchwood.server.v1.UsersService/ListUsers",
		}, func(context.Context, any) (any, error) {
			t.Fatal("handler should not run")
			return nil, nil
		})
		requireUnauthenticated(t, err)
	}
}

func requireUnauthenticated(t *testing.T, err error) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
}

// TestAuthInterceptor_RejectsSameKeyMultipleCredentials（R01-P2-2）：同一
// 凭证 key（authorization / x-api-key / cookie）出现多个值必须拒绝——
// 防止多值头部导致的解析歧义与注入。
func TestAuthInterceptor_RejectsSameKeyMultipleCredentials(t *testing.T) {
	t.Parallel()

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind: shared.ActorKindEndUser,
		UserID:    "user-1",
		Roles:     []string{"users"},
	}}, nil, nil, nil)
	requireNoError(t, err)

	for _, md := range []metadata.MD{
		metadata.Pairs("authorization", "Bearer a", "authorization", "Bearer b"),
		metadata.Pairs("x-api-key", "k1", "x-api-key", "k2"),
		metadata.Pairs("cookie", "TORCHWOOD_session_console=abc", "cookie", "TORCHWOOD_session_console=def"),
	} {
		ctx := metadata.NewIncomingContext(context.Background(), md)
		_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
			FullMethod: "/torchwood.server.v1.UsersService/ListUsers",
		}, func(context.Context, any) (any, error) {
			t.Fatal("handler should not run")
			return nil, nil
		})
		requireUnauthenticated(t, err)
	}
}
