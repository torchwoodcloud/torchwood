package functions

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func strPtr(s string) *string { return &s }

// platformAdminCtx 返回携带平台 admin principal 的上下文（G12 后 functions
// 写方法 use-case 层要求 server 写主体——admin 会话或 API key，测试需显式注入）。
func platformAdminCtx() context.Context {
	return contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID:         "admin-1",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
		UserID:          "admin-1",
		Roles:           []string{"owner"},
	})
}

// G12（产品决策 B）：functions 写方法 use-case 层守卫为 RequireServerPrincipal——
// console admin 会话（viewer/member 的角色细粒度由拦截器 adminRoleMethodRules
// 把关）与 API key（scope 由 apiKeyScopeRules 把关）放行；端用户 PermissionDenied、
// 匿名 Unauthenticated。
func TestFunctionsWriteMethods_RequireServerPrincipal(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())

	endUser := &shared.Principal{ActorID: "user-1", ActorKind: shared.ActorKindEndUser, UserID: "user-1"}
	ctx := contexts.WithPrincipal(context.Background(), endUser)
	base := CreateFunctionCommand{ID: "fn_new", ProjectID: "p1", Name: "f", Runtime: "node-18.0", TimeoutSeconds: timeoutPtr(15)}

	_, err := uc.CreateFunction(ctx, base)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 CreateFunction 应被拒")

	_, err = uc.UpdateFunction(ctx, UpdateFunctionCommand{ProjectID: "p1", FunctionID: "fn_1", Name: strPtr("x")})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 UpdateFunction 应被拒")

	err = uc.DeleteFunction(ctx, "p1", "fn_1")
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 DeleteFunction 应被拒")

	_, err = uc.CreateDeployment(ctx, CreateDeploymentCommand{ProjectID: "p1", FunctionID: "fn_1", Code: []byte("PK\x03\x04code")})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 CreateDeployment 应被拒")

	err = uc.DeleteDeployment(ctx, "p1", "fn_1", "dep_ready")
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 DeleteDeployment 应被拒")

	_, err = uc.SetVariables(ctx, "p1", "fn_1", map[string]string{"A": "1"})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 SetVariables 应被拒")

	_, err = uc.CreateExecution(ctx, CreateExecutionCommand{ProjectID: "p1", FunctionID: "fn_1"})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 CreateExecution 应被拒")

	// 匿名（无 principal）→ Unauthenticated。
	_, err = uc.CreateFunction(context.Background(), base)
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// 放行主体：API key（service）与各角色 admin 会话均通过守卫、进入业务校验
	// （无效 ID 返回 InvalidArgument 证明守卫已过；viewer/member 由拦截器把关）。
	allowed := []*shared.Principal{
		{ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"}, Permissions: []string{"functions.write"}},
		{ActorID: "admin-2", ActorKind: shared.ActorKindAdmin, UserID: "admin-2", Roles: []string{"viewer"}},
		{ActorID: "admin-3", ActorKind: shared.ActorKindAdmin, UserID: "admin-3", Roles: []string{"member"}},
	}
	for _, p := range allowed {
		actx := contexts.WithPrincipal(context.Background(), p)
		_, err := uc.CreateFunction(actx, CreateFunctionCommand{ID: "bad id!", ProjectID: "p1", Name: "f", Runtime: "node-18.0"})
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%+v 应通过守卫进入业务校验", p)
	}

	// 平台 admin 放行（既有语义保持）。
	_, err = uc.CreateFunction(platformAdminCtx(), CreateFunctionCommand{ID: "bad id!", ProjectID: "p1", Name: "f", Runtime: "node-18.0"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

// 读方法（GetVariables/GetDeployment/List）不受写守卫影响，受限主体仍可读。
func TestFunctionsReadMethods_NotGatedByPlatformAdmin(t *testing.T) {
	repo := newMockRepo()
	seedReadyFunction(repo, "p1", "fn_1", true, 15)
	uc := newTestUC(newMockExecutor(nil, nil), repo, newMockQueue())
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "admin-2", ActorKind: shared.ActorKindAdmin, UserID: "admin-2", Roles: []string{"viewer"},
	})

	_, err := uc.GetVariables(ctx, "p1", "fn_1")
	require.NoError(t, err, "GetVariables 返回掩码值，viewer 可读")

	_, err = uc.GetDeployment(ctx, "p1", "fn_1", "dep_ready")
	require.NoError(t, err, "GetDeployment viewer 可读")

	fns, err := uc.ListFunctions(ctx, "p1")
	require.NoError(t, err)
	require.Len(t, fns, 1)
}
