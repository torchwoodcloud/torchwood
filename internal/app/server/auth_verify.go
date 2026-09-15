package server

import (
	"context"
	"strings"
	"time"

	appshared "github.com/torchwoodcloud/torchwood/internal/app/shared"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/groups"
	"github.com/torchwoodcloud/torchwood/internal/domain/projects"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/domain/users"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
	"github.com/torchwoodcloud/torchwood/pkg/idgen"
	"github.com/torchwoodcloud/torchwood/pkg/jwtparser"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Auth 是对外凭证校验用例（token introspection，RFC 9660 模式）：把进程内
// 凭证校验能力以 Server API 暴露。判定语义与请求侧认证完全一致——直接
// 复用 domainauth.CredentialVerifier（infra 校验器），本用例只负责分派、
// 非破坏性前置过滤（拒绝 admin/refresh/一次性 JWT 且绝不消费一次性 token）
// 与结果投影（roles/groups/labels/expires_at 的结构化补充）。
type Auth struct {
	verifier    domainauth.CredentialVerifier
	apiKeyRepo  projects.APIKeyRepository
	usersRepo   users.Repository
	memberships groups.MembershipRepository
}

func NewAuth(
	verifier domainauth.CredentialVerifier,
	apiKeyRepo projects.APIKeyRepository,
	usersRepo users.Repository,
	memberships groups.MembershipRepository,
) *Auth {
	return &Auth{
		verifier:    verifier,
		apiKeyRepo:  apiKeyRepo,
		usersRepo:   usersRepo,
		memberships: memberships,
	}
}

// CredentialDispatch 是 VerifyToken 的凭证分派指令。
type CredentialDispatch int

const (
	// CredentialAuto 按结构分派：sk- 前缀走 API key，其余按端用户 JWT。
	CredentialAuto CredentialDispatch = iota
	CredentialJWT
	CredentialAPIKey
)

// VerifyTokenCommand 是 VerifyToken 的输入。
type VerifyTokenCommand struct {
	Token string
	Type  CredentialDispatch
}

// VerifiedGroupRef 是已接受成员关系的结构化投影。
type VerifiedGroupRef struct {
	ID   string
	Role string
}

// VerifiedCredential 是 introspection 判定结果；Valid=false 时其余字段为零值
// （不泄露主体信息）。
type VerifiedCredential struct {
	Valid     bool
	UserID    string
	Username  string
	ProjectID string
	SessionID string
	// ExpiresAt 零值 = 无到期信息（JWT exp / API key expire_at）。
	ExpiresAt time.Time
	Roles     []string
	Groups    []VerifiedGroupRef
	Labels    []string
	ActorKind shared.ActorKind
	APIKeyID  string
}

// VerifyToken 校验 Torchwood 凭证原文。任何校验失败（过期/撤销/封禁/不
// 存在/签名无效/跨项目）→ Valid=false 且不报错（introspection 契约）；
// 仅基础设施故障（Internal）如实上报。admin JWT / refresh token / 一次性
// JWT / 函数执行 token 与端用户凭证语义不符，一律 Valid=false。
func (a *Auth) VerifyToken(ctx context.Context, cmd VerifyTokenCommand) (*VerifiedCredential, error) {
	// 纵深防御：拦截器已按 method_auth 把关（SERVER 面：admin 会话或持
	// users.read 的 API key）；此处拒绝匿名/端用户绕过拦截器直接调用。
	if err := appshared.RequireServerPrincipal(ctx); err != nil {
		return nil, err
	}
	switch cmd.Type {
	case CredentialAuto:
		if strings.HasPrefix(cmd.Token, idgen.APIKeySecretPrefix) {
			return a.verifyAPIKey(ctx, cmd.Token)
		}
		return a.verifyJWT(ctx, cmd.Token)
	case CredentialJWT:
		return a.verifyJWT(ctx, cmd.Token)
	case CredentialAPIKey:
		return a.verifyAPIKey(ctx, cmd.Token)
	default:
		return nil, status.Error(codes.InvalidArgument, "unknown credential type")
	}
}

// verifyJWT 校验端用户 access JWT。前置过滤在 ValidateCredential 之前用
// ParseClaims（验签但不产生副作用）完成——尤其一次性 JWT：直接走
// ValidateCredential 会被原子消费，introspection 绝不能让校验动作改变
// 凭证状态。
func (a *Auth) verifyJWT(ctx context.Context, raw string) (*VerifiedCredential, error) {
	invalid := &VerifiedCredential{}
	claims, ok := a.verifier.ParseClaims(raw)
	if !ok {
		return invalid, nil
	}
	// 非破坏性拒绝：一次性 JWT（读判定不消费）、refresh token、非端用户
	// JWT（admin 等）。ActorKind 为空兼容存量旧 token（validator 同语义：
	// 非 "admin" 一律走端用户分支）。
	if claims.OneTime {
		return invalid, nil
	}
	if claims.TokenType != "" && claims.TokenType != jwtparser.TokenTypeAccess {
		return invalid, nil
	}
	if claims.ActorKind != "" && claims.ActorKind != string(shared.ActorKindEndUser) {
		return invalid, nil
	}
	p, err := a.verifier.ValidateCredential(ctx, raw, shared.CredentialTypeToken)
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			// 过期/会话已删/用户封禁/不存在等校验失败 → valid=false + 200。
			return invalid, nil
		}
		return nil, err
	}
	if p.ActorKind != shared.ActorKindEndUser {
		return invalid, nil
	}
	if a.callerProjectMismatch(ctx, p.ProjectID) {
		return invalid, nil
	}
	out := &VerifiedCredential{
		Valid:     true,
		UserID:    p.UserID,
		Username:  p.Email,
		ProjectID: p.ProjectID,
		SessionID: p.SessionID,
		Roles:     p.Roles,
		ActorKind: p.ActorKind,
	}
	if claims.ExpiresAt > 0 {
		out.ExpiresAt = time.Unix(claims.ExpiresAt, 0).UTC()
	}
	// 结构化补充（与角色解析同源的数据面）：labels 取 users 行，groups 取
	// accepted memberships。补充查询失败不改变判定（roles 已实时解析，是
	// 授权事实源；这里只是展示投影）。
	if a.usersRepo != nil && p.ProjectID != "" && p.UserID != "" {
		if u, err := a.usersRepo.GetByID(ctx, p.ProjectID, p.UserID); err == nil && u != nil {
			out.Labels = append([]string{}, u.Labels...)
			if out.Username == "" {
				out.Username = u.Email
			}
		}
	}
	if a.memberships != nil && p.ProjectID != "" && p.UserID != "" {
		if list, err := a.memberships.ListByUser(ctx, p.ProjectID, p.UserID); err == nil {
			out.Groups = verifiedGroups(list)
		}
	}
	return out, nil
}

