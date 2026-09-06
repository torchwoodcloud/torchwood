package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	"github.com/torchwooddev/torchwood/internal/domain/shared"
	"github.com/torchwooddev/torchwood/internal/pkg/contexts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Validator validates a raw credential and returns the authenticated Principal.
type Validator interface {
	Authenticate(ctx context.Context, req shared.AuthnRequest) (*shared.Principal, error)
	ValidateToken(ctx context.Context, token string) (*shared.Principal, error)
	ValidateCredential(ctx context.Context, raw string, credentialType shared.CredentialType) (*shared.Principal, error)
	ValidateAdminProjectAccess(ctx context.Context, principal *shared.Principal) error
}

// AuthInterceptor 是统一授权执行点（机制重设计 M3）：策略唯一声明在 proto
// （authz 注解），经 runtime.BuildMethodPolicies 收集为 domainauth.PolicySet
// 注入本拦截器；admin 角色/API key scope/permissions 全部从 PolicySet 读取，
// 本包不再持有任何手写策略表。
type AuthInterceptor struct {
	validator Validator
	policies  *domainauth.PolicySet
	logger    *slog.Logger
}

// NewAuthInterceptor 构造拦截器；policies 为策略注册表（nil 拒绝构造）。
func NewAuthInterceptor(validator Validator, policies *domainauth.PolicySet) (*AuthInterceptor, error) {
	if validator == nil {
		return nil, errors.New("validator cannot be nil")
	}
	if policies == nil {
		return nil, errors.New("method policies cannot be nil")
	}
	return &AuthInterceptor{
		validator: validator,
		policies:  policies,
		logger:    slog.Default(),
	}, nil
}

// WithLogger 替换认证失败留痕所用的 logger（默认 slog.Default()），返回自身便于链式。
func (i *AuthInterceptor) WithLogger(l *slog.Logger) *AuthInterceptor {
	if l != nil {
		i.logger = l
	}
	return i
}

// logAuthFailure 在认证/鉴权拒绝路径输出结构化告警日志，只记录方法名、
// 拒绝原因类别与凭证类型，绝不记录 token 本体。
func (i *AuthInterceptor) logAuthFailure(ctx context.Context, method, reason string, credentialType shared.CredentialType) {
	ci := contexts.ClientInfoFrom(ctx)
	i.logger.WarnContext(ctx, "grpc auth rejected",
		slog.String("method", method),
		slog.String("reason", reason),
		slog.String("credential_type", string(credentialType)),
		slog.String("ip", ci.IP),
		slog.String("user_agent", ci.UserAgent),
	)
}

