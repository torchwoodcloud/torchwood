package runtime

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lynx-go/lynx"
	lynxgrpc "github.com/lynx-go/lynx/server/grpc"
	clientv1 "github.com/torchwooddev/torchwood/genproto/client/v1"
	consolev1 "github.com/torchwooddev/torchwood/genproto/console/v1"
	serverv1 "github.com/torchwooddev/torchwood/genproto/server/v1"
	sharedv1 "github.com/torchwooddev/torchwood/genproto/shared/v1"
	"github.com/torchwooddev/torchwood/internal/api/clientgrpc"
	"github.com/torchwooddev/torchwood/internal/api/consolegrpc"
	"github.com/torchwooddev/torchwood/internal/api/interceptor"
	"github.com/torchwooddev/torchwood/internal/api/servergrpc"
	"github.com/torchwooddev/torchwood/internal/domain/audit"
	domainauth "github.com/torchwooddev/torchwood/internal/domain/auth"
	domainbilling "github.com/torchwooddev/torchwood/internal/domain/billing"
	"github.com/torchwooddev/torchwood/internal/infra/auth"
	"github.com/torchwooddev/torchwood/internal/infra/health"
	"github.com/torchwooddev/torchwood/internal/pkg/config"
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
) (*lynxgrpc.Server, error) {
	grpcCfg := cfg.GetServer().GetGrpc()
	timeout := parseDuration(grpcCfg.GetTimeout(), 30*time.Second)

	publicMethods, apiKeyMethods, permissionMethods, err := collectMethodsByAccess(authzFileDescriptors()...)
	if err != nil {
		return nil, err
	}
	// 语义断言（机制重设计 M2）：完备性/死 scope/档位/client·console 值域/
	// 项目寻址不变量/streaming fail-closed——策略语义违例启动即失败。
	policySet, err := BuildMethodPolicies(authzFileDescriptors()...)
	if err != nil {
		return nil, err
	}
	if err := domainauth.AssertSemantic(policySet); err != nil {
		return nil, err
	}
	// 过渡期交叉核验（A2 拦截器换源 PolicySet 后两断言随两表退役）：
	// proto 推导的 SERVER 方法集合必须与 apiKeyScopeRules 完全一致，
	// scope 写方法必须已登记 adminRoleMethodRules。
	interceptor.AssertAPIKeyScopeCoverage(apiKeyMethods)
	interceptor.AssertAdminRoleWriteCoverage()

	authInterceptor, err := interceptor.NewAuthInterceptor(validator, publicMethods, apiKeyMethods, permissionMethods)
	if err != nil {
		return nil, err
	}
	authInterceptor = authInterceptor.WithLogger(app.Logger())
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
	if outboxService != nil {
		serverv1.RegisterOutboxServiceServer(grpcSrv, outboxService)
	}

	// fail-closed：所有已注册方法都必须带有 authz 注解，缺失的方法会在拦截器里被放行。
	if err := assertRegisteredMethodsHaveAuthz(grpcSrv, publicMethods, apiKeyMethods, permissionMethods); err != nil {
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

// assertRegisteredMethodsHaveAuthz 断言每个已注册的 gRPC 方法都存在于某一类 access map
// （public/apiKey/permission 之一）。漏配的方法不在任何 map 中，会在拦截器
// "len(perms)==0 跳过"分支被任意有效凭证放行，因此启动期直接报错（fail-closed）。
func assertRegisteredMethodsHaveAuthz(grpcSrv *grpc.Server, publicMethods, apiKeyMethods []string, permissionMethods map[string][]string) error {
	covered := make(map[string]struct{}, len(publicMethods)+len(apiKeyMethods)+len(permissionMethods))
	for _, m := range publicMethods {
		covered[m] = struct{}{}
	}
	for _, m := range apiKeyMethods {
		covered[m] = struct{}{}
	}
	for m := range permissionMethods {
		covered[m] = struct{}{}
	}

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
			if _, ok := covered[fullMethod]; !ok {
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
		consolev1.File_console_v1_auth_proto,
		consolev1.File_console_v1_admins_proto,
	}
}

// collectMethodsByAccess 从 PolicySet 派生拦截器所需的三张方法集合
// （A2 拦截器换源后本函数退役）。END_USER/PERMISSION 都落在 permissionMethods；
// SERVER 落在 apiKeyMethods（历史命名，A2 更名 serverMethods）。
func collectMethodsByAccess(fileDescs ...protoreflect.FileDescriptor) (publicMethods []string, apiKeyMethods []string, permissionMethods map[string][]string, err error) {
	set, err := BuildMethodPolicies(fileDescs...)
	if err != nil {
		return nil, nil, nil, err
	}
	permissionMethods = make(map[string][]string)
	for _, p := range set.Methods() {
		switch p.Access {
		case domainauth.AccessPublic:
			publicMethods = append(publicMethods, p.Method)
		case domainauth.AccessServer:
			apiKeyMethods = append(apiKeyMethods, p.Method)
		case domainauth.AccessEndUser, domainauth.AccessPermission:
			permissionMethods[p.Method] = p.Permissions
		default:
			return nil, nil, nil, fmt.Errorf("method %s: access %d 无 bucket", p.Method, p.Access)
		}
	}
	return publicMethods, apiKeyMethods, permissionMethods, nil
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
