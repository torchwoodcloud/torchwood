package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Round3 H3-1：Databases schema DDL 用例层守卫与 G12 Functions 同口径——
// RequireServerPrincipal（console admin 会话 / API key 放行，角色细粒度由
// 拦截器 adminRoleMethodRules 把关；端用户 PermissionDenied、匿名
// Unauthenticated）。修复前 RequirePlatformPrincipal 会误伤持 databases.write
// 的 API key（ActorKind=service 必然被拒）。
func TestDatabases_DDLMethods_RequireServerPrincipal(t *testing.T) {
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)

	denied := []*shared.Principal{
		{ActorID: "user-1", ActorKind: shared.ActorKindEndUser, UserID: "user-1"},
	}
	for _, p := range denied {
		ctx := contexts.WithPrincipal(context.Background(), p)
		for _, call := range ddlCalls(uc, ctx) {
			require.Equal(t, codes.PermissionDenied, status.Code(call.fn()), "%+v 应被拒（%v）", p, call.name)
		}
	}

	// 匿名（无 principal）→ Unauthenticated。
	for _, call := range ddlCalls(uc, context.Background()) {
		require.Equal(t, codes.Unauthenticated, status.Code(call.fn()), "匿名应被拒（%v）", call.name)
	}

	// 放行主体：API key（service）与各角色 admin 会话均通过守卫进入业务路径
	// （错误码不得是守卫的 PermissionDenied/Unauthenticated；非法 id 等业务
	// 校验返回 InvalidArgument，DeleteCollection 直达 fake adapter 返回 OK；
	// viewer/member 角色细粒度由拦截器把关）。
	allowed := []*shared.Principal{
		{ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"}, Permissions: []string{"databases.write"}},
		{ActorID: "admin-2", ActorKind: shared.ActorKindAdmin, UserID: "admin-2", Roles: []string{"viewer"}},
		{ActorID: "admin-3", ActorKind: shared.ActorKindAdmin, UserID: "admin-3", Roles: []string{"member"}},
		{ActorID: "admin-4", ActorKind: shared.ActorKindAdmin, UserID: "admin-4", Roles: []string{"owner"}, IsPlatformAdmin: true},
	}
	for _, p := range allowed {
		ctx := contexts.WithPrincipal(context.Background(), p)
		for _, call := range ddlCalls(uc, ctx) {
			code := status.Code(call.fn())
			require.NotEqual(t, codes.PermissionDenied, code, "%+v 应通过守卫（%v）", p, call.name)
			require.NotEqual(t, codes.Unauthenticated, code, "%+v 应通过守卫（%v）", p, call.name)
		}
	}
}

// Round3 H3：守卫放开后 sentinel 库仍拒（不被 RequireServerPrincipal 的放行吞掉）。
func TestDatabases_DDLMethods_KeepSystemCollectionProtection(t *testing.T) {
	uc := NewDatabases(fakeProjectRepo{}, newFakeDocDB(), nil)
	ctx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
		Permissions: []string{"databases.write"},
	})

	// 对外 database_id 拒 sentinel（系统集合不经 Databases API）。
	err := uc.CreateCollection(ctx, "proj-1", databases.SystemDatabaseID, "users", "Users",
		nil, nil, nil, false)
	require.Equal(t, codes.InvalidArgument, status.Code(err), "sentinel 库创建集合必须被拒")

	err = uc.DeleteCollection(ctx, "proj-1", databases.SystemDatabaseID, "users")
	require.Equal(t, codes.InvalidArgument, status.Code(err), "sentinel 库删除集合必须被拒")

	err = uc.DeleteAttribute(ctx, "proj-1", databases.SystemDatabaseID, "users", "email")
	require.Equal(t, codes.InvalidArgument, status.Code(err), "sentinel 库删属性必须被拒")
}

// ddlCall 封装一次 DDL use-case 调用。
type ddlCall struct {
	name string
	fn   func() error
}

// ddlCalls 返回全部 9 个 schema DDL 入口的调用清单（统一使用非法 id 触发
// 守卫之后的业务校验错误）。
func ddlCalls(uc *Databases, ctx context.Context) []ddlCall {
	return []ddlCall{
		{"CreateDatabase", func() error {
			return uc.CreateDatabase(ctx, "proj-1", "bad id!", "App")
		}},
		{"DeleteDatabase", func() error {
			return uc.DeleteDatabase(ctx, "proj-1", "bad id!")
		}},
		{"CreateCollection", func() error {
			return uc.CreateCollection(ctx, "proj-1", "bad id!", "coll", "Coll", nil, nil, nil, false)
		}},
		{"UpdateCollection", func() error {
			return uc.UpdateCollection(ctx, "proj-1", "bad id!", "coll", databases.CollectionPatch{}, databases.Principal{Roles: []string{"keys"}})
		}},
		{"DeleteCollection", func() error {
			return uc.DeleteCollection(ctx, "proj-1", "bad id!", "coll")
		}},
		{"CreateAttribute", func() error {
			return uc.CreateAttribute(ctx, "proj-1", "bad id!", "coll", databases.Attribute{Key: "k", Type: "string"})
		}},
		{"DeleteAttribute", func() error {
			return uc.DeleteAttribute(ctx, "proj-1", "bad id!", "coll", "k")
		}},
		{"CreateIndex", func() error {
			return uc.CreateIndex(ctx, "proj-1", "bad id!", "coll", databases.Index{ID: "idx", Type: "key", Attributes: []string{"a"}})
		}},
		{"DeleteIndex", func() error {
			return uc.DeleteIndex(ctx, "proj-1", "bad id!", "coll", "idx")
		}},
	}
}
