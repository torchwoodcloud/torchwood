package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// emptyProjectRepo 返回空项目仓库：GetProject 恒 nil（单元测试不依赖 DB，
// 守卫通过后以 NotFound 证明进入了业务路径）。
type emptyProjectRepo struct{}

func (f *emptyProjectRepo) CreateProject(context.Context, *projects.Project) error { return nil }
func (f *emptyProjectRepo) GetProject(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (f *emptyProjectRepo) GetProjectByName(context.Context, string) (*projects.Project, error) {
	return nil, nil
}
func (f *emptyProjectRepo) ListProjects(context.Context) ([]projects.Project, error)    { return nil, nil }
func (f *emptyProjectRepo) UpdateProject(context.Context, *projects.Project) error      { return nil }
func (f *emptyProjectRepo) DeleteProject(context.Context, string) error                 { return nil }
func (f *emptyProjectRepo) DeleteProjectControlPlaneRows(context.Context, string) error { return nil }

var _ projects.Repository = (*emptyProjectRepo)(nil)

// G2-2 纵深防御：CreateUser/DeleteUserSession 对匿名/端用户拒绝，
// console admin 会话与 API key 主体（Server API 写凭证）放行。
func TestUsers_WriteMethods_RequireServerPrincipal(t *testing.T) {
	uc := NewUsers(&emptyProjectRepo{}, nil, nil, nil, nil, nil, nil)

	denied := []*shared.Principal{
		{ActorID: "user-1", ActorKind: shared.ActorKindEndUser, UserID: "user-1"},
	}
	for _, p := range denied {
		ctx := contexts.WithPrincipal(context.Background(), p)
		_, err := uc.CreateUser(ctx, "p1", CreateUserCommand{Email: "a@b.c", Password: "Passw0rd"})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%+v CreateUser 应被拒", p)
		err = uc.DeleteUserSession(ctx, "p1", "u1", "s1")
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%+v DeleteUserSession 应被拒", p)
	}

	// 匿名（无 principal）→ Unauthenticated。
	_, err := uc.CreateUser(context.Background(), "p1", CreateUserCommand{Email: "a@b.c", Password: "Passw0rd"})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
	err = uc.DeleteUserSession(context.Background(), "p1", "u1", "s1")
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// console admin 会话（含受限 member/viewer——角色细粒度由拦截器把关）放行。
	_, err = uc.CreateUser(platformAdminCtx(context.Background()), "p1", CreateUserCommand{Email: "a@b.c", Password: "Passw0rd"})
	require.Equal(t, codes.NotFound, status.Code(err), "admin actor 应通过守卫，随后因项目不存在返回 NotFound")
	err = uc.DeleteUserSession(platformAdminCtx(context.Background()), "p1", "u1", "s1")
	require.Equal(t, codes.NotFound, status.Code(err))

	// API key（service actor）放行（scope 细粒度由拦截器把关）。
	keyCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
		Permissions: []string{"users.write"},
	})
	_, err = uc.CreateUser(keyCtx, "p1", CreateUserCommand{Email: "a@b.c", Password: "Passw0rd"})
	require.Equal(t, codes.NotFound, status.Code(err), "API key 应通过守卫，随后因项目不存在返回 NotFound")
	err = uc.DeleteUserSession(keyCtx, "p1", "u1", "s1")
	require.Equal(t, codes.NotFound, status.Code(err))
}

// Round3 H1-3：UpdateUser（可改 email/status 的接管面）纵深防御——端用户
// PermissionDenied、匿名 Unauthenticated；admin 会话与 API key 通过守卫进入
// 业务路径（项目不存在 → NotFound）。角色细粒度由拦截器 adminRoleMethodRules
// 把关（owner/admin）。
func TestUsers_UpdateUser_RequiresServerWriteActor(t *testing.T) {
	uc := NewUsers(&emptyProjectRepo{}, nil, nil, nil, nil, nil, nil)
	updates := map[string]any{"status": "active"}

	endUserCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "user-1", ActorKind: shared.ActorKindEndUser, UserID: "user-1",
	})
	_, err := uc.UpdateUser(endUserCtx, "p1", "u1", updates, databases.Principal{})
	require.Equal(t, codes.PermissionDenied, status.Code(err), "端用户 UpdateUser 应被拒")

	_, err = uc.UpdateUser(context.Background(), "p1", "u1", updates, databases.Principal{})
	require.Equal(t, codes.Unauthenticated, status.Code(err), "匿名 UpdateUser 应被拒")

	_, err = uc.UpdateUser(platformAdminCtx(context.Background()), "p1", "u1", updates, databases.Principal{})
	require.Equal(t, codes.NotFound, status.Code(err), "admin 会话应通过守卫，随后因项目不存在返回 NotFound")

	keyCtx := contexts.WithPrincipal(context.Background(), &shared.Principal{
		ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"},
		Permissions: []string{"users.write"},
	})
	_, err = uc.UpdateUser(keyCtx, "p1", "u1", updates, databases.Principal{})
	require.Equal(t, codes.NotFound, status.Code(err), "API key 应通过守卫，随后因项目不存在返回 NotFound")
}
