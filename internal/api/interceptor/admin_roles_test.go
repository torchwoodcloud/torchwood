package interceptor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// G2-1/G2-2：adminRoleMethodRules 方法覆盖断言（与 proto RPC 清单逐一核对，
// 防止新增写方法漏登记——漏登的方法对 viewer/member 放行，构成提权）。
func TestAdminRoleMethodRules_FunctionsWriteMethodsRequireOwnerAdmin(t *testing.T) {
	t.Parallel()
	writeMethods := []string{
		"/torchwood.server.v1.FunctionsService/CreateFunction",
		"/torchwood.server.v1.FunctionsService/UpdateFunction",
		"/torchwood.server.v1.FunctionsService/DeleteFunction",
		"/torchwood.server.v1.FunctionsService/CreateDeployment",
		"/torchwood.server.v1.FunctionsService/DeleteDeployment",
		"/torchwood.server.v1.FunctionsService/SetVariables",
		"/torchwood.server.v1.FunctionsService/CreateExecution",
	}
	for _, m := range writeMethods {
		roles := adminRoleMethodRules[m]
		require.NotNil(t, roles, "functions 写方法 %s 必须登记 adminRoleMethodRules", m)
		require.Contains(t, roles, "owner", "%s 必须允许 owner", m)
		require.Contains(t, roles, "admin", "%s 必须允许 admin", m)
		require.NotContains(t, roles, "member", "%s 不得允许 member", m)
		require.NotContains(t, roles, "viewer", "%s 不得允许 viewer", m)
	}

	// 读方法（含返回掩码值的 GetVariables）viewer 可读：不得登记受限规则。
	for _, m := range []string{
		"/torchwood.server.v1.FunctionsService/ListRuntimes",
		"/torchwood.server.v1.FunctionsService/ListSpecifications",
		"/torchwood.server.v1.FunctionsService/ListFunctions",
		"/torchwood.server.v1.FunctionsService/GetFunction",
		"/torchwood.server.v1.FunctionsService/ListDeployments",
		"/torchwood.server.v1.FunctionsService/GetDeployment",
		"/torchwood.server.v1.FunctionsService/GetVariables",
		"/torchwood.server.v1.FunctionsService/ListExecutions",
		"/torchwood.server.v1.FunctionsService/GetExecution",
	} {
		require.NotContains(t, adminRoleMethodRules, m, "functions 读方法 %s 不得登记受限规则", m)
	}
}

func TestAdminRoleMethodRules_BusinessWriteMethodsAllowMember(t *testing.T) {
	t.Parallel()
	// CreateUser/CreateBucket 是业务写（member 可做）；UpdateProject 保持
	// 现有语义仅收 viewer；三者均拒绝 viewer。
	for _, m := range []string{
		"/torchwood.server.v1.UsersService/CreateUser",
		"/torchwood.server.v1.StorageService/CreateBucket",
		"/torchwood.server.v1.ProjectsService/UpdateProject",
	} {
		roles := adminRoleMethodRules[m]
		require.NotNil(t, roles, "%s 必须登记 adminRoleMethodRules", m)
		for _, allowed := range []string{"member", "owner", "admin"} {
			require.Contains(t, roles, allowed, "%s 必须允许 %s", m, allowed)
		}
		require.NotContains(t, roles, "viewer", "%s 不得允许 viewer", m)
	}

	// DeleteUserSession 是管理员操作，仅 owner/admin。
	roles := adminRoleMethodRules["/torchwood.server.v1.UsersService/DeleteUserSession"]
	require.NotNil(t, roles)
	require.Contains(t, roles, "owner")
	require.Contains(t, roles, "admin")
	require.NotContains(t, roles, "member")
	require.NotContains(t, roles, "viewer")

	// CreateProject/DeleteProject 是平台级资源，仅 owner/admin。
	for _, m := range []string{
		"/torchwood.server.v1.ProjectsService/CreateProject",
		"/torchwood.server.v1.ProjectsService/DeleteProject",
	} {
		roles := adminRoleMethodRules[m]
		require.NotNil(t, roles, "%s 必须登记 adminRoleMethodRules", m)
		require.Contains(t, roles, "owner")
		require.Contains(t, roles, "admin")
		require.NotContains(t, roles, "member")
		require.NotContains(t, roles, "viewer")
	}
}

