package testutil

import (
	"context"

	"github.com/torchwooddev/torchwood/internal/domain/databases"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/bun/bunrepo"
	"github.com/torchwooddev/torchwood/internal/infra/bun/model"
	"github.com/torchwooddev/torchwood/internal/infra/clients"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

const (
	MethodHealthCheck    = "/torchwood.server.v1.HealthService/Check"
	MethodListUsers      = "/torchwood.server.v1.UsersService/ListUsers"
	MethodAccountMe      = "/torchwood.client.v1.AccountService/Me"
	MethodAccountSignOut = "/torchwood.client.v1.AccountService/SignOut"
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
	validator := auth.NewValidator(
		cfg,
		bunrepo.NewAPIKeyRepository(db),
		nil,
		bunrepo.NewAdminRepository(db),
		bunrepo.NewAdminProjectRepository(db),
		nil,
		bunrepo.NewSessionRepository(db),
		bunrepo.NewUserRepository(db),
		nil,
	)
	authIC, err := interceptor.NewAuthInterceptor(
		validator,
		[]string{MethodHealthCheck},
		[]string{MethodListUsers},
		map[string][]string{
			MethodAccountMe:      {"users"},
			MethodAccountSignOut: {"users"},
		},
	)
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
	ctx = metadata.NewIncomingContext(ctx, md)
	info := &grpc.UnaryServerInfo{FullMethod: method}
	handler := func(ctx context.Context, req any) (any, error) { return nil, nil }
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
