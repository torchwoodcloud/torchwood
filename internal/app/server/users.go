package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/password"
	"github.com/torchwoodcloud/torchwood/pkg/uow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Users struct {
	projectRepo projects.Repository
	sessions    domainauth.SessionService
	sessionRepo domainauth.SessionRepository
	db          uow.Runner
	usersRepo   users.Repository
	groupsRepo  groups.GroupRepository
	memberships groups.MembershipRepository
}

func NewUsers(
	projectRepo projects.Repository,
	sessions domainauth.SessionService,
	db uow.Runner,
	usersRepo users.Repository,
	sessionRepo domainauth.SessionRepository,
	groupsRepo groups.GroupRepository,
	memberships groups.MembershipRepository,
) *Users {
	return &Users{
		projectRepo: projectRepo,
		sessions:    sessions,
		sessionRepo: sessionRepo,
		db:          db,
		usersRepo:   usersRepo,
		groupsRepo:  groupsRepo,
		memberships: memberships,
	}
}

func (u *Users) resolveProject(ctx context.Context, projectID string) (*projects.Project, error) {
	p, err := u.projectRepo.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, status.Error(codes.NotFound, "project not found")
	}
	return p, nil
}

func (u *Users) ListUsers(ctx context.Context, projectID string, q databases.Query, _ databases.Principal) ([]databases.Document, int64, string, error) {
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, 0, "", err
	}
	list, err := u.usersRepo.List(ctx, projectID, users.ListFilter{
		Queries:   q.Queries,
		PageSize:  q.PageSize,
		PageToken: q.PageToken,
	})
	if err != nil {
		return nil, 0, "", err
	}
	docs := make([]databases.Document, 0, len(list.Users))
	for _, usr := range list.Users {
		if d := userAsDocument(usr); d != nil {
			docs = append(docs, *d)
		}
	}
	return docs, list.TotalCount, list.NextPageToken, nil
}

func (u *Users) GetUser(ctx context.Context, projectID, userID string, _ databases.Principal) (*databases.Document, error) {
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	found, err := u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, nil
	}
	return userAsDocument(found), nil
}

var userUpdateProtectedFields = map[string]struct{}{
	"password_hash": {},
}