// G2-1：viewer/member admin 调 Functions 全部写方法必须 PermissionDenied。
func TestAuthInterceptor_RejectsViewerOrMemberOnFunctionsWriteMethods(t *testing.T) {
	t.Parallel()
	methods := []string{
		"/torchwood.server.v1.FunctionsService/CreateFunction",
		"/torchwood.server.v1.FunctionsService/UpdateFunction",
		"/torchwood.server.v1.FunctionsService/DeleteFunction",
		"/torchwood.server.v1.FunctionsService/CreateDeployment",
		"/torchwood.server.v1.FunctionsService/DeleteDeployment",
		"/torchwood.server.v1.FunctionsService/SetVariables",
		"/torchwood.server.v1.FunctionsService/CreateExecution",
	}
	for _, role := range []string{"viewer", "member"} {
		for _, method := range methods {
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
				t.Fatalf("handler should not run for role %s on %s", role, method)
				return nil, nil
			})
			requirePermissionDenied(t, err)
		}
	}
}

// G2-1：owner/admin 调 Functions 写方法放行（含 GetVariables 读方法）。
func TestAuthInterceptor_AllowsOwnerAdminOnFunctionsWriteMethods(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"owner", "admin"} {
		for _, method := range []string{
			"/torchwood.server.v1.FunctionsService/CreateFunction",
			"/torchwood.server.v1.FunctionsService/UpdateFunction",
			"/torchwood.server.v1.FunctionsService/DeleteFunction",
			"/torchwood.server.v1.FunctionsService/CreateDeployment",
			"/torchwood.server.v1.FunctionsService/DeleteDeployment",
			"/torchwood.server.v1.FunctionsService/SetVariables",
			"/torchwood.server.v1.FunctionsService/CreateExecution",
			"/torchwood.server.v1.FunctionsService/GetVariables",
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

// G2-2：viewer 调业务写方法（CreateUser/CreateBucket/UpdateProject）与
// DeleteUserSession 必须 PermissionDenied。
func TestAuthInterceptor_RejectsViewerOnGatedWriteMethods(t *testing.T) {
	t.Parallel()
	methods := []string{
		"/torchwood.server.v1.UsersService/CreateUser",
		"/torchwood.server.v1.StorageService/CreateBucket",
		"/torchwood.server.v1.ProjectsService/UpdateProject",
		"/torchwood.server.v1.UsersService/DeleteUserSession",
	}
	for _, method := range methods {
		ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
			ActorKind:      shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeSession,
			Roles:          []string{"viewer"},
		}}, nil, []string{method}, nil)
		requireNoError(t, err)

		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
		_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
			FullMethod: method,
		}, func(context.Context, any) (any, error) {
			t.Fatalf("handler should not run on %s", method)
			return nil, nil
		})
		requirePermissionDenied(t, err)
	}
}

// G2-2：member 可写业务资源（CreateUser/CreateBucket/UpdateProject 放行），
// 但 DeleteUserSession（管理员操作）拒绝。
func TestAuthInterceptor_MemberAllowedOnBusinessWritesDeniedOnAdminOp(t *testing.T) {
	t.Parallel()
	for _, method := range []string{
		"/torchwood.server.v1.UsersService/CreateUser",
		"/torchwood.server.v1.StorageService/CreateBucket",
		"/torchwood.server.v1.ProjectsService/UpdateProject",
	} {
		ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
			ActorKind:      shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeSession,
			Roles:          []string{"member"},
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
		if err != nil {
			t.Fatalf("member 应可调 %s: %v", method, err)
		}
		if !called {
			t.Fatalf("expected handler to run on %s", method)
		}
	}

	ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
		ActorKind:      shared.ActorKindAdmin,
		CredentialType: shared.CredentialTypeSession,
		Roles:          []string{"member"},
	}}, nil, []string{"/torchwood.server.v1.UsersService/DeleteUserSession"}, nil)
	requireNoError(t, err)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
	_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
		FullMethod: "/torchwood.server.v1.UsersService/DeleteUserSession",
	}, func(context.Context, any) (any, error) {
		t.Fatal("handler should not run")
		return nil, nil
	})
	requirePermissionDenied(t, err)
}