func (i *AuthInterceptor) UnaryAuthMiddleware(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	policy, ok := i.policies.Get(info.FullMethod)
	if !ok {
		// fail-closed：未登记策略的方法一律拒绝（启动期另有
		// assertRegisteredMethodsHaveAuthz 兜底，此处防御直接调用）。
		i.logAuthFailure(ctx, info.FullMethod, "policy_missing", "")
		return nil, status.Error(codes.PermissionDenied, "no auth policy for method")
	}

	if policy.Access == domainauth.AccessPublic {
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			if principal, err := i.validator.Authenticate(ctx, authnRequestFromMD(md)); err == nil && principal != nil {
				ctx = contexts.WithPrincipal(ctx, principal)
			}
		}
		return handler(ctx, req)
	}

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		i.logAuthFailure(ctx, info.FullMethod, "metadata_missing", "")
		return nil, status.Error(codes.Unauthenticated, "metadata is not provided")
	}

	authn := authnRequestFromMD(md)
	principal, err := i.validator.Authenticate(ctx, authn)
	if err != nil {
		ct, _, parseErr := shared.ParseAuthnRequest(authn)
		if parseErr != nil {
			i.logAuthFailure(ctx, info.FullMethod, parseFailureReason(parseErr), "")
			return nil, status.Error(codes.Unauthenticated, parseErr.Error())
		}
		i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", ct)
		return nil, err
	}
	if principal == nil {
		i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", "")
		return nil, status.Error(codes.Unauthenticated, "invalid or expired credential")
	}
	credentialType := principal.CredentialType

	if policy.Access == domainauth.AccessServer {
		// SERVER 面（原 ACCESS_API_KEY）：凭证族 = API key 或 admin 会话。
		if principal.CredentialType != shared.CredentialTypeAPIKey && principal.ActorKind != shared.ActorKindAdmin {
			i.logAuthFailure(ctx, info.FullMethod, "credential_type_not_allowed", credentialType)
			return nil, status.Error(codes.Unauthenticated, "developer API requires x-api-key header or admin session")
		}
		if principal.CredentialType == shared.CredentialTypeAPIKey {
			// 平台专属面不声明 api_key_scope（AssertSemantic 保证），scope
			// 匹配 fail-closed：未声明即拒绝（通配符不豁免）。
			if !i.policies.AllowsAPIKey(info.FullMethod, principal.Permissions) {
				i.logAuthFailure(ctx, info.FullMethod, "apikey_scope_missing", credentialType)
				return nil, status.Error(codes.PermissionDenied, "api key missing required scope")
			}
		}
	}

	// Allow admin console sessions to target a specific project via header.
	if principal.ActorKind == shared.ActorKindAdmin {
		// 角色门（SERVER 面的 admin_roles；nil = 不限角色）。
		if roles := policy.AdminRoles; len(roles) > 0 && !principal.HasAnyRole(adminRoleStrings(roles)) {
			i.logAuthFailure(ctx, info.FullMethod, "admin_role_denied", credentialType)
			return nil, status.Error(codes.PermissionDenied, "missing required admin role")
		}
		// P3-4：X-Torchwood-Project 多值拒绝（fail-closed，一致性缺口修复）。
		if values := md.Get("x-torchwood-project"); len(values) > 1 {
			i.logAuthFailure(ctx, info.FullMethod, "multi_project_header", credentialType)
			return nil, status.Error(codes.InvalidArgument, "multiple X-Torchwood-Project headers not allowed")
		} else if len(values) == 1 {
			if projectID := strings.TrimSpace(values[0]); projectID != "" {
				principal.ProjectID = projectID
			}
		}
		if err := i.validator.ValidateAdminProjectAccess(ctx, principal); err != nil {
			i.logAuthFailure(ctx, info.FullMethod, "admin_project_access_denied", credentialType)
			return nil, err
		}
	}

	if perms := policy.Permissions; len(perms) > 0 {
		// PERMISSION/END_USER 面角色门。API key 只允许经 SERVER 面的 scope
		// 门禁调用；console/owner 类权限是 admin 会话专属，scope * / all
		// 也不得放行（安全评审 M7）。
		if principal.CredentialType == shared.CredentialTypeAPIKey {
			i.logAuthFailure(ctx, info.FullMethod, "apikey_permission_method_denied", credentialType)
			return nil, status.Error(codes.PermissionDenied, "api key credentials not allowed on permission-gated methods")
		}
		if !principal.HasAnyRole(perms) {
			i.logAuthFailure(ctx, info.FullMethod, "permission_denied", credentialType)
			return nil, status.Error(codes.PermissionDenied, "missing required permission")
		}
	}

	ctx = contexts.WithPrincipal(ctx, principal)
	return handler(ctx, req)
}

// adminRoleStrings 将 enum 角色转为主体角色串（AdminRole 的字符串形态即
// principal.Roles 中的角色串，两者由 policy.go 词表锁定一致）。
func adminRoleStrings(roles []domainauth.AdminRole) []string {
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		out = append(out, string(r))
	}
	return out
}

func parseFailureReason(err error) string {
	switch {
	case errors.Is(err, shared.ErrMultipleCredentials):
		return "multiple_credentials"
	case errors.Is(err, shared.ErrInvalidAuthorization):
		return "invalid_authorization"
	default:
		return "credential_missing"
	}
}

func authnRequestFromMD(md metadata.MD) shared.AuthnRequest {
	return shared.AuthnRequest{
		Authorization: md.Get("authorization"),
		APIKey:        md.Get("x-api-key"),
		CookieHeaders: md.Get("cookie"),
	}
}

// ParseAuthorizationHeader 解析 Authorization 头；实现位于 shared.ParseAuthnRequest。
func ParseAuthorizationHeader(raw string) (shared.CredentialType, string, bool) {
	return shared.ParseAuthorizationHeader(raw)
}

func firstMetadataValue(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
