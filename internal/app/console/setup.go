package console

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"strings"

	"github.com/torchwoodcloud/torchwood/internal/app/server"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/ident"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// bootstrapLockKey 是引导流程的 pg_advisory_xact_lock 常量键（"Torchwoo"
// 的 ASCII 编码），用于串行化首个管理员检查+创建。
const bootstrapLockKey = int64(0x546F726368776F6F)

// Setup 处理首个管理员 bootstrap：查询初始化状态（GetSetupStatus）与首个
// 管理员注册（SignUp）。SignUp 是公开端点（ConsoleAuthService 服务级
// ACCESS_PUBLIC），「仅首次可用 + 需 setup token」的保证必须在本 use-case
// 内完成，不依赖拦截器。
type Setup struct {
	setupToken       string
	admins           adminCreator
	projects         projectCreator
	auth             tokenIssuer
	adminRepo        projects.AdminRepository
	adminProjectRepo projects.AdminProjectRepository
	projectRepo      projects.Repository
}

// 以下接口由 internal/app 的对应 use-case 实现（构造参数为具体类型，
// 字段收窄为接口仅用于单元测试注入失败场景）：
//   - projectCreator: *server.Projects
//   - databaseCreator: *server.Databases
//   - adminCreator:   *Admins
//   - tokenIssuer:    *Auth
type projectCreator interface {
	CreateProjectInternal(ctx context.Context, cmd server.CreateProjectCommand) (*projects.Project, error)
	DeleteProjectInternal(ctx context.Context, id string) error
}

type adminCreator interface {
	Create(ctx context.Context, cmd CreateAdminCommand) (*projects.Admin, error)
}

type tokenIssuer interface {
	SignIn(ctx context.Context, cmd SignInCommand) (*TokenPair, error)
}

func NewSetup(
	cfg *config.AppConfig,
	admins *Admins,
	projects *server.Projects,
	auth *Auth,
	adminRepo projects.AdminRepository,
	adminProjectRepo projects.AdminProjectRepository,
	projectRepo projects.Repository,
) *Setup {
	setupToken := ""
	if cfg != nil && cfg.GetSecurity() != nil {
		setupToken = strings.TrimSpace(cfg.GetSecurity().GetSetupToken())
	}
	return &Setup{
		setupToken:       setupToken,
		admins:           admins,
		projects:         projects,
		auth:             auth,
		adminRepo:        adminRepo,
		adminProjectRepo: adminProjectRepo,
		projectRepo:      projectRepo,
	}
}

// GetSetupStatus 返回是否尚未初始化（admins 表为空即 needs_setup=true）。
func (s *Setup) GetSetupStatus(ctx context.Context) (bool, error) {
	admins, err := s.adminRepo.ListAdmins(ctx)
	if err != nil {
		return false, status.Errorf(codes.Internal, "check setup status: %v", err)
	}
	return len(admins) == 0, nil
}

// SetupTokenConfigured 返回部署方是否配置了引导令牌（security.setup_token）。
func (s *Setup) SetupTokenConfigured() bool {
	return s.setupToken != ""
}

type SignUpCommand struct {
	Email      string
	Password   string
	SetupToken string
	ProjectID  string
	DatabaseID string
}

type SignUpResult struct {
	Admin  *projects.Admin
	Tokens *TokenPair
}

