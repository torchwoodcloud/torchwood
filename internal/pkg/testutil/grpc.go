package testutil

import (
	"context"

	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	"github.com/torchwoodcloud/torchwood/internal/domain/databases"
	domainfunctions "github.com/torchwoodcloud/torchwood/internal/domain/functions"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwoodcloud/torchwood/internal/infra/bun/model"
	"github.com/torchwoodcloud/torchwood/internal/infra/clients"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	MethodHealthCheck    = "/torchwood.server.v1.HealthService/Check"
	MethodListUsers      = "/torchwood.server.v1.UsersService/ListUsers"
	MethodAccountMe      = "/torchwood.client.v1.AccountService/Me"
	MethodAccountSignOut = "/torchwood.client.v1.AccountService/SignOut"
	// P2 客户端调用面与执行身份回访（验收测试用；策略与 proto 注解同构）。
	MethodInvokeFunction = "/torchwood.client.v1.FunctionsService/InvokeFunction"
	MethodAssetsGrant    = "/torchwood.server.v1.AssetsService/Grant"
	// Analytics 摄入双面（PR2；策略与 proto 注解同构：client 面 service_auth
	// 默认 END_USER → permissions 归一 ["users"]；server 面 method_auth =
	// admin_roles {member,admin,owner} + analytics:write）。
	MethodAnalyticsClientIngest = "/torchwood.client.v1.AnalyticsService/IngestEvents"
	MethodAnalyticsServerIngest = "/torchwood.server.v1.AnalyticsService/IngestEvents"
)

// InterceptorEnv wires clientInfo + auth + rate limit + audit interceptors
// the same way production does.
type InterceptorEnv struct {
	DB          *clients.Database
	Validator   *auth.Validator
	Auth        *interceptor.AuthInterceptor
	RateLimit   *interceptor.RateLimitInterceptor
	RateLimiter *FakeRateLimiter
	Audit       *interceptor.AuditInterceptor
}

func NewInterceptorEnv(db *clients.Database, cfg *config.AppConfig, docDB databases.DocumentDB) (*InterceptorEnv, error) {
	return newInterceptorEnv(db, cfg, docDB, nil)
}

// NewInterceptorEnvWithExecutionTokens 在 NewInterceptorEnv 之上把函数执行
// token 服务装配进 validator（P0 执行身份：函数容器以 `twx_` token 回访
// Server API 的验收链路需要；nil 等价 NewInterceptorEnv）。
func NewInterceptorEnvWithExecutionTokens(db *clients.Database, cfg *config.AppConfig, docDB databases.DocumentDB, execTokens domainfunctions.ExecutionTokenService) (*InterceptorEnv, error) {
	return newInterceptorEnv(db, cfg, docDB, execTokens)
}

