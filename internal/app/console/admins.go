package console

import (
	"context"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/password"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// AdminRole 是 admins.role 的合法取值。
const (
	AdminRoleOwner  = "owner"
	AdminRoleAdmin  = "admin"
	AdminRoleMember = "member"
	AdminRoleViewer = "viewer"
)

var validAdminRoles = map[string]struct{}{
	AdminRoleOwner:  {},
	AdminRoleAdmin:  {},
	AdminRoleMember: {},
	AdminRoleViewer: {},
}

// Admins 管理系统管理员；写操作的调用者身份（callerID）由 transport 层
// 从 principal 传入，use case 内完成业务级保护（最后 owner、自我保护）。
type Admins struct {
	repo             projects.AdminRepository
	adminProjectRepo projects.AdminProjectRepository
	// db 是工作单元缝（M5 C1）：改密/删除与凭证撤销必须同事务提交；
	// 未装配（部分单测）时退化为直接执行（单语句自动提交，语义不变）。
	db uow.Runner
}

func NewAdmins(repo projects.AdminRepository, adminProjectRepo projects.AdminProjectRepository, db uow.Runner) *Admins {
	return &Admins{repo: repo, adminProjectRepo: adminProjectRepo, db: db}
}

type CreateAdminCommand struct {
	Email    string
	Password string
	Role     string
}

type UpdateAdminCommand struct {
	ID       string
	CallerID string
	Role     string
	Password string // 非空则重置密码
}

func (a *Admins) List(ctx context.Context) ([]projects.Admin, error) {
	admins, err := a.repo.ListAdmins(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list admins: %v", err)
	}
	return admins, nil
}

func (a *Admins) Get(ctx context.Context, id string) (*projects.Admin, error) {
	admin, err := a.repo.GetAdmin(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get admin: %v", err)
	}
	if admin == nil {
		return nil, status.Error(codes.NotFound, "admin not found")
	}
	return admin, nil
}

func (a *Admins) Create(ctx context.Context, cmd CreateAdminCommand) (*projects.Admin, error) {
	// 纵深防御（G2-4/R04-P2-2）：管理员的增删改仅接受 admin actor，
	// 对齐 handler 层 requireAdminActor；防止绕过拦截器直接调用 use-case。
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return nil, err
	}
	email := normalizeAdminEmail(cmd.Email)
	if email == "" {
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if err := validateAdminRole(cmd.Role); err != nil {
		return nil, err
	}
	if err := users.ValidatePasswordStrength(cmd.Password); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	existing, err := a.repo.GetAdminByEmail(ctx, email)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check admin email: %v", err)
	}
	if existing != nil {
		return nil, status.Error(codes.AlreadyExists, "admin email already registered")
	}
	hash, err := password.Hash(cmd.Password)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hash password: %v", err)
	}
	now := time.Now()
	admin := &projects.Admin{
		ID:           idgen.UUID().String(),
		Email:        email,
		PasswordHash: hash,
		Role:         cmd.Role,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := a.repo.CreateAdmin(ctx, admin); err != nil {
		return nil, status.Errorf(codes.Internal, "create admin: %v", err)
	}
	// B1：若当前 principal 携带 ProjectID（Console header），对 member/viewer 授予该项目；owner/admin 仍全局，可不写行。
	if principal, ok := contexts.Principal(ctx); ok && principal.ProjectID != "" && a.adminProjectRepo != nil {
		if cmd.Role == AdminRoleMember || cmd.Role == AdminRoleViewer {
			if err := a.adminProjectRepo.GrantProjectAccess(ctx, admin.ID, principal.ProjectID); err != nil {
				return nil, status.Errorf(codes.Internal, "grant project access: %v", err)
			}
		}
	}
	return admin, nil
}

