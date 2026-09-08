package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/torchwooddev/torchwood/internal/domain/audit"
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
// DenyAuditor 是认证拒绝审计的最小写入口（audit.Repository 的窄投影，
// 复用 Entry.Metadata 承载 denied/reason，M5 C6）。
type DenyAuditor interface {
	Insert(ctx context.Context, entry *audit.Entry) error
}

type AuthInterceptor struct {
	validator Validator
	policies  *domainauth.PolicySet
	logger    *slog.Logger
	// denyAudit 是可选的拒绝审计 sink（WithDenyAuditSink 注入，nil 不写）。
	denyAudit DenyAuditor
	// apiKeyFailThrottle 是 X-API-Key 认证失败的按 IP 频控（T-02，可选）：
	// 认证失败时向 limiter 计一次失败，超限即 429；未注入不启用。
	apiKeyFailLimiter domainauth.RateLimiter
	apiKeyFailLimit   int
	apiKeyFailWindow  time.Duration
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

// WithDenyAuditSink 注入认证拒绝审计 sink（M5 C6）：best-effort，写失败仅
// 告警不影响拒绝响应；nil（未装配）时保持纯日志行为，既有测试不受影响。
func (i *AuthInterceptor) WithDenyAuditSink(repo DenyAuditor) *AuthInterceptor {
	if repo != nil {
		i.denyAudit = repo
	}
	return i
}

// API key 认证失败频控默认值（T-02）：每 IP 10 次失败/60s 窗口。
const (
	defaultAPIKeyFailLimit  = 10
	defaultAPIKeyFailWindow = time.Minute
)

// WithAPIKeyFailThrottle 注入 X-API-Key 认证失败的按 IP 频控（T-02）：
// limiter 复用 domainauth.RateLimiter 端口；limit<=0 或 window<=0 时回落
// 内置默认；limiter 为 nil 不启用。
func (i *AuthInterceptor) WithAPIKeyFailThrottle(limiter domainauth.RateLimiter, limit int, window time.Duration) *AuthInterceptor {
	if limiter == nil {
		return i
	}
	if limit <= 0 {
		limit = defaultAPIKeyFailLimit
	}
	if window <= 0 {
		window = defaultAPIKeyFailWindow
	}
	i.apiKeyFailLimiter = limiter
	i.apiKeyFailLimit = limit
	i.apiKeyFailWindow = window
	return i
}

// recordAPIKeyAuthFailure 在 API key 认证失败路径计数：超限返回 429
// （带 RetryInfo detail），否则返回 nil 继续原有 401 语义。limiter 基础
// 设施故障只告警不拦截（fail-open：限速器故障不放大认证面故障）。
func (i *AuthInterceptor) recordAPIKeyAuthFailure(ctx context.Context, ip string) error {
	if i.apiKeyFailLimiter == nil || ip == "" {
		return nil
	}
	err := i.apiKeyFailLimiter.Allow(ctx, "apikeyauth:ip:"+ip, i.apiKeyFailLimit, i.apiKeyFailWindow)
	if err == nil {
		return nil
	}
	if status.Code(err) == codes.ResourceExhausted {
		i.logger.WarnContext(ctx, "api key auth throttle tripped",
			slog.String("ip", ip), slog.Int("limit", i.apiKeyFailLimit))
		return withRetryInfoFallback(err, i.apiKeyFailWindow)
	}
	i.logger.WarnContext(ctx, "api key auth throttle limiter error (fail-open)",
		slog.String("error", err.Error()))
	return nil
}

// WithLogger 替换认证失败留痕所用的 logger（默认 slog.Default()），返回自身便于链式。
func (i *AuthInterceptor) WithLogger(l *slog.Logger) *AuthInterceptor {
	if l != nil {
		i.logger = l
	}
	return i
}

// logAuthFailure 在认证/鉴权拒绝路径输出结构化告警日志，只记录方法名、
// 拒绝原因类别与凭证类型，绝不记录 token 本体。M5 C6：并联 best-effort
// 写一条拒绝审计行（Entry.Status="denied"，Metadata["denied"]=true、
// Metadata["reason"]=拒绝原因），Actor/Project 从已解析 principal 取
// （认证前拒绝为空）；复用 auditFromHTTP 的 3s + WithoutCancel 模式，
// 拒绝响应不被审计写阻塞或连带失败。
func (i *AuthInterceptor) logAuthFailure(ctx context.Context, method, reason string, credentialType shared.CredentialType, principal *shared.Principal) {
	ci := contexts.ClientInfoFrom(ctx)
	i.logger.WarnContext(ctx, "grpc auth rejected",
		slog.String("method", method),
		slog.String("reason", reason),
		slog.String("credential_type", string(credentialType)),
		slog.String("ip", ci.IP),
		slog.String("user_agent", ci.UserAgent),
	)
	i.writeDenyAudit(ctx, method, reason, credentialType, principal, ci)
}

// writeDenyAudit best-effort 落拒绝审计行；sink 未装配时不做任何事。
func (i *AuthInterceptor) writeDenyAudit(ctx context.Context, method, reason string, credentialType shared.CredentialType, principal *shared.Principal, ci contexts.ClientInfo) {
	if i.denyAudit == nil {
		return
	}
	entry := &audit.Entry{
		Action:    method,
		Status:    "denied",
		IP:        ci.IP,
		UserAgent: ci.UserAgent,
		CreatedAt: time.Now().UTC(),
		Metadata: map[string]any{
			"denied":          true,
			"reason":          reason,
			"credential_type": string(credentialType),
		},
	}
	if principal != nil {
		entry.ActorID = string(principal.ActorID)
		entry.ActorKind = string(principal.ActorKind)
		entry.ProjectID = principal.ProjectID
	}
	insertCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := i.denyAudit.Insert(insertCtx, entry); err != nil {
		i.logger.Warn("auth deny audit insert failed",
			slog.String("method", method), slog.String("error", err.Error()))
	}
}

func (i *AuthInterceptor) UnaryAuthMiddleware(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	policy, ok := i.policies.Get(info.FullMethod)
	if !ok {
		// fail-closed：未登记策略的方法一律拒绝（启动期另有
		// assertRegisteredMethodsHaveAuthz 兜底，此处防御直接调用）。
		i.logAuthFailure(ctx, info.FullMethod, "policy_missing", "", nil)
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
		i.logAuthFailure(ctx, info.FullMethod, "metadata_missing", "", nil)
		return nil, status.Error(codes.Unauthenticated, "metadata is not provided")
	}

	authn := authnRequestFromMD(md)
	principal, err := i.validator.Authenticate(ctx, authn)
	if err != nil {
		ct, _, parseErr := shared.ParseAuthnRequest(authn)
		if parseErr != nil {
			i.logAuthFailure(ctx, info.FullMethod, parseFailureReason(parseErr), "", nil)
			return nil, status.Error(codes.Unauthenticated, parseErr.Error())
		}
		// T-02：API key 认证失败按来源 IP 计数，超限 429（在 401 之后判定
		// 顺序：只对"失败"计数，不惩罚携带有效 key 的请求）。
		if ct == shared.CredentialTypeAPIKey {
			if ci := contexts.ClientInfoFrom(ctx); ci.IP != "" {
				if throttleErr := i.recordAPIKeyAuthFailure(ctx, ci.IP); throttleErr != nil {
					i.logAuthFailure(ctx, info.FullMethod, "apikey_auth_throttled", ct, nil)
					return nil, throttleErr
				}
			}
		}
		i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", ct, nil)
		return nil, err
	}
	if principal == nil {
		i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", "", nil)
		return nil, status.Error(codes.Unauthenticated, "invalid or expired credential")
	}
	credentialType := principal.CredentialType

	if policy.Access == domainauth.AccessServer {
		// SERVER 面（原 ACCESS_API_KEY）：凭证族 = API key 或 admin 会话。
		if principal.CredentialType != shared.CredentialTypeAPIKey && principal.ActorKind != shared.ActorKindAdmin {
			i.logAuthFailure(ctx, info.FullMethod, "credential_type_not_allowed", credentialType, principal)
			return nil, status.Error(codes.Unauthenticated, "developer API requires x-api-key header or admin session")
		}
		if principal.CredentialType == shared.CredentialTypeAPIKey {
			// 平台专属面不声明 api_key_scope（AssertSemantic 保证），scope
			// 匹配 fail-closed：未声明即拒绝（通配符不豁免）。
			rule := i.policies.HasAPIKeyScope(info.FullMethod)
			if rule == nil {
				i.logAuthFailure(ctx, info.FullMethod, "apikey_scope_missing", credentialType, principal)
				return nil, status.Error(codes.PermissionDenied, "api key missing required scope")
			}
			// 资源级 scope（T-02）：按方法声明的资源族从请求体提取目标实例
			//（database_id/bucket_id；CreateDatabase/GetBucket 等以 id 寻址），
			// `databases:blog` 只放行寻址 blog 的请求，全集型方法（List 等
			// 无目标）对实例限定 scope 一律 403。
			if !i.policies.AllowsAPIKeyTargets(info.FullMethod, principal.Permissions, apiKeyScopeTargets(rule, req)) {
				i.logAuthFailure(ctx, info.FullMethod, "apikey_scope_missing", credentialType, principal)
				return nil, status.Error(codes.PermissionDenied, "api key missing required scope")
			}
		}
	}

	// Allow admin console sessions to target a specific project via header.
	if principal.ActorKind == shared.ActorKindAdmin {
		// 角色门（SERVER 面的 admin_roles；nil = 不限角色）。
		if roles := policy.AdminRoles; len(roles) > 0 && !principal.HasAnyRole(adminRoleStrings(roles)) {
			i.logAuthFailure(ctx, info.FullMethod, "admin_role_denied", credentialType, principal)
			return nil, status.Error(codes.PermissionDenied, "missing required admin role")
		}
		// P3-4：X-Torchwood-Project 多值拒绝（fail-closed，一致性缺口修复）。
		if values := md.Get("x-torchwood-project"); len(values) > 1 {
			i.logAuthFailure(ctx, info.FullMethod, "multi_project_header", credentialType, principal)
			return nil, status.Error(codes.InvalidArgument, "multiple X-Torchwood-Project headers not allowed")
		} else if len(values) == 1 {
			if projectID := strings.TrimSpace(values[0]); projectID != "" {
				principal.ProjectID = projectID
			}
		}
		if err := i.validator.ValidateAdminProjectAccess(ctx, principal); err != nil {
			i.logAuthFailure(ctx, info.FullMethod, "admin_project_access_denied", credentialType, principal)
			return nil, err
		}
	}

	if perms := policy.Permissions; len(perms) > 0 {
		// PERMISSION/END_USER 面角色门。API key 只允许经 SERVER 面的 scope
		// 门禁调用；console/owner 类权限是 admin 会话专属，scope * / all
		// 也不得放行（安全评审 M7）。
		if principal.CredentialType == shared.CredentialTypeAPIKey {
			i.logAuthFailure(ctx, info.FullMethod, "apikey_permission_method_denied", credentialType, principal)
			return nil, status.Error(codes.PermissionDenied, "api key credentials not allowed on permission-gated methods")
		}
		if !principal.HasAnyRole(perms) {
			i.logAuthFailure(ctx, info.FullMethod, "permission_denied", credentialType, principal)
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

// scopeTargetGetter 是 genproto 请求消息的实例寻址 getter（生成代码恒有）。
type scopeTargetGetter interface{ GetId() string }

// apiKeyScopeTargets 按方法 scope 声明的资源族从请求消息提取目标实例：
//   - databases：优先 GetDatabaseId()（绝大多数方法），CreateDatabase 以
//     GetId() 寻址被创建的库；
//   - storage：优先 GetBucketId()（文件族），GetBucket/UpdateBucket 等
//     以 GetId() 寻址；
//   - 其余资源族不做实例寻址（零值 targets，资源限定 scope 恒不匹配）。
func apiKeyScopeTargets(rule *domainauth.ScopeRule, req any) domainauth.ScopeTargets {
	var targets domainauth.ScopeTargets
	if rule == nil || req == nil {
		return targets
	}
	switch rule.Resource {
	case domainauth.ScopeDatabases:
		if g, ok := req.(interface{ GetDatabaseId() string }); ok {
			targets.DatabaseID = g.GetDatabaseId()
			return targets
		}
		if g, ok := req.(scopeTargetGetter); ok {
			targets.DatabaseID = g.GetId()
		}
	case domainauth.ScopeStorage:
		if g, ok := req.(interface{ GetBucketId() string }); ok {
			targets.BucketID = g.GetBucketId()
			return targets
		}
		if g, ok := req.(scopeTargetGetter); ok {
			targets.BucketID = g.GetId()
		}
	}
	return targets
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