// Round3 H1-1：apiKeyScopeRules 中每个 op=="write" 的 Server 写方法都必须已
// 登记 adminRoleMethodRules（漏登的写方法对 viewer/member 跳过角色检查，
// 构成提权）；读方法（op=="read"）不得出现在角色表（viewer 必须能读）。
func TestAdminRoleMethodRules_Coverage_AllWriteMethodsRegistered(t *testing.T) {
	t.Parallel()
	missing, extra := adminRoleWriteCoverageDiff(apiKeyScopeRules, adminRoleMethodRules)
	require.Empty(t, missing, "写方法未登记角色表（viewer 可越权）: %v", missing)
	require.Empty(t, extra, "角色表登记了读方法或未映射方法: %v", extra)

	// 角色表允许角色均不包含 viewer；写方法允许角色非空。
	for m, roles := range adminRoleMethodRules {
		require.NotEmpty(t, roles, "%s 必须声明允许角色", m)
		require.NotContains(t, roles, "viewer", "%s 不得允许 viewer", m)
	}
}

// BillingService 全部只读：不得进角色写表（viewer 可读）。
func TestAdminRoleMethodRules_BillingServiceReadMethodsNotRegistered(t *testing.T) {
	t.Parallel()
	for _, m := range []string{
		"/torchwood.server.v1.BillingService/GetUsage",
		"/torchwood.server.v1.BillingService/ListRollups",
		"/torchwood.server.v1.BillingService/ListStatements",
	} {
		require.NotContains(t, adminRoleMethodRules, m, "billing 读方法 %s 不得登记受限规则", m)
		rule, ok := apiKeyScopeRules[m]
		require.True(t, ok, "%s 必须登记 apiKeyScopeRules", m)
		require.Equal(t, "billing", rule.resource)
		require.Equal(t, "read", rule.op)
	}
}

func TestAdminRoleMethodRules_SubscriptionsWriteMethods(t *testing.T) {
	t.Parallel()
	for _, m := range []string{
		"/torchwood.server.v1.SubscriptionsService/CreatePlan",
		"/torchwood.server.v1.SubscriptionsService/UpdatePlan",
		"/torchwood.server.v1.SubscriptionsService/DeletePlan",
	} {
		roles := adminRoleMethodRules[m]
		require.NotNil(t, roles, "%s 必须登记", m)
		require.Contains(t, roles, "member")
		require.NotContains(t, roles, "viewer")
	}
	for _, m := range []string{
		"/torchwood.server.v1.SubscriptionsService/CancelSubscription",
		"/torchwood.server.v1.SubscriptionsService/ExpireSubscription",
	} {
		roles := adminRoleMethodRules[m]
		require.NotNil(t, roles, "%s 必须登记", m)
		require.Contains(t, roles, "owner")
		require.Contains(t, roles, "admin")
		require.NotContains(t, roles, "member")
	}
	require.NotContains(t, adminRoleMethodRules, "/torchwood.server.v1.SubscriptionsService/ListPlans")
	require.True(t, APIKeyScopeAllowed("/torchwood.server.v1.SubscriptionsService/ListPlans", []string{"subscriptions.read"}))
	require.True(t, APIKeyScopeAllowed("/torchwood.server.v1.SubscriptionsService/CreatePlan", []string{"subscriptions.write"}))
	require.False(t, APIKeyScopeAllowed("/torchwood.server.v1.SubscriptionsService/CreatePlan", []string{"subscriptions.read"}))
}

