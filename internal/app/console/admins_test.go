package console_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/torchwooddev/torchwood/internal/app/console"
	"github.com/torchwooddev/torchwood/internal/domain/projects"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"github.com/torchwooddev/torchwood/pkg/password"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// adminActorCtx 返回携带 console admin actor principal 的上下文（G2-4 后
// Admins 写方法要求 admin actor，测试需显式注入）。
func adminActorCtx(ctx context.Context) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:   "admin-1",
		ActorKind: shared.ActorKindAdmin,
		AdminID:   "admin-1",
	})
}

type memAdminRepo struct {
	admins []projects.Admin
}

func (r *memAdminRepo) GetAdmin(_ context.Context, id string) (*projects.Admin, error) {
	for i := range r.admins {
		if r.admins[i].ID == id {
			return &r.admins[i], nil
		}
	}
	return nil, nil
}

func (r *memAdminRepo) GetAdminByEmail(_ context.Context, email string) (*projects.Admin, error) {
	for i := range r.admins {
		if r.admins[i].Email == email {
			return &r.admins[i], nil
		}
	}
	return nil, nil
}

func (r *memAdminRepo) ListAdmins(_ context.Context) ([]projects.Admin, error) {
	out := make([]projects.Admin, len(r.admins))
	copy(out, r.admins)
	return out, nil
}

func (r *memAdminRepo) CreateAdmin(_ context.Context, admin *projects.Admin) error {
	r.admins = append(r.admins, *admin)
	return nil
}

func (r *memAdminRepo) UpdateAdmin(_ context.Context, admin *projects.Admin) error {
	for i := range r.admins {
		if r.admins[i].ID == admin.ID {
			r.admins[i] = *admin
			return nil
		}
	}
	return nil
}

func (r *memAdminRepo) DeleteAdmin(_ context.Context, id string) error {
	for i := range r.admins {
		if r.admins[i].ID == id {
			r.admins = append(r.admins[:i], r.admins[i+1:]...)
			return nil
		}
	}
	return nil
}

func (r *memAdminRepo) CountAdminsByRole(_ context.Context, role string) (int64, error) {
	var n int64
	for _, a := range r.admins {
		if a.Role == role {
			n++
		}
	}
	return n, nil
}

func (r *memAdminRepo) WithBootstrapLock(_ context.Context, _ int64, fn func(ctx context.Context) error) error {
	return fn(context.Background())
}

var _ projects.AdminRepository = (*memAdminRepo)(nil)

func newAdminRepo(admins ...projects.Admin) *memAdminRepo {
	return &memAdminRepo{admins: admins}
}