// verifyAPIKey 校验项目 API key（sha256 查表 + 启用/过期/项目绑定检查，
// 全部由共享校验器完成）；到期时刻从密钥行补读。
func (a *Auth) verifyAPIKey(ctx context.Context, raw string) (*VerifiedCredential, error) {
	invalid := &VerifiedCredential{}
	p, err := a.verifier.ValidateCredential(ctx, raw, shared.CredentialTypeAPIKey)
	if err != nil {
		if status.Code(err) == codes.Unauthenticated {
			return invalid, nil
		}
		return nil, err
	}
	if p.ActorKind != shared.ActorKindService {
		return invalid, nil
	}
	if a.callerProjectMismatch(ctx, p.ProjectID) {
		return invalid, nil
	}
	out := &VerifiedCredential{
		Valid:     true,
		ProjectID: p.ProjectID,
		Roles:     p.Roles,
		ActorKind: p.ActorKind,
		APIKeyID:  p.APIKeyID,
	}
	if a.apiKeyRepo != nil && p.ProjectID != "" && p.APIKeyID != "" {
		if key, err := a.apiKeyRepo.GetAPIKey(ctx, p.ProjectID, p.APIKeyID); err == nil && key != nil && key.ExpireAt != nil {
			out.ExpiresAt = key.ExpireAt.UTC()
		}
	}
	return out, nil
}

// callerProjectMismatch 判定调用者与凭证的项目绑定是否越界：API key 恒绑定
// 项目、admin 会话经 X-Torchwood-Project 携带项目上下文；未携带项目上下文
// （平台 admin）可跨项目 introspection。越界按 valid=false 处理，不泄露他
// 项目主体信息。
func (a *Auth) callerProjectMismatch(ctx context.Context, credentialProjectID string) bool {
	p, ok := contexts.Principal(ctx)
	if !ok || p.ProjectID == "" {
		return false
	}
	return p.ProjectID != credentialProjectID
}

// verifiedGroups 投影 accepted 成员关系：每个 (group, role) 一条；无角色的
// 成员关系产出一条空 role 条目（roles 词汇中的 group:<gid> 对应）。
func verifiedGroups(list []*groups.Membership) []VerifiedGroupRef {
	out := make([]VerifiedGroupRef, 0, len(list))
	for _, m := range list {
		if m == nil || m.Status != groups.StatusAccepted || m.GroupID == "" {
			continue
		}
		if len(m.Roles) == 0 {
			out = append(out, VerifiedGroupRef{ID: m.GroupID})
			continue
		}
		for _, role := range m.Roles {
			if role == "" {
				continue
			}
			out = append(out, VerifiedGroupRef{ID: m.GroupID, Role: role})
		}
	}
	return out
}