// CreateUser 服务端创建用户：校验 email 唯一性与密码强度，写入静态 users 表。
// 与 Client SignUp 共用同一套权限与存储语义。
// 纵深防御（G2-2）：业务写主体（console admin 会话 / API key）才允许经
// SystemPrincipal 写库；viewer 角色细粒度由拦截器 adminRoleMethodRules 把关。
func (u *Users) CreateUser(ctx context.Context, projectID string, cmd CreateUserCommand) (*databases.Document, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	email := normalizeEmail(cmd.Email)
	if email == "" {
		return nil, status.Error(codes.InvalidArgument, "email is required")
	}
	if err := users.ValidatePasswordStrength(cmd.Password); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if cmd.Status != "" {
		if err := users.ValidateStatus(cmd.Status); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	} else {
		cmd.Status = users.StatusActive
	}

	existing, err := u.usersRepo.GetByEmail(ctx, projectID, email)
	if err != nil {
		return nil, err
	}
	if err := users.RequireUniqueEmail(existing); err != nil {
		return nil, appshared.MapUserError(err)
	}

	registered, err := users.Register(users.RegisterInput{
		ID:       idgen.UUID().String(),
		Email:    email,
		Password: cmd.Password,
		Name:     cmd.Name,
		Status:   cmd.Status,
		Labels:   users.LabelsFromAny(cmd.Labels),
		Prefs:    cmd.Prefs,
	})
	if err != nil {
		return nil, appshared.MapUserError(err)
	}
	if err := u.usersRepo.Insert(ctx, projectID, registered); err != nil {
		if mapped := appshared.MapUserError(err); mapped != err {
			return nil, mapped
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	return userAsDocument(registered), nil
}

func (u *Users) UpdateUser(ctx context.Context, projectID, userID string, updates map[string]any, principal databases.Principal) (*databases.Document, error) {
	// 纵深防御（Round3 H1-3）：UpdateUser 可改 email/status（接管面），
	// 必须由 Server 写主体（console admin 会话 / API key）调用；端用户/匿名
	// 即使绕过拦截器也不得以 SystemPrincipal 改他人资料。owner/admin 角色
	// 细粒度由拦截器 adminRoleMethodRules 把关。
	// M5 C3（评审补偿控制）：email/status 实际变更时撤销该用户全部端会话。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	if raw, ok := updates["status"]; ok {
		s, ok := raw.(string)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "status must be a string")
		}
		if err := users.ValidateStatus(s); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
	if raw, ok := updates["email"]; ok {
		email, ok := raw.(string)
		if !ok {
			return nil, status.Error(codes.InvalidArgument, "email must be a string")
		}
		email = normalizeEmail(email)
		if email == "" {
			return nil, status.Error(codes.InvalidArgument, "email must not be empty")
		}
		// 改邮箱查重（排除自身 userID）：users_email_unique 唯一索引兜底并发冲突。
		taken, err := u.usersRepo.GetByEmail(ctx, projectID, email)
		if err != nil {
			return nil, err
		}
		if taken != nil && taken.ID != userID {
			return nil, status.Error(codes.AlreadyExists, "email already registered")
		}
		updates["email"] = email
		// 与 UpdateAccount 一致：改邮箱必须重新验证。
		updates["email_verified"] = false
	}
	filtered := make(map[string]any, len(updates))
	for k, v := range updates {
		if strings.HasPrefix(k, "_") {
			continue
		}
		if _, blocked := userUpdateProtectedFields[k]; blocked {
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no updatable fields supplied (password_hash is managed via the dedicated password endpoint)")
	}
	current, err := u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	// M5 C3（评审补偿控制）：email/status 是接管面字段，实际发生变更即撤销
	// 该用户全部端会话——对齐端用户改密语义（DeleteSessionsByUser 先撤后
	// 提交，G3 同源），堵住"被接管的旧会话在 email/status 变更后继续存活"
	// 窗口；写入当前同值视为幂等无操作。email 变更保持 email_verified=false。
	takeoverChanged := false
	if email, ok := filtered["email"].(string); ok && email != current.Email {
		takeoverChanged = true
	}
	if statusVal, ok := filtered["status"].(string); ok && statusVal != current.Status {
		takeoverChanged = true
	}
	if takeoverChanged {
		if err := u.sessions.DeleteSessionsByUser(ctx, projectID, userID); err != nil {
			return nil, fmt.Errorf("revoke sessions after takeover-field change: %w", err)
		}
	}
	if err := u.usersRepo.Update(ctx, projectID, userID, filtered); err != nil {
		if mapped := appshared.MapUserError(err); mapped != err {
			return nil, mapped
		}
		return nil, fmt.Errorf("update user: %w", appshared.MapDocumentDBError(err))
	}
	found, err := u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	return userAsDocument(found), nil
}

// UpdateUserPassword 服务端直接重置密码，并撤销该用户全部会话（与客户端
// 改密后清会话语义一致），避免旧令牌继续有效。
// 决策 v8：users 六写方法归一业务写档（member+users.write）——接管的信任
// 边界收敛到 scope/角色授予环节；角色/scope 细粒度由拦截器策略表把关。
func (u *Users) UpdateUserPassword(ctx context.Context, projectID, userID, newPassword string) (*databases.Document, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	if err := users.ValidatePasswordStrength(newPassword); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	found, err := u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	hash, err := password.Hash(newPassword)
	if err != nil {
		return nil, err
	}
	if err := u.usersRepo.Update(ctx, projectID, userID, map[string]any{"password_hash": hash}); err != nil {
		return nil, fmt.Errorf("update user password: %w", err)
	}
	if err := u.sessions.DeleteSessionsByUser(ctx, projectID, userID); err != nil {
		return nil, fmt.Errorf("revoke sessions after password reset: %w", err)
	}
	found, err = u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	return userAsDocument(found), nil
}

// ListUserSessions 列出指定用户的全部会话。
func (u *Users) ListUserSessions(ctx context.Context, projectID, userID string) ([]databases.Document, error) {
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	found, err := u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	list, err := u.sessionRepo.ListByUser(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	docs := make([]databases.Document, 0, len(list))
	for i := range list {
		docs = append(docs, sessionAsDocument(&list[i]))
	}
	return docs, nil
}

func (u *Users) DeleteUserSession(ctx context.Context, projectID, userID, sessionID string) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return err
	}
	sess, err := u.sessionRepo.GetByID(ctx, projectID, sessionID)
	if err != nil {
		return err
	}
	if sess == nil || sess.UserID != userID {
		return status.Error(codes.NotFound, "session not found")
	}
	return u.sessionRepo.Delete(ctx, projectID, sessionID)
}