func mkAdmin(id, email, role string) projects.Admin {
	now := time.Now()
	return projects.Admin{
		ID:           id,
		Email:        email,
		PasswordHash: "hash",
		Role:         role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestAdmins_Create_ValidatesInput(t *testing.T) {
	t.Parallel()
	uc := console.NewAdmins(newAdminRepo(), nil)

	_, err := uc.Create(adminActorCtx(context.Background()), console.CreateAdminCommand{Email: "", Password: "Passw0rd", Role: "owner"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = uc.Create(adminActorCtx(context.Background()), console.CreateAdminCommand{Email: "a@b.c", Password: "Passw0rd", Role: "superuser"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	_, err = uc.Create(adminActorCtx(context.Background()), console.CreateAdminCommand{Email: "a@b.c", Password: "short", Role: "owner"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestAdmins_Create_HashesPasswordAndNormalizesEmail(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo()
	uc := console.NewAdmins(repo, nil)

	created, err := uc.Create(adminActorCtx(context.Background()), console.CreateAdminCommand{
		Email: "  Ops@Example.COM ", Password: "Passw0rd", Role: "member",
	})
	require.NoError(t, err)
	require.Equal(t, "ops@example.com", created.Email)
	require.NotEqual(t, "Passw0rd", created.PasswordHash)
	ok, err := password.Verify("Passw0rd", created.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok)

	// Duplicate email.
	_, err = uc.Create(adminActorCtx(context.Background()), console.CreateAdminCommand{Email: "ops@example.com", Password: "Passw0rd", Role: "member"})
	require.Equal(t, codes.AlreadyExists, status.Code(err))
}

func TestAdmins_Update_RejectsSelfDemotion(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(mkAdmin("a1", "owner@x.com", "owner"), mkAdmin("a2", "admin@x.com", "admin"))
	uc := console.NewAdmins(repo, nil)

	_, err := uc.Update(adminActorCtx(context.Background()), console.UpdateAdminCommand{ID: "a1", CallerID: "a1", Role: "member"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestAdmins_Update_RejectsDemotingLastOwner(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(mkAdmin("a1", "owner@x.com", "owner"))
	uc := console.NewAdmins(repo, nil)

	_, err := uc.Update(adminActorCtx(context.Background()), console.UpdateAdminCommand{ID: "a1", CallerID: "a2", Role: "member"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestAdmins_Update_AllowsRoleChangeAndPasswordReset(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(
		mkAdmin("a1", "owner@x.com", "owner"),
		mkAdmin("a2", "admin@x.com", "admin"),
	)
	uc := console.NewAdmins(repo, nil)

	updated, err := uc.Update(adminActorCtx(context.Background()), console.UpdateAdminCommand{
		ID: "a2", CallerID: "a1", Role: "member", Password: "NewPassw0rd",
	})
	require.NoError(t, err)
	require.Equal(t, "member", updated.Role)
	ok, err := password.Verify("NewPassw0rd", updated.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok)
}

func TestAdmins_Delete_RejectsSelfDeletion(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(mkAdmin("a1", "owner@x.com", "owner"))
	uc := console.NewAdmins(repo, nil)

	err := uc.Delete(adminActorCtx(context.Background()), "a1", "a1")
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Len(t, repo.admins, 1)
}

func TestAdmins_Delete_RejectsDeletingLastOwner(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(mkAdmin("a1", "owner@x.com", "owner"))
	uc := console.NewAdmins(repo, nil)

	err := uc.Delete(adminActorCtx(context.Background()), "a1", "a2")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Len(t, repo.admins, 1)
}

func TestAdmins_Delete_AllowsWithSecondOwner(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(
		mkAdmin("a1", "owner1@x.com", "owner"),
		mkAdmin("a2", "owner2@x.com", "owner"),
		mkAdmin("a3", "admin@x.com", "admin"),
	)
	uc := console.NewAdmins(repo, nil)

	require.NoError(t, uc.Delete(adminActorCtx(context.Background()), "a3", "a1"))
	require.Len(t, repo.admins, 2)
}

// TestAdmins_WriteMethods_RequireConsolePrincipal（G2-4/R04-P2-2 纵深防御）：
// Create/Update/Delete 对非 admin actor（API key/端用户/匿名）一律
// PermissionDenied，对齐 handler 层 requireAdminActor。
func TestAdmins_WriteMethods_RequireConsolePrincipal(t *testing.T) {
	t.Parallel()
	repo := newAdminRepo(
		mkAdmin("a1", "owner@x.com", "owner"),
		mkAdmin("a2", "admin@x.com", "admin"),
	)
	uc := console.NewAdmins(repo, nil)

	denied := []*shared.Principal{
		{ActorID: "key-1", ActorKind: shared.ActorKindService, Roles: []string{"keys"}, Permissions: []string{"*"}},
		{ActorID: "user-1", ActorKind: shared.ActorKindEndUser, UserID: "user-1"},
	}
	for _, p := range denied {
		ctx := contexts.WithPrincipal(context.Background(), p)
		_, err := uc.Create(ctx, console.CreateAdminCommand{Email: "a@b.c", Password: "Passw0rd", Role: "member"})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%+v Create 应被拒", p)
		_, err = uc.Update(ctx, console.UpdateAdminCommand{ID: "a2", CallerID: "a1", Role: "member"})
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%+v Update 应被拒", p)
		err = uc.Delete(ctx, "a2", "a1")
		require.Equal(t, codes.PermissionDenied, status.Code(err), "%+v Delete 应被拒", p)
	}

	// 匿名 → PermissionDenied（与 handler 层 requireAdminActor 语义一致）。
	_, err := uc.Create(context.Background(), console.CreateAdminCommand{Email: "a@b.c", Password: "Passw0rd", Role: "member"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	err = uc.Delete(context.Background(), "a2", "a1")
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = uc.Update(context.Background(), console.UpdateAdminCommand{ID: "a2", CallerID: "a1", Role: "member"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// admin actor 放行进入业务校验（业务规则仍生效：改自己角色被拒）。
	_, err = uc.Update(adminActorCtx(context.Background()), console.UpdateAdminCommand{ID: "a1", CallerID: "a1", Role: "member"})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "admin actor 应通过 actor 守卫，随后触发自我保护业务规则")
}
