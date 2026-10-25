package interceptor

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/lynx-go/grpcapi/authz"
	"github.com/lynx-go/grpcapi/policy"
	"github.com/lynx-go/grpcapi/principal"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/shared"
	"github.com/torchwoodcloud/torchwood/internal/pkg/contexts"
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

// AuthInterceptor 是统一认证拦截器（机制重设计 M3 → grpcapi 执行侧迁移）：
// 本拦截器只做认证（凭证提取/校验/双 Principal 注入）与业务校验
// （X-Torchwood-Project、ValidateAdminProjectAccess）；档位/角色/scope 的
// 授权执行在库 authz 拦截器（authz.NewInterceptor + RequirementDecider，
// 挂本拦截器之后），策略树唯一来自 proto 注解（policy.Build）。
// DenyAuditor 是认证拒绝审计的最小写入口（audit.Repository 的窄投影，
// 复用 Entry.Metadata 承载 denied/reason，M5 C6）。
type DenyAuditor interface {
	Insert(ctx context.Context, entry *audit.Entry) error
}

type AuthInterceptor struct {
	validator Validator
	// policies 只消费 IsAnonymous（PUBLIC 面可选认证语义）；档位/角色/scope
	// 执行不在此处——库 authz 拦截器按同一策略集求值。
	policies *policy.Set
	logger   *slog.Logger
	// denyAudit 是可选的拒绝审计 sink（WithDenyAuditSink 注入，nil 不写）。
	denyAudit DenyAuditor
	// apiKeyFailThrottle 是 X-API-Key 认证失败的按 IP 频控（T-02，可选）：
	// 认证失败时向 limiter 计一次失败，超限即 429；未注入不启用。
	apiKeyFailLimiter domainauth.RateLimiter
	apiKeyFailLimit   int
	apiKeyFailWindow  time.Duration
}

// NewAuthInterceptor 构造认证拦截器；policies 为策略表（nil 拒绝构造），
// 仅用于 PUBLIC 面判定——授权执行由库 authz 拦截器消费同一策略集。
func NewAuthInterceptor(validator Validator, policies *policy.Set) (*AuthInterceptor, error) {
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

// AuthzOnDecision 返回库 authz 拦截器的 OnDecision 回调（装配进
// authz.Options.OnDecision）：拒绝裁决并联写拒绝审计行与结构化日志（M5 C6
// 语义延续——原手工档位判定的 scope/角色/权限拒绝留痕迁移到本回调），
// best-effort 不影响裁决。Reason 取库 Decision.Reason（requirement not
// satisfied / no any_of alternative satisfied / missing principal 等）。
func (i *AuthInterceptor) AuthzOnDecision() func(context.Context, policy.Operation, authz.Decision) {
	return func(ctx context.Context, op policy.Operation, d authz.Decision) {
		if d.Allow {
			return
		}
		p, _ := contexts.Principal(ctx)
		credentialType := shared.CredentialType("")
		if p != nil {
			credentialType = p.CredentialType
		}
		i.logAuthFailure(ctx, op.ID, "authz_denied:"+d.Reason, credentialType, p)
	}
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
	if i.policies.IsAnonymous(info.FullMethod) {
		return i.servePublic(ctx, req, info, handler)
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

	ctx, err = i.finalizePrincipal(ctx, md, info.FullMethod, principal)
	if err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// servePublic 处理 PUBLIC（anonymous 策略）方法：凭证尽力注入、失败不
// 阻断——无条件放行（授权面的 anonymous 树恒放行，无效凭证语义在认证侧
// 收口）。T-02 复扫：显式携带的无效 X-API-Key 不得静默降级为匿名——按
// 匿名继续会让误配的调用方以"数据变空"呈现、且 key 暴破与正常匿名流量
// 在响应码上不可区分。无效 key 一律 401 并进入失败限速计数。浏览器不携带
// X-API-Key，零误伤；无效 Bearer/cookie（如过期会话）保持匿名降级，不破坏
// 公开页的无凭证/过期凭证浏览语义。
func (i *AuthInterceptor) servePublic(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return handler(ctx, req)
	}
	authn := authnRequestFromMD(md)
	principal, authnErr := i.validator.Authenticate(ctx, authn)
	if authnErr != nil {
		if ct, _, parseErr := shared.ParseAuthnRequest(authn); parseErr == nil && ct == shared.CredentialTypeAPIKey {
			if ci := contexts.ClientInfoFrom(ctx); ci.IP != "" {
				if throttleErr := i.recordAPIKeyAuthFailure(ctx, ci.IP); throttleErr != nil {
					i.logAuthFailure(ctx, info.FullMethod, "apikey_auth_throttled", ct, nil)
					return nil, throttleErr
				}
			}
			i.logAuthFailure(ctx, info.FullMethod, "credential_invalid", ct, nil)
			return nil, status.Error(codes.Unauthenticated, "invalid api key")
		}
	}
	if authnErr == nil && principal != nil {
		ctx = injectPrincipals(ctx, principal)
	}
	return handler(ctx, req)
}

// finalizePrincipal 处理认证后的主体收尾（与档位无关的业务校验，保持
// 原位）：X-Torchwood-Project 单值覆盖 + ValidateAdminProjectAccess（admin
// 主体），并做双 principal 注入——torchwood contexts（下游 handler 消费）
// 与库 principal（紧随其后的库 authz 拦截器求值输入，claim 投影约定见
// domainauth.ProjectPrincipal）。
func (i *AuthInterceptor) finalizePrincipal(ctx context.Context, md metadata.MD, fullMethod string, principal *shared.Principal) (context.Context, error) {
	if principal.ActorKind == shared.ActorKindAdmin {
		// P3-4：X-Torchwood-Project 多值拒绝（fail-closed，一致性缺口修复）。
		if values := md.Get("x-torchwood-project"); len(values) > 1 {
			i.logAuthFailure(ctx, fullMethod, "multi_project_header", principal.CredentialType, principal)
			return nil, status.Error(codes.InvalidArgument, "multiple X-Torchwood-Project headers not allowed")
		} else if len(values) == 1 {
			if projectID := strings.TrimSpace(values[0]); projectID != "" {
				principal.ProjectID = projectID
			}
		}
		if err := i.validator.ValidateAdminProjectAccess(ctx, principal); err != nil {
			i.logAuthFailure(ctx, fullMethod, "admin_project_access_denied", principal.CredentialType, principal)
			return nil, err
		}
	}
	return injectPrincipals(ctx, principal), nil
}

// injectPrincipals 做双 principal 注入（torchwood + 库）。
func injectPrincipals(ctx context.Context, p *shared.Principal) context.Context {
	ctx = contexts.WithPrincipal(ctx, p)
	return principal.With(ctx, domainauth.ProjectPrincipal(p))
}

// firstMetadataValue 取 metadata 首值（去空白）；无值返回空串。
func firstMetadataValue(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
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
