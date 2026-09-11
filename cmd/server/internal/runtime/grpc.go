package runtime

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lynx-go/lynx"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	clientv1 "github.com/torchwoodcloud/torchwood/genproto/client/v1"
	consolev1 "github.com/torchwoodcloud/torchwood/genproto/console/v1"
	serverv1 "github.com/torchwoodcloud/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwoodcloud/torchwood/genproto/shared/v1"
	"github.com/torchwoodcloud/torchwood/internal/api/clientgrpc"
	"github.com/torchwoodcloud/torchwood/internal/api/consolegrpc"
	"github.com/torchwoodcloud/torchwood/internal/api/interceptor"
	"github.com/torchwoodcloud/torchwood/internal/api/servergrpc"
	"github.com/torchwoodcloud/torchwood/internal/domain/audit"
	domainauth "github.com/torchwoodcloud/torchwood/internal/domain/auth"
	domainbilling "github.com/torchwoodcloud/torchwood/internal/domain/billing"
	"github.com/torchwoodcloud/torchwood/internal/infra/auth"
	"github.com/torchwoodcloud/torchwood/internal/infra/health"
	"github.com/torchwoodcloud/torchwood/internal/pkg/config"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

func NewGRPCServer(
	app lynx.App,
	cfg *config.AppConfig,
	validator *auth.Validator,
	auditRepo audit.Repository,
	rateLimiter domainauth.RateLimiter,
	checkers *health.Checkers,
	account *clientgrpc.AccountService,
	clientDatabases *clientgrpc.DatabasesService,
	clientGroups *clientgrpc.GroupsService,
	clientPayments *clientgrpc.PaymentsService,
	clientAssets *clientgrpc.AssetsService,
	clientSubscriptions *clientgrpc.SubscriptionsService,
	clientFunctions *clientgrpc.FunctionsService,
	clientLeaderboards *clientgrpc.LeaderboardsService,
	health *servergrpc.HealthService,
	projects *servergrpc.ProjectsService,
	storage *servergrpc.StorageService,
	users *servergrpc.UsersService,
	apiKeys *servergrpc.APIKeysService,
	oauthProviders *servergrpc.OAuthProvidersService,
	groups *servergrpc.GroupsService,
	databases *servergrpc.DatabasesService,
	functions *servergrpc.FunctionsService,
	serverPayments *servergrpc.PaymentsService,
	serverAssets *servergrpc.AssetsService,
	serverSubscriptions *servergrpc.SubscriptionsService,
	billingService *servergrpc.BillingService,
	usageMeter domainbilling.UsageCounter,
	consoleAuth *consolegrpc.AuthService,
	adminsService *consolegrpc.AdminsService,
	outboxService *servergrpc.OutboxService,
	auditLogsService *servergrpc.AuditLogsService,
	serverLeaderboards *servergrpc.LeaderboardsService,
	consoleLeaderboards *consolegrpc.LeaderboardsService,
	policySet *domainauth.PolicySet,
) (*lynxgrpc.Server, error) {
	grpcCfg := cfg.GetServer().GetGrpc()
	timeout := parseDuration(grpcCfg.GetTimeout(), 30*time.Second)

	// 策略注册表由 ProvideMethodPolicies 注入（唯一收集点，含语义断言）。
	authInterceptor, err := interceptor.NewAuthInterceptor(validator, policySet)
	if err != nil {
		return nil, err
	}
	authInterceptor = authInterceptor.WithLogger(app.Logger())
	// M5 C6：auth 拒绝并联审计（拦截器层拒绝到不了 audit 中间件，无双写）。
	authInterceptor = authInterceptor.WithDenyAuditSink(auditRepo)
	// T-02：X-API-Key 认证失败按来源 IP 频控（security.login_throttle
	// .api_key_auth，默认 10 次/60s；未配置维度时 WithAPIKeyFailThrottle
	// 回落内置默认）。
	authInterceptor = authInterceptor.WithAPIKeyFailThrottle(
		rateLimiter,
		int(cfg.GetSecurity().GetLoginThrottle().GetApiKeyAuth().GetLimit()),
		parseDuration(cfg.GetSecurity().GetLoginThrottle().GetApiKeyAuth().GetWindow(), 0),
	)
	trustedProxies, err := interceptor.ParseTrustedProxies(cfg.GetSecurity().GetTrustedProxies())
	if err != nil {
		return nil, fmt.Errorf("parse security.trusted_proxies: %w", err)
	}
	auditInterceptor := interceptor.NewAuditInterceptor(auditRepo).WithLogger(app.Logger()).WithTrustedProxies(trustedProxies)
	clientInfoInterceptor := interceptor.NewClientInfoInterceptor(trustedProxies)
	// 通用 API 限流（roadmap §3.4）：挂在 clientInfo 与 auth 之后（需要
	// trusted-proxy 校验后的 IP 与 principal）、audit 之前；复用
	// domainauth.RateLimiter 端口的 Redis 固定窗口实现。
	rateLimitInterceptor := interceptor.NewRateLimitInterceptor(rateLimiter, cfg)
	usageInterceptor := interceptor.NewUsageInterceptor(usageMeter).WithLogger(app.Logger())
	// 形状校验（protovalidate 注解，09-api-guide §1.6）：插在链尾——
	// audit/usage 之后、handler 之前。校验失败请求与手写校验时期行为
	// 完全一致（InvalidArgument 审计行照常落库、用量照常计数），只是把
	// handler 开头的形状检查外提为 proto 声明。
	validateInterceptor, err := interceptor.NewValidateInterceptor()
	if err != nil {
		return nil, err
	}

	srv := lynxgrpc.NewServer(
		lynxgrpc.WithAddr(grpcCfg.GetAddr()),
		lynxgrpc.WithTimeout(timeout),
		lynxgrpc.WithLogger(app.Logger()),
		// 轮询 checkers 并同步 grpc.health.v1.Health（10s 周期快照）。
		lynxgrpc.WithHealthCheckers(func() []lynx.Checker { return checkers.Deps() }),
		lynxgrpc.WithInterceptors(
			clientInfoInterceptor.UnaryMiddleware,
			authInterceptor.UnaryAuthMiddleware,
			rateLimitInterceptor.UnaryRateLimitMiddleware,
			auditInterceptor.UnaryAuditMiddleware,
			usageInterceptor.UnaryUsageMiddleware,
			validateInterceptor.UnaryValidateMiddleware,
		),
		// 允许 ≤1MiB 的 deployment 代码包走 gRPC（base64 膨胀后约 1.33x）。
		lynxgrpc.WithServerOptions(grpc.MaxRecvMsgSize(8<<20)),
	)
	grpcSrv := srv.GetServer()

	clientv1.RegisterAccountServiceServer(grpcSrv, account)
	clientv1.RegisterDatabasesServiceServer(grpcSrv, clientDatabases)
	clientv1.RegisterGroupsServiceServer(grpcSrv, clientGroups)
	clientv1.RegisterPaymentsServiceServer(grpcSrv, clientPayments)
	clientv1.RegisterAssetsServiceServer(grpcSrv, clientAssets)
	clientv1.RegisterSubscriptionsServiceServer(grpcSrv, clientSubscriptions)
	clientv1.RegisterFunctionsServiceServer(grpcSrv, clientFunctions)
	clientv1.RegisterLeaderboardsServiceServer(grpcSrv, clientLeaderboards)
	serverv1.RegisterHealthServiceServer(grpcSrv, health)
	serverv1.RegisterProjectsServiceServer(grpcSrv, projects)
	serverv1.RegisterStorageServiceServer(grpcSrv, storage)
	serverv1.RegisterUsersServiceServer(grpcSrv, users)
	serverv1.RegisterAPIKeysServiceServer(grpcSrv, apiKeys)
	serverv1.RegisterOAuthProvidersServiceServer(grpcSrv, oauthProviders)
	serverv1.RegisterGroupsServiceServer(grpcSrv, groups)
	serverv1.RegisterDatabasesServiceServer(grpcSrv, databases)
	serverv1.RegisterFunctionsServiceServer(grpcSrv, functions)
	serverv1.RegisterPaymentsServiceServer(grpcSrv, serverPayments)
	serverv1.RegisterAssetsServiceServer(grpcSrv, serverAssets)
	serverv1.RegisterSubscriptionsServiceServer(grpcSrv, serverSubscriptions)
	serverv1.RegisterBillingServiceServer(grpcSrv, billingService)
	consolev1.RegisterConsoleAuthServiceServer(grpcSrv, consoleAuth)
	consolev1.RegisterAdminsServiceServer(grpcSrv, adminsService)
	consolev1.RegisterLeaderboardsServiceServer(grpcSrv, consoleLeaderboards)
	if outboxService != nil {
		serverv1.RegisterOutboxServiceServer(grpcSrv, outboxService)
	}
	serverv1.RegisterAuditLogsServiceServer(grpcSrv, auditLogsService)
	serverv1.RegisterLeaderboardsServiceServer(grpcSrv, serverLeaderboards)

	// fail-closed：所有已注册方法都必须带有 authz 注解，缺失的方法会在拦截器里被放行。
	if err := assertRegisteredMethodsHaveAuthz(grpcSrv, policySet); err != nil {
		return nil, err
	}

	return srv, nil
}

