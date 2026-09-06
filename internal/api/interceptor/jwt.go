package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"strings"

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

type AuthInterceptor struct {
	validator         Validator
	publicMethods     map[string]struct{}
	apiKeyMethods     map[string]struct{}
	permissionMethods map[string][]string
	logger            *slog.Logger
}

func NewAuthInterceptor(validator Validator, publicMethods, apiKeyMethods []string, permissionMethods map[string][]string) (*AuthInterceptor, error) {
	if validator == nil {
		return nil, errors.New("validator cannot be nil")
	}
	i := &AuthInterceptor{
		validator:         validator,
		publicMethods:     make(map[string]struct{}),
		apiKeyMethods:     make(map[string]struct{}),
		permissionMethods: permissionMethods,
		logger:            slog.Default(),
	}
	if i.permissionMethods == nil {
		i.permissionMethods = map[string][]string{}
	}
	for _, m := range publicMethods {
		i.publicMethods[m] = struct{}{}
	}
	for _, m := range apiKeyMethods {
		i.apiKeyMethods[m] = struct{}{}
	}
	return i, nil
}

// WithLogger 替换认证失败留痕所用的 logger（默认 slog.Default()），返回自身便于链式调用。
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
	if _, ok := i.publicMethods[info.FullMethod]; ok {
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

	if _, isAPIKeyMethod := i.apiKeyMethods[info.FullMethod]; isAPIKeyMethod {
		if principal.CredentialType != shared.CredentialTypeAPIKey && principal.ActorKind != shared.ActorKindAdmin {
			i.logAuthFailure(ctx, info.FullMethod, "credential_type_not_allowed", credentialType)
			return nil, status.Error(codes.Unauthenticated, "developer API requires x-api-key header or admin session")
		}
		if principal.CredentialType == shared.CredentialTypeAPIKey {
			// API key 凭证禁止调用 APIKeys 服务，防止泄露的 key 自铸新 key 造成永久提权。
			if IsAPIKeysServiceMethod(info.FullMethod) {
				i.logAuthFailure(ctx, info.FullMethod, "apikey_self_management_denied", credentialType)
				return nil, status.Error(codes.PermissionDenied, "api keys cannot manage api keys")
			}
			if !APIKeyScopeAllowed(info.FullMethod, principal.Permissions) {
				i.logAuthFailure(ctx, info.FullMethod, "apikey_scope_missing", credentialType)
				return nil, status.Error(codes.PermissionDenied, "api key missing required scope")
			}
		}
	}

	// Allow admin console sessions to target a specific project via header.
	if principal.ActorKind == shared.ActorKindAdmin {
		// 受限 admin（viewer/member）不得调用仅 owner/admin 的 Server API 写方法。
		if perms := adminRoleMethodRules[info.FullMethod]; len(perms) > 0 && !principal.HasAnyRole(perms) {
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

	if perms := i.permissionMethods[info.FullMethod]; len(perms) > 0 {
		// API Key 只允许经 apiKeyMethods 的 scope 门禁调用；console/owner 类
		// 权限是 admin 会话专属，scope * / all 也不得放行（安全评审 M7）。
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