// CreateUserToken 模拟登录：以指定用户身份创建会话并签发 token。用途：
// 服务端登录桥接（自有 auth 体系验证后为用户铸造 Torchwood 会话，见
// docs/developer/12-sdk.md §4.4 的 agent 典型流程）与客服/调试模拟登录。
// 决策 v8：归一业务写档（member+users.write）。
// M5 C2（评审 B-1 补偿控制）：签发走短时会话通路——会话 15min、access/
// refresh TTL 被实现内 1h 硬上限常量封顶，不继承 7 天默认会话 TTL，任何
// security.jwt.* 配置都无法突破。
func (u *Users) CreateUserToken(ctx context.Context, projectID, userID string) (*domainauth.TokenBundle, error) {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return nil, err
	}
	found, err := u.usersRepo.GetByID(ctx, projectID, userID)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if !found.CanAuthenticate() {
		return nil, status.Error(codes.FailedPrecondition, "user account is not active")
	}
	bundle, _, err := u.sessions.CreateShortLivedSessionAndTokens(ctx, projectID, userID, found.Email, "server_token")
	if err != nil {
		return nil, err
	}
	// 审计标记：记录谁为哪个用户模拟签发了 token（session provider 为 server_token）。
	if p, ok := contexts.Principal(ctx); ok {
		slog.Info("create user token",
			"project", projectID,
			"user", userID,
			"caller_actor", p.ActorID,
			"caller_kind", p.ActorKind,
			"caller_credential", p.CredentialType,
		)
	}
	return bundle, nil
}

// 决策 v8：users 六写方法归一业务写档（member+users.write）。
func (u *Users) DeleteUser(ctx context.Context, projectID, userID string, _ databases.Principal) error {
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return err
	}
	if _, err := u.resolveProject(ctx, projectID); err != nil {
		return err
	}
	return u.db.Run(ctx, func(txCtx context.Context) error {
		var groupIDs []string
		if u.memberships != nil {
			mems, err := u.memberships.ListByUser(txCtx, projectID, userID)
			if err != nil {
				return err
			}
			seen := map[string]struct{}{}
			for _, m := range mems {
				if m.Status == groups.StatusAccepted && m.GroupID != "" {
					if _, ok := seen[m.GroupID]; ok {
						continue
					}
					seen[m.GroupID] = struct{}{}
					groupIDs = append(groupIDs, m.GroupID)
				}
			}
		}
		if err := u.usersRepo.Delete(txCtx, projectID, userID); err != nil {
			return err
		}
		if u.groupsRepo == nil {
			return nil
		}
		for _, groupID := range groupIDs {
			if err := u.groupsRepo.RecountAccepted(txCtx, projectID, groupID); err != nil {
				return err
			}
		}
		return nil
	})
}

// CreateUserCommand 是 CreateUser 的输入。
type CreateUserCommand struct {
	Email    string
	Password string
	Name     string
	Status   string
	Labels   []any
	Prefs    map[string]any
}

func normalizeEmail(email string) string {
	return users.NormalizeEmail(email)
}

func userAsDocument(u *users.User) *databases.Document {
	if u == nil {
		return nil
	}
	doc := &databases.Document{ID: u.ID, CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, Data: u.DocumentData()}
	return doc
}

func sessionAsDocument(s *domainauth.Session) databases.Document {
	if s == nil {
		return databases.Document{}
	}
	return databases.Document{
		ID:        s.ID,
		CreatedAt: s.CreatedAt,
		UpdatedAt: s.UpdatedAt,
		Data: map[string]any{
			"user_id":    s.UserID,
			"provider":   s.Provider,
			"user_agent": s.UserAgent,
			"ip":         s.IP,
			"country":    s.Country,
			"expire_at":  s.ExpireAt,
		},
	}
}