// authzExemptServicePrefixes 是不参与业务 authz 注解校验的 gRPC 框架内置服务白名单：
// grpc.health.v1.Health 由 lynx 注册用于健康检查，grpc.reflection.* 用于 server reflection，
// 它们不是业务 API、不携带业务 authz 注解，由部署层网络策略保护。
var authzExemptServicePrefixes = []string{
	"grpc.health.v1.",
	"grpc.reflection.",
}

// assertRegisteredMethodsHaveAuthz 断言每个已注册的 gRPC 方法都在策略注册
// 表中。漏配的方法会被拦截器 fail-closed 拒绝（policy_missing），启动期
// 直接报错以尽早暴露注解缺失。
func assertRegisteredMethodsHaveAuthz(grpcSrv *grpc.Server, policies *domainauth.PolicySet) error {
	var missing []string
	for serviceName, info := range grpcSrv.GetServiceInfo() {
		exempt := false
		for _, prefix := range authzExemptServicePrefixes {
			if strings.HasPrefix(serviceName, prefix) {
				exempt = true
				break
			}
		}
		if exempt {
			continue
		}
		for _, m := range info.Methods {
			fullMethod := "/" + serviceName + "/" + m.Name
			if _, ok := policies.Get(fullMethod); !ok {
				missing = append(missing, fullMethod)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("registered grpc methods missing authz annotation: %s", strings.Join(missing, ", "))
	}
	return nil
}

// authzFileDescriptors 是 authz 推导（collectMethodsByAccess）与 swagger 一致性
// 测试共用的业务 proto 文件清单（单一事实源；新增服务文件只登记此处）。
func authzFileDescriptors() []protoreflect.FileDescriptor {
	return []protoreflect.FileDescriptor{
		clientv1.File_client_v1_account_proto,
		clientv1.File_client_v1_databases_proto,
		clientv1.File_client_v1_groups_proto,
		clientv1.File_client_v1_payments_proto,
		clientv1.File_client_v1_assets_proto,
		clientv1.File_client_v1_subscriptions_proto,
		clientv1.File_client_v1_functions_proto,
		clientv1.File_client_v1_leaderboards_proto,
		serverv1.File_server_v1_projects_proto,
		serverv1.File_server_v1_health_proto,
		serverv1.File_server_v1_storage_proto,
		serverv1.File_server_v1_users_proto,
		serverv1.File_server_v1_apikeys_proto,
		serverv1.File_server_v1_oauth_providers_proto,
		serverv1.File_server_v1_groups_proto,
		serverv1.File_server_v1_databases_proto,
		serverv1.File_server_v1_functions_proto,
		serverv1.File_server_v1_payments_proto,
		serverv1.File_server_v1_assets_proto,
		serverv1.File_server_v1_subscriptions_proto,
		serverv1.File_server_v1_billing_proto,
		serverv1.File_server_v1_outbox_proto,
		serverv1.File_server_v1_audit_logs_proto,
		serverv1.File_server_v1_leaderboards_proto,
		consolev1.File_console_v1_auth_proto,
		consolev1.File_console_v1_admins_proto,
		consolev1.File_console_v1_leaderboards_proto,
	}
}

func resolveServiceDefaultAccess(service protoreflect.ServiceDescriptor) sharedv1.AccessLevel {
	options, ok := service.Options().(*descriptorpb.ServiceOptions)
	if !ok || options == nil || !proto.HasExtension(options, sharedv1.E_ServiceAuth) {
		return sharedv1.AccessLevel_ACCESS_LEVEL_UNSPECIFIED
	}
	ext := proto.GetExtension(options, sharedv1.E_ServiceAuth)
	policy, ok := ext.(*sharedv1.ServiceAuth)
	if !ok {
		return sharedv1.AccessLevel_ACCESS_LEVEL_UNSPECIFIED
	}
	return policy.GetDefaultAccess()
}

// resolveMethodAccess 已由 BuildMethodPolicies/buildMethodPolicy 取代
// （策略解析单一实现）；保留 resolveServiceDefaultAccess 供 swagger 一致性
// 测试推导服务默认 access。

func parseDuration(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