// SignUp 注册首个管理员并完成引导：admin(owner) + 指定 project + 指定
// database + admin_projects 关联 + 签发 TokenPair。不创建 API Key，登录后
// 由 Console 的 API Key 页面生成。
// 任一后续步骤失败时 best-effort 回删已建资源，避免「admin 已建但 project
// 缺失」导致无法重试也无法登录的死锁；补偿失败只记日志。
func (s *Setup) SignUp(ctx context.Context, cmd SignUpCommand) (*SignUpResult, error) {
	// setup token 校验先于一切状态检查：未配置即拒绝（引导入口默认关闭），
	// 请求令牌与配置不一致同样拒绝，防止无凭据抢占首个 owner。
	if s.setupToken == "" {
		return nil, status.Error(codes.FailedPrecondition, "setup token is not configured; set TORCHWOOD_SECURITY_SETUP_TOKEN before bootstrapping")
	}
	if !setupTokenEqual(cmd.SetupToken, s.setupToken) {
		return nil, status.Error(codes.PermissionDenied, "invalid setup token")
	}

	projectID := strings.TrimSpace(cmd.ProjectID)
	databaseID := strings.TrimSpace(cmd.DatabaseID)
	if err := ident.ValidateSchemaResourceID(projectID); err != nil {
		return nil, status.Error(codes.InvalidArgument, "project_id must match ^[a-z][a-z0-9]{0,27}$")
	}
	if err := ident.ValidateSchemaResourceID(databaseID); err != nil {
		return nil, status.Error(codes.InvalidArgument, "database_id must match ^[a-z][a-z0-9]{0,27}$")
	}

	// 并发兜底：首次性检查与首个 admin 创建在 pg_advisory_xact_lock 事务内
	// 串行化——并发的 SignUp 只有一个能通过「admins 为空」检查，其余看到
	// 已提交的首个 admin 后被拒绝，杜绝抢占 owner。事务内（含后续 repo
	// 调用）通过注入的 ctx 复用同一连接。
	var (
		admin *projects.Admin
		err   error
	)
	if err := s.adminRepo.WithBootstrapLock(ctx, bootstrapLockKey, func(txCtx context.Context) error {
		admins, err := s.adminRepo.ListAdmins(txCtx)
		if err != nil {
			return status.Errorf(codes.Internal, "check setup state: %v", err)
		}
		if len(admins) > 0 {
			return status.Error(codes.FailedPrecondition, "setup already completed")
		}
		// Admins.Create 要求 admin actor（G2-4 纵深防御）；SignUp 是公开
		// 引导端点，由本 use-case 完成 setup token 授权后代表平台执行首个
		// owner 创建，故注入 bootstrap principal 表明授权来源。
		admin, err = s.admins.Create(bootstrapPrincipalCtx(txCtx), CreateAdminCommand{
			Email:    cmd.Email,
			Password: cmd.Password,
			Role:     AdminRoleOwner,
		})
		return err
	}); err != nil {
		return nil, err
	}

	var project *projects.Project
	rollback := func() {
		if project != nil {
			if err := s.projects.DeleteProjectInternal(ctx, project.ID); err != nil {
				slog.Warn("setup rollback: delete project failed", "project", project.ID, "error", err)
			}
		}
		// 直接走 repo 删除：Admins.Delete 会拒绝删除最后一个 owner，
		// 与失败补偿的语义冲突。
		if err := s.adminRepo.DeleteAdmin(ctx, admin.ID); err != nil {
			slog.Warn("setup rollback: delete admin failed", "admin", admin.ID, "error", err)
		}
	}

	project, err = s.projects.CreateProjectInternal(ctx, server.CreateProjectCommand{
		ID:              projectID,
		Name:            projectID,
		Description:     "Bootstrap project",
		FirstDatabaseID: databaseID,
	})
	if err != nil {
		rollback()
		return nil, status.Errorf(codes.Internal, "create project: %v", err)
	}

	// 保持 admin_projects 关联数据完整（owner 实际会被
	// ValidateAdminProjectAccess 放行，但表应反映真实授权关系）。
	if err := s.adminProjectRepo.GrantProjectAccess(ctx, admin.ID, project.ID); err != nil {
		rollback()
		return nil, status.Errorf(codes.Internal, "grant project access: %v", err)
	}

	// 用规范化后的 email（Admins.Create 内部已小写化）签发 TokenPair。
	tokens, err := s.auth.SignIn(ctx, SignInCommand{Email: admin.Email, Password: cmd.Password})
	if err != nil {
		rollback()
		return nil, err
	}

	return &SignUpResult{
		Admin:  admin,
		Tokens: tokens,
	}, nil
}

// bootstrapPrincipalCtx 注入引导路径专用的平台 admin 主体，供 Admins.Create
// 的 actor 守卫放行。SignUp 已完成 setup token 授权。
func bootstrapPrincipalCtx(ctx context.Context) context.Context {
	return contexts.WithPrincipal(ctx, &shared.Principal{
		ActorID:         "setup",
		ActorKind:       shared.ActorKindAdmin,
		IsPlatformAdmin: true,
		AdminID:         "setup",
	})
}

// setupTokenEqual 常量时间比较 setup token，避免时序侧信道。
func setupTokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