func (a *Admins) Update(ctx context.Context, cmd UpdateAdminCommand) (*projects.Admin, error) {
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return nil, err
	}
	if cmd.ID == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	admin, err := a.repo.GetAdmin(ctx, cmd.ID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get admin: %v", err)
	}
	if admin == nil {
		return nil, status.Error(codes.NotFound, "admin not found")
	}

	if cmd.Role != "" && cmd.Role != admin.Role {
		if err := validateAdminRole(cmd.Role); err != nil {
			return nil, err
		}
		if cmd.CallerID == admin.ID {
			return nil, status.Error(codes.InvalidArgument, "cannot change your own role")
		}
		// 防止把最后一个 owner 降级导致系统失去管理入口。
		if admin.Role == AdminRoleOwner {
			if err := a.ensureNotLastOwner(ctx, admin.ID); err != nil {
				return nil, err
			}
		}
		admin.Role = cmd.Role
	}

	if cmd.Password != "" {
		if err := users.ValidatePasswordStrength(cmd.Password); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		hash, err := password.Hash(cmd.Password)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "hash password: %v", err)
		}
		admin.PasswordHash = hash
	}

	admin.UpdatedAt = time.Now()
	// M5 C1：改密即撤销既有凭证——撤销时间戳与密码哈希同事务落库，消除
	// "密码已改但改密前签发的 access/refresh token 仍有效"窗口。
	// （iat <= revoked_at 即拒，正在同秒签发的 token 一并覆盖。）
	if err := a.runInTx(ctx, func(txCtx context.Context) error {
		if err := a.repo.UpdateAdmin(txCtx, admin); err != nil {
			return status.Errorf(codes.Internal, "update admin: %v", err)
		}
		if cmd.Password != "" {
			if err := a.repo.RevokeCredentials(txCtx, admin.ID, admin.UpdatedAt); err != nil {
				return status.Errorf(codes.Internal, "revoke admin credentials: %v", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return admin, nil
}

func (a *Admins) Delete(ctx context.Context, id, callerID string) error {
	if err := appshared.RequireConsolePrincipal(ctx); err != nil {
		return err
	}
	if id == "" {
		return status.Error(codes.InvalidArgument, "id is required")
	}
	admin, err := a.repo.GetAdmin(ctx, id)
	if err != nil {
		return status.Errorf(codes.Internal, "get admin: %v", err)
	}
	if admin == nil {
		return status.Error(codes.NotFound, "admin not found")
	}
	if callerID != "" && admin.ID == callerID {
		return status.Error(codes.InvalidArgument, "cannot delete your own account")
	}
	if admin.Role == AdminRoleOwner {
		if err := a.ensureNotLastOwner(ctx, admin.ID); err != nil {
			return err
		}
	}
	// M5 C1：删除前先落撤销痕迹再删行（同事务）。行删除本身已令后续验证
	// 以 admin not found 拒绝，撤销痕迹是纵深防御：保留不变量"管理面凭证
	// 生命周期动作必有持久撤销记录"，也为将来软删除演进兜底。
	if err := a.runInTx(ctx, func(txCtx context.Context) error {
		if err := a.repo.RevokeCredentials(txCtx, id, time.Now()); err != nil {
			return status.Errorf(codes.Internal, "revoke admin credentials: %v", err)
		}
		if err := a.repo.DeleteAdmin(txCtx, id); err != nil {
			return status.Errorf(codes.Internal, "delete admin: %v", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

// runInTx 在可用的工作单元内执行 fn；runner 未装配时直接执行
// （单语句自动提交，与事务内执行语义一致）。
func (a *Admins) runInTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if a.db == nil {
		return fn(ctx)
	}
	return a.db.Run(ctx, fn)
}

// ensureNotLastOwner 拒绝删除/降级系统中最后一个 owner。
func (a *Admins) ensureNotLastOwner(ctx context.Context, exceptID string) error {
	count, err := a.repo.CountAdminsByRole(ctx, AdminRoleOwner)
	if err != nil {
		return status.Errorf(codes.Internal, "count owners: %v", err)
	}
	if count <= 1 {
		return status.Error(codes.FailedPrecondition, "cannot remove the last owner")
	}
	return nil
}

func validateAdminRole(role string) error {
	if _, ok := validAdminRoles[role]; !ok {
		return status.Errorf(codes.InvalidArgument, "invalid role %q (allowed: owner, admin, member, viewer)", role)
	}
	return nil
}

func normalizeAdminEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