// AssertAdminRoleWriteCoverage 的纯函数 diff：缺失一条写方法 / 多登记一条
// 读方法 / 多登记未映射方法，都必须被检出并列出方法名。
func TestAdminRoleWriteCoverageDiff_DetectsMissingAndExtra(t *testing.T) {
	t.Parallel()
	scope := map[string]apiKeyScopeRule{
		"/s/A": {"users", "write"},
		"/s/B": {"users", "write"},
		"/s/C": {"users", "read"},
	}
	role := map[string][]string{
		"/s/A": {"owner"},
		"/s/C": {"member"}, // 读方法误登记 → extra
		"/s/D": {"owner"},  // scope 表不存在 → extra
	}

	missing, extra := adminRoleWriteCoverageDiff(scope, role)
	require.Equal(t, []string{"/s/B"}, missing, "缺失的写方法必须列出")
	require.Equal(t, []string{"/s/C", "/s/D"}, extra, "读方法/未映射方法必须列出")
}

// Round3 H1-1：viewer 调全部补登的写方法（DeleteAPIKey/UpdateUser/
// CreateDocument/CreateGroup/DeleteBucket/CreateFileToken）必须 PermissionDenied。
func TestAuthInterceptor_RejectsViewerOnNewlyRegisteredWrites(t *testing.T) {
	t.Parallel()
	methods := []string{
		"/torchwood.server.v1.APIKeysService/DeleteAPIKey",
		"/torchwood.server.v1.UsersService/UpdateUser",
		"/torchwood.server.v1.DatabasesService/CreateDocument",
		"/torchwood.server.v1.GroupsService/CreateGroup",
		"/torchwood.server.v1.StorageService/DeleteBucket",
		"/torchwood.server.v1.StorageService/CreateFileToken",
	}
	for _, method := range methods {
		ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
			ActorKind:      shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeSession,
			Roles:          []string{"viewer"},
		}}, nil, []string{method}, nil)
		requireNoError(t, err)

		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
		_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
			FullMethod: method,
		}, func(context.Context, any) (any, error) {
			t.Fatalf("handler should not run for viewer on %s", method)
			return nil, nil
		})
		requirePermissionDenied(t, err)
	}
}

// Round3 H1-1：member 调接管面写（DeleteAPIKey/UpdateUser）必须
// PermissionDenied；调业务写（CreateDocument/CreateGroup）过拦截器。
func TestAuthInterceptor_MemberDeniedOnTakeoverWritesAllowedOnBusinessWrites(t *testing.T) {
	t.Parallel()
	for _, method := range []string{
		"/torchwood.server.v1.APIKeysService/DeleteAPIKey",
		"/torchwood.server.v1.UsersService/UpdateUser",
	} {
		ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
			ActorKind:      shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeSession,
			Roles:          []string{"member"},
		}}, nil, []string{method}, nil)
		requireNoError(t, err)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Session admin-token"))
		_, err = ic.UnaryAuthMiddleware(ctx, nil, &grpc.UnaryServerInfo{
			FullMethod: method,
		}, func(context.Context, any) (any, error) {
			t.Fatalf("handler should not run for member on %s", method)
			return nil, nil
		})
		requirePermissionDenied(t, err)
	}

	for _, method := range []string{
		"/torchwood.server.v1.DatabasesService/CreateDocument",
		"/torchwood.server.v1.GroupsService/CreateGroup",
	} {
		ic, err := NewAuthInterceptor(stubValidator{principal: &shared.Principal{
			ActorKind:      shared.ActorKindAdmin,
			CredentialType: shared.CredentialTypeSession,
			Roles:          []string{"member"},
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
		require.NoError(t, err, "member 应可调 %s（业务写）", method)
		if !called {
			t.Fatalf("expected handler to run on %s", method)
		}
	}
}