func newInterceptorEnv(db *clients.Database, cfg *config.AppConfig, docDB databases.DocumentDB, execTokens domainfunctions.ExecutionTokenService) (*InterceptorEnv, error) {
	validator := auth.NewValidatorWithOneTimeTokens(
		cfg,
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
		nil,
		execTokens,
	)
	// 小策略注册表（与生产 BuildMethodPolicies 同构的 PolicySet 注入；
	// 全量策略语义由 runtime AssertSemantic + 矩阵测试把关）。
	policies, err := domainauth.NewPolicySet([]domainauth.MethodPolicy{
		{Method: MethodHealthCheck, Service: "/torchwood.server.v1.HealthService", Access: domainauth.AccessPublic},
		{Method: MethodListUsers, Service: "/torchwood.server.v1.UsersService", Access: domainauth.AccessServer,
			Scope: &domainauth.ScopeRule{Resource: domainauth.ScopeUsers, Op: domainauth.ScopeRead}},
		{Method: MethodAccountMe, Service: "/torchwood.client.v1.AccountService", Access: domainauth.AccessEndUser, Permissions: []string{"users"}},
		{Method: MethodAccountSignOut, Service: "/torchwood.client.v1.AccountService", Access: domainauth.AccessEndUser, Permissions: []string{"users"}},
		// P2 客户端调用面（service_auth default_access END_USER，无 method_auth
		// → permissions 归一 ["users"]）与执行身份 assets:write scope 门。
		{Method: MethodInvokeFunction, Service: "/torchwood.client.v1.FunctionsService", Access: domainauth.AccessEndUser, Permissions: []string{"users"}},
		{Method: MethodAssetsGrant, Service: "/torchwood.server.v1.AssetsService", Access: domainauth.AccessServer,
			Scope: &domainauth.ScopeRule{Resource: domainauth.ScopeAssets, Op: domainauth.ScopeWrite}},
		// Analytics 摄入双面（PR2）。
		{Method: MethodAnalyticsClientIngest, Service: "/torchwood.client.v1.AnalyticsService", Access: domainauth.AccessEndUser, Permissions: []string{"users"}},
		{Method: MethodAnalyticsServerIngest, Service: "/torchwood.server.v1.AnalyticsService", Access: domainauth.AccessServer,
			AdminRoles: []domainauth.AdminRole{domainauth.AdminRoleMember, domainauth.AdminRoleAdmin, domainauth.AdminRoleOwner},
			Scope:      &domainauth.ScopeRule{Resource: domainauth.ScopeAnalytics, Op: domainauth.ScopeWrite}},
	})
	if err != nil {
		return nil, err
	}
	authIC, err := interceptor.NewAuthInterceptor(validator, policies)
	if err != nil {
		return nil, err
	}
	rateLimiter := &FakeRateLimiter{}
	return &InterceptorEnv{
		DB:          db,
		Validator:   validator,
		Auth:        authIC,
		RateLimit:   interceptor.NewRateLimitInterceptor(rateLimiter, cfg),
		RateLimiter: rateLimiter,
		Audit:       interceptor.NewAuditInterceptor(bunrepo.NewAuditRepository(db)),
	}, nil
}

// InvokeUnary runs clientInfo -> auth -> rate limit -> audit -> handler for
// the given gRPC method and metadata (production chain order).
func (e *InterceptorEnv) InvokeUnary(ctx context.Context, method string, md metadata.MD) error {
	return e.InvokeUnaryHandler(ctx, method, md, func(context.Context, any) (any, error) { return nil, nil })
}

// InvokeUnaryHandler 以自定义 handler 跑同一拦截器链（验收测试把真实 use-case
// handler 挂进链路，验证凭证解析 → 策略门 → 业务语义的端到端组合）。
func (e *InterceptorEnv) InvokeUnaryHandler(ctx context.Context, method string, md metadata.MD, handler func(ctx context.Context, req any) (any, error)) error {
	ctx = metadata.NewIncomingContext(ctx, md)
	info := &grpc.UnaryServerInfo{FullMethod: method}
	auditHandler := func(ctx context.Context, req any) (any, error) {
		return e.Audit.UnaryAuditMiddleware(ctx, req, info, handler)
	}
	rateLimitHandler := func(ctx context.Context, req any) (any, error) {
		return e.RateLimit.UnaryRateLimitMiddleware(ctx, req, info, auditHandler)
	}
	authHandler := func(ctx context.Context, req any) (any, error) {
		return e.Auth.UnaryAuthMiddleware(ctx, req, info, rateLimitHandler)
	}
	clientInfo := interceptor.NewClientInfoInterceptor(nil)
	_, err := clientInfo.UnaryMiddleware(ctx, nil, info, authHandler)
	return err
}

func (e *InterceptorEnv) AuditLogCount(ctx context.Context) (int, error) {
	return e.DB.NewSelect().Model((*model.AuditLog)(nil)).Count(ctx)
}

func (e *InterceptorEnv) LatestAuditLog(ctx context.Context) (*model.AuditLog, error) {
	row := new(model.AuditLog)
	err := e.DB.NewSelect().Model(row).Order("created_at DESC").Limit(1).Scan(ctx)
	if err != nil {
		return nil, err
	}
	return row, nil
}
